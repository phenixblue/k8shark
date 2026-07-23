// Package query evaluates a JSONPath expression against every captured
// object in an archive, across all resource types and namespaces, at a
// chosen point in time.
package query

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/server"
	"k8s.io/client-go/util/jsonpath"
)

// Options configures a Run.
type Options struct {
	// Expression is a kubectl-style JSONPath template, e.g. "{.spec.containers[*].image}".
	Expression string
	// At selects the snapshot to query. Zero means the latest captured state.
	At time.Time
	// Resource limits the query to one resource type, e.g. "pods". Empty means all.
	Resource string
	// Namespace limits the query to one namespace. Empty means all.
	Namespace string
}

// Match is one JSONPath result found on one captured object.
type Match struct {
	Path      string          `json:"path"`
	Group     string          `json:"group,omitempty"`
	Version   string          `json:"version,omitempty"`
	Resource  string          `json:"resource,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Name      string          `json:"name"`
	Value     json.RawMessage `json:"value"`
}

// Result is the full set of matches for one query.
type Result struct {
	Matches []Match `json:"matches"`
}

// Run evaluates opts.Expression against every captured object in store at
// the resolved snapshot, returning one Match per non-empty JSONPath result.
// Objects that don't have the queried field are skipped, not treated as errors.
func Run(store *server.CaptureStore, opts Options) (*Result, error) {
	jp := jsonpath.New("query").AllowMissingKeys(true)
	if err := jp.Parse(opts.Expression); err != nil {
		return nil, fmt.Errorf("parsing jsonpath expression %q: %w", opts.Expression, err)
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

	matches := make([]Match, 0)
	for _, path := range paths {
		if strings.Contains(path, "?") {
			continue // Table/query-param variants of a path already covered plainly
		}
		g, v, r, ns := parseAPIPath(path)
		if r == "" {
			continue // discovery/openapi/other non-resource paths
		}
		if opts.Resource != "" && r != opts.Resource {
			continue
		}
		if opts.Namespace != "" && ns != opts.Namespace {
			continue
		}
		body, code, err := store.ReconstructAt(path, at)
		if err != nil || code != 200 || len(body) == 0 {
			continue
		}
		for _, item := range extractItems(body) {
			var data any
			if json.Unmarshal(item, &data) != nil {
				continue
			}
			results, err := jp.FindResults(data)
			if err != nil {
				continue
			}
			name := itemName(item)
			for _, set := range results {
				for _, rv := range set {
					value, ok := marshalValue(rv)
					if !ok {
						continue
					}
					matches = append(matches, Match{
						Path: path, Group: g, Version: v, Resource: r,
						Namespace: ns, Name: name, Value: value,
					})
				}
			}
		}
	}
	return &Result{Matches: matches}, nil
}

// extractItems returns the objects to query in body: a list's items, or the
// body itself when it isn't list-shaped.
func extractItems(body []byte) []json.RawMessage {
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.Items == nil {
		return []json.RawMessage{body}
	}
	return list.Items
}

func itemName(item json.RawMessage) string {
	var meta struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if json.Unmarshal(item, &meta) != nil {
		return ""
	}
	return meta.Metadata.Name
}

func marshalValue(rv reflect.Value) (json.RawMessage, bool) {
	if !rv.IsValid() {
		return nil, false
	}
	b, err := json.Marshal(rv.Interface())
	if err != nil {
		return nil, false
	}
	return json.RawMessage(b), true
}

// parseAPIPath extracts group, version, resource, and namespace from a canonical
// API list path. Cluster-scoped paths return an empty namespace.
func parseAPIPath(path string) (group, version, resource, namespace string) {
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	switch {
	case len(parts) >= 3 && parts[0] == "api": // /api/v1/...
		version = parts[1]
		rest := parts[2:]
		if len(rest) >= 3 && rest[0] == "namespaces" {
			namespace = rest[1]
			resource = rest[2]
		} else if len(rest) >= 1 {
			resource = rest[0]
		}
	case len(parts) >= 4 && parts[0] == "apis": // /apis/<group>/<version>/...
		group, version = parts[1], parts[2]
		rest := parts[3:]
		if len(rest) >= 3 && rest[0] == "namespaces" {
			namespace = rest[1]
			resource = rest[2]
		} else if len(rest) >= 1 {
			resource = rest[0]
		}
	}
	return
}
