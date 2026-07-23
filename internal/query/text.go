package query

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phenixblue/k8shark/internal/server"
)

// TextOptions configures a SearchText.
type TextOptions struct {
	// Pattern is the substring (default) or regular expression to search for.
	Pattern string
	// Regex treats Pattern as a Go regular expression instead of a plain substring.
	Regex bool
	// At selects the snapshot to search. Zero means the latest captured state.
	At time.Time
	// Resource limits the search to one resource type, e.g. "pods". Empty means all.
	Resource string
	// Namespace limits the search to one namespace. Empty means all.
	Namespace string
}

// TextMatch is one substring/regex match found in a captured object body or
// pod log.
type TextMatch struct {
	Path      string `json:"path"`
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Resource  string `json:"resource,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Field is the dotted JSON field path of the matched string, e.g.
	// "metadata.annotations.note" or "spec.containers[0].image". Empty for log matches.
	Field string `json:"field,omitempty"`
	// Container and Previous are set only for matches found in a captured pod log.
	Container string `json:"container,omitempty"`
	Previous  bool   `json:"previous,omitempty"`
	// Snippet is the matched text with surrounding context.
	Snippet string `json:"snippet"`
}

// TextResult is the full set of matches for one full-text search.
type TextResult struct {
	Matches []TextMatch `json:"matches"`
}

// SearchText finds opts.Pattern across every captured object body and pod
// log in store at the resolved snapshot.
func SearchText(store *server.CaptureStore, opts TextOptions) (*TextResult, error) {
	find, err := newFinder(opts.Pattern, opts.Regex)
	if err != nil {
		return nil, err
	}

	at := opts.At
	if at.IsZero() {
		at = store.Metadata.CapturedUntil
	}

	paths := make([]string, 0, len(store.Index))
	for path := range store.Index {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	matches := make([]TextMatch, 0)
	for _, path := range paths {
		if isLogPath(path) {
			m, ok := searchLogPath(store, path, at, find, opts)
			if ok {
				matches = append(matches, m...)
			}
			continue
		}
		if isDuplicateView(path) {
			continue // ?as=Table / ?as=TableSchema alternate representations of a path already covered plainly
		}
		g, v, r, pathNS := parseAPIPath(path)
		if r == "" {
			continue // discovery/openapi/other non-resource paths
		}
		if opts.Resource != "" && r != opts.Resource {
			continue
		}
		body, code, err := store.ReconstructAt(path, at)
		if err != nil || code != 200 || len(body) == 0 {
			continue
		}
		for _, item := range extractItems(body) {
			meta := itemMeta(item, pathNS)
			if opts.Namespace != "" && meta.Namespace != opts.Namespace {
				continue
			}
			var data any
			if json.Unmarshal(item, &data) != nil {
				continue
			}
			walkStrings(data, "", func(field, s string) {
				start, end, ok := find(s)
				if !ok {
					return
				}
				matches = append(matches, TextMatch{
					Path: path, Group: g, Version: v, Resource: r,
					Namespace: meta.Namespace, Name: meta.Name,
					Field: field, Snippet: snippet(s, start, end),
				})
			})
		}
	}
	return &TextResult{Matches: matches}, nil
}

func searchLogPath(store *server.CaptureStore, path string, at time.Time, find finder, opts TextOptions) ([]TextMatch, bool) {
	ns, name, container, previous, ok := parseLogPath(path)
	if !ok {
		return nil, false
	}
	if opts.Namespace != "" && ns != opts.Namespace {
		return nil, false
	}
	if opts.Resource != "" && opts.Resource != "pods" {
		return nil, false
	}
	body, code, err := store.ReconstructAt(path, at)
	if err != nil || code != 200 || len(body) == 0 {
		return nil, false
	}
	var logText string
	if json.Unmarshal(body, &logText) != nil {
		return nil, false
	}

	matches := make([]TextMatch, 0)
	for _, line := range strings.Split(logText, "\n") {
		start, end, ok := find(line)
		if !ok {
			continue
		}
		matches = append(matches, TextMatch{
			Path: path, Resource: "pods", Namespace: ns, Name: name,
			Container: container, Previous: previous,
			Snippet: snippet(line, start, end),
		})
	}
	return matches, true
}

// isLogPath reports whether path is a captured pod-log endpoint
// (/api/v1/namespaces/<ns>/pods/<name>/log[?container=<c>[&previous=true]]).
func isLogPath(path string) bool {
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return strings.HasSuffix(p, "/log")
}

// parseLogPath extracts the namespace, pod name, container, and previous-log
// flag from a captured pod-log index key.
func parseLogPath(path string) (namespace, name, container string, previous, ok bool) {
	base, query := path, ""
	if i := strings.IndexByte(path, '?'); i >= 0 {
		base, query = path[:i], path[i+1:]
	}
	parts := strings.Split(strings.TrimPrefix(base, "/"), "/")
	if len(parts) != 7 || parts[0] != "api" || parts[1] != "v1" || parts[2] != "namespaces" ||
		parts[4] != "pods" || parts[6] != "log" {
		return "", "", "", false, false
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", "", "", false, false
	}
	return parts[3], parts[5], values.Get("container"), values.Get("previous") == "true", true
}

// walkStrings visits every string leaf in v (a JSON-decoded object graph),
// calling fn with its dotted field path and value. Map keys are visited in
// sorted order for deterministic results.
func walkStrings(v any, path string, fn func(field, s string)) {
	switch t := v.(type) {
	case string:
		fn(path, t)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkStrings(t[k], joinField(path, k), fn)
		}
	case []any:
		for i, vv := range t {
			walkStrings(vv, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	}
}

func joinField(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// finder reports the [start, end) byte range of the first match in s.
type finder func(s string) (start, end int, ok bool)

func newFinder(pattern string, isRegex bool) (finder, error) {
	if pattern == "" {
		return nil, fmt.Errorf("search pattern must not be empty")
	}
	if isRegex {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compiling regex %q: %w", pattern, err)
		}
		return func(s string) (int, int, bool) {
			loc := re.FindStringIndex(s)
			if loc == nil {
				return 0, 0, false
			}
			return loc[0], loc[1], true
		}, nil
	}
	return func(s string) (int, int, bool) {
		idx := strings.Index(s, pattern)
		if idx < 0 {
			return 0, 0, false
		}
		return idx, idx + len(pattern), true
	}, nil
}

// snippetRadius is how many bytes of context to keep on either side of a
// match when the matched string is long. The actual cut point is snapped
// outward to the nearest UTF-8 rune boundary (see snippet), so this is
// approximate rather than an exact byte or character count.
const snippetRadius = 40

// snippet returns s trimmed to roughly snippetRadius bytes of context around
// [start, end) (byte offsets, as produced by strings.Index/regexp), with
// ellipses marking trimmed content. Cut points are snapped outward to the
// nearest UTF-8 rune boundary so a multi-byte character is never split.
func snippet(s string, start, end int) string {
	from := start - snippetRadius
	if from < 0 {
		from = 0
	}
	for from > 0 && !utf8.RuneStart(s[from]) {
		from--
	}
	to := end + snippetRadius
	if to > len(s) {
		to = len(s)
	}
	for to < len(s) && !utf8.RuneStart(s[to]) {
		to++
	}
	out := s[from:to]
	if from > 0 {
		out = "…" + out
	}
	if to < len(s) {
		out += "…"
	}
	return out
}
