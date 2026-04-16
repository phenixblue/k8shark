package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	archivepkg "github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
	"github.com/pmezard/go-difflib/difflib"
)

type Options struct {
	BeforeArchive string
	AfterArchive  string
	Archive       string
	BeforeAt      string
	AfterAt       string
	Resource      string
	Namespace     string
}

type Result struct {
	Changes []Change `json:"changes"`
}

type Change struct {
	Path      string          `json:"path"`
	Group     string          `json:"group,omitempty"`
	Version   string          `json:"version,omitempty"`
	Resource  string          `json:"resource,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
}

func Run(opts Options) (*Result, error) {
	before, after, err := loadSnapshots(opts)
	if err != nil {
		return nil, err
	}
	defer before.cleanup()
	if after.ar != before.ar {
		defer after.cleanup()
	}

	pathSet := make(map[string]struct{}, len(before.snapshot)+len(after.snapshot))
	for path := range before.snapshot {
		pathSet[path] = struct{}{}
	}
	for path := range after.snapshot {
		pathSet[path] = struct{}{}
	}

	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		if !matchesFilters(path, opts.Resource, opts.Namespace) {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)

	changes := make([]Change, 0)
	for _, path := range paths {
		beforeBody := before.snapshot[path]
		afterBody := after.snapshot[path]
		if jsonEqual(beforeBody, afterBody) {
			continue
		}
		g, v, r, ns := parseAPIPath(path)
		changes = append(changes, Change{
			Path:      path,
			Group:     g,
			Version:   v,
			Resource:  r,
			Namespace: ns,
			Before:    beforeBody,
			After:     afterBody,
		})
	}

	return &Result{Changes: changes}, nil
}

func RenderText(result *Result, color bool) (string, error) {
	if len(result.Changes) == 0 {
		return "", nil
	}

	var out strings.Builder
	for i, change := range result.Changes {
		beforePretty := prettyJSON(change.Before)
		afterPretty := prettyJSON(change.After)
		text, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A:        difflib.SplitLines(beforePretty),
			B:        difflib.SplitLines(afterPretty),
			FromFile: "before" + change.Path,
			ToFile:   "after" + change.Path,
			Context:  3,
		})
		if err != nil {
			return "", err
		}
		if i > 0 {
			out.WriteString("\n")
		}
		out.WriteString("Path: ")
		out.WriteString(change.Path)
		out.WriteString("\n")
		if color {
			out.WriteString(colorizeDiff(text))
		} else {
			out.WriteString(text)
		}
		if !strings.HasSuffix(text, "\n") {
			out.WriteString("\n")
		}
	}
	return out.String(), nil
}

type archiveSnapshot struct {
	ar       *archivepkg.Archive
	meta     capture.CaptureMetadata
	snapshot map[string]json.RawMessage
}

func (s *archiveSnapshot) cleanup() {
	if s == nil || s.ar == nil {
		return
	}
	_ = s.ar.Close()
}

func loadSnapshots(opts Options) (*archiveSnapshot, *archiveSnapshot, error) {
	switch {
	case opts.BeforeArchive != "" || opts.AfterArchive != "":
		if opts.BeforeArchive == "" || opts.AfterArchive == "" {
			return nil, nil, fmt.Errorf("both --before and --after are required when comparing two archives")
		}
		if opts.Archive != "" || opts.BeforeAt != "" || opts.AfterAt != "" {
			return nil, nil, fmt.Errorf("use either --before/--after or --archive with --before-at/--after-at")
		}
		before, err := loadArchiveSnapshot(opts.BeforeArchive, time.Time{})
		if err != nil {
			return nil, nil, err
		}
		after, err := loadArchiveSnapshot(opts.AfterArchive, time.Time{})
		if err != nil {
			before.cleanup()
			return nil, nil, err
		}
		return before, after, nil
	case opts.Archive != "":
		if opts.BeforeAt == "" || opts.AfterAt == "" {
			return nil, nil, fmt.Errorf("--before-at and --after-at are required with --archive")
		}
		base, err := loadArchiveSnapshot(opts.Archive, time.Time{})
		if err != nil {
			return nil, nil, err
		}
		beforeAt, err := parseSnapshotTime(base.meta, opts.BeforeAt)
		if err != nil {
			base.cleanup()
			return nil, nil, err
		}
		afterAt, err := parseSnapshotTime(base.meta, opts.AfterAt)
		if err != nil {
			base.cleanup()
			return nil, nil, err
		}
		before, err := loadArchiveSnapshot(opts.Archive, beforeAt)
		if err != nil {
			base.cleanup()
			return nil, nil, err
		}
		after, err := loadArchiveSnapshot(opts.Archive, afterAt)
		if err != nil {
			base.cleanup()
			before.cleanup()
			return nil, nil, err
		}
		base.cleanup()
		return before, after, nil
	default:
		return nil, nil, fmt.Errorf("provide either --before and --after, or --archive with --before-at and --after-at")
	}
}

func loadArchiveSnapshot(archivePath string, at time.Time) (*archiveSnapshot, error) {
	ar, err := archivepkg.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("opening archive %q: %w", archivePath, err)
	}
	store, err := server.LoadStore(ar)
	if err != nil {
		_ = ar.Close()
		return nil, fmt.Errorf("loading archive %q: %w", archivePath, err)
	}
	shot := &archiveSnapshot{
		ar:       ar,
		meta:     store.Metadata,
		snapshot: make(map[string]json.RawMessage, len(store.Index)),
	}
	for path := range store.Index {
		if strings.Contains(path, "?as=Table") {
			continue
		}
		body, code, err := store.Latest(path, at)
		if err != nil {
			shot.cleanup()
			return nil, fmt.Errorf("reading %s from %q: %w", path, archivePath, err)
		}
		if code != 200 {
			continue
		}
		shot.snapshot[path] = append(json.RawMessage(nil), body...)
	}
	return shot, nil
}

func parseSnapshotTime(meta capture.CaptureMetadata, raw string) (time.Time, error) {
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		d, derr := time.ParseDuration(raw)
		if derr != nil {
			return time.Time{}, fmt.Errorf("parsing time %q: must be RFC3339 or a relative duration like -5m", raw)
		}
		at = meta.CapturedUntil.Add(d)
	}
	if !meta.CapturedAt.IsZero() && at.Before(meta.CapturedAt) {
		return time.Time{}, fmt.Errorf("parsing time %q: requested time %s is before capture start %s", raw, at.Format(time.RFC3339), meta.CapturedAt.Format(time.RFC3339))
	}
	if !meta.CapturedUntil.IsZero() && at.After(meta.CapturedUntil) {
		return time.Time{}, fmt.Errorf("parsing time %q: requested time %s is after capture end %s", raw, at.Format(time.RFC3339), meta.CapturedUntil.Format(time.RFC3339))
	}
	return at, nil
}

func matchesFilters(path, resource, namespace string) bool {
	_, _, r, ns := parseAPIPath(path)
	if resource != "" && r != resource {
		return false
	}
	if namespace != "" && ns != namespace {
		return false
	}
	if resource != "" || namespace != "" {
		return r != ""
	}
	return true
}

func parseAPIPath(path string) (group, version, resource, namespace string) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case len(parts) >= 3 && parts[0] == "api":
		version = parts[1]
		if len(parts) == 3 {
			resource = parts[2]
		} else if len(parts) == 5 && parts[2] == "namespaces" {
			namespace = parts[3]
			resource = parts[4]
		}
	case len(parts) >= 4 && parts[0] == "apis":
		group = parts[1]
		version = parts[2]
		if len(parts) == 4 {
			resource = parts[3]
		} else if len(parts) == 6 && parts[3] == "namespaces" {
			namespace = parts[4]
			resource = parts[5]
		}
	}
	return
}

func prettyJSON(body []byte) string {
	if len(body) == 0 {
		return "null\n"
	}
	var out bytes.Buffer
	if err := json.Indent(&out, body, "", "  "); err == nil {
		out.WriteByte('\n')
		return out.String()
	}
	return string(body) + "\n"
}

func jsonEqual(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	var av any
	if err := json.Unmarshal(a, &av); err != nil {
		return string(a) == string(b)
	}
	var bv any
	if err := json.Unmarshal(b, &bv); err != nil {
		return string(a) == string(b)
	}
	aj, _ := json.Marshal(av)
	bj, _ := json.Marshal(bv)
	return string(aj) == string(bj)
}

func colorizeDiff(in string) string {
	const (
		red   = "\x1b[31m"
		green = "\x1b[32m"
		cyan  = "\x1b[36m"
		reset = "\x1b[0m"
	)
	lines := strings.SplitAfter(in, "\n")
	var out strings.Builder
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "@@"):
			out.WriteString(cyan)
			out.WriteString(line)
			out.WriteString(reset)
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			out.WriteString(green)
			out.WriteString(line)
			out.WriteString(reset)
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			out.WriteString(red)
			out.WriteString(line)
			out.WriteString(reset)
		default:
			out.WriteString(line)
		}
	}
	return out.String()
}
