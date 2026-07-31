package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	archivepkg "github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/k8spath"
	"github.com/phenixblue/k8shark/internal/store"
	"github.com/phenixblue/k8shark/internal/timewindow"
	"github.com/pmezard/go-difflib/difflib"
)

type Options struct {
	BeforeArchive string
	AfterArchive  string
	Archive       string
	// From/To are the before/after snapshot times for the --archive
	// (single-archive) mode: RFC3339 or a relative duration like -5m.
	From      string
	To        string
	Resource  string
	Namespace string
	// Identities decrypts encrypted archive(s); ignored for plaintext ones.
	// A single key source is shared across both archives in the two-archive
	// mode (a documented v1.0 limitation).
	Identities []age.Identity
}

// SchemaVersion is the version of the `kshrk diff -o json` output shape.
// Bumped only on a breaking change to Result/Change's own fields — not for
// changes to the passthrough Kubernetes objects under Change.Before /
// Change.After, whose field names belong to the cluster's API, not to k8shark
// (see docs/stability-policy.md).
const SchemaVersion = 1

type Result struct {
	SchemaVersion int      `json:"schema_version"`
	Changes       []Change `json:"changes"`
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

	return &Result{SchemaVersion: SchemaVersion, Changes: changes}, nil
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
	store    *store.CaptureStore
	meta     capture.CaptureMetadata
	snapshot map[string]json.RawMessage
}

func (s *archiveSnapshot) cleanup() {
	if s == nil || s.ar == nil {
		return
	}
	// store.Close waits for LoadStore's background enrichment pass, which
	// reads from the archive independently of the snapshot loop below — it
	// must finish before ar.Close() runs (#232).
	if s.store != nil {
		s.store.Close()
	}
	_ = s.ar.Close()
}

func loadSnapshots(opts Options) (*archiveSnapshot, *archiveSnapshot, error) {
	switch {
	case opts.BeforeArchive != "" || opts.AfterArchive != "":
		if opts.BeforeArchive == "" || opts.AfterArchive == "" {
			return nil, nil, fmt.Errorf("both --before and --after are required when comparing two archives")
		}
		if opts.Archive != "" || opts.From != "" || opts.To != "" {
			return nil, nil, fmt.Errorf("use either --before/--after or --archive with --from/--to")
		}
		before, err := loadArchiveSnapshot(opts.BeforeArchive, time.Time{}, opts.Identities)
		if err != nil {
			return nil, nil, err
		}
		after, err := loadArchiveSnapshot(opts.AfterArchive, time.Time{}, opts.Identities)
		if err != nil {
			before.cleanup()
			return nil, nil, err
		}
		return before, after, nil
	case opts.Archive != "":
		if opts.From == "" || opts.To == "" {
			return nil, nil, fmt.Errorf("--from and --to are required with --archive")
		}
		base, err := loadArchiveSnapshot(opts.Archive, time.Time{}, opts.Identities)
		if err != nil {
			return nil, nil, err
		}
		beforeAt, err := timewindow.ParseAt(opts.From, base.meta.CapturedAt, base.meta.CapturedUntil, "--from")
		if err != nil {
			base.cleanup()
			return nil, nil, err
		}
		afterAt, err := timewindow.ParseAt(opts.To, base.meta.CapturedAt, base.meta.CapturedUntil, "--to")
		if err != nil {
			base.cleanup()
			return nil, nil, err
		}
		before, err := loadArchiveSnapshot(opts.Archive, beforeAt, opts.Identities)
		if err != nil {
			base.cleanup()
			return nil, nil, err
		}
		after, err := loadArchiveSnapshot(opts.Archive, afterAt, opts.Identities)
		if err != nil {
			base.cleanup()
			before.cleanup()
			return nil, nil, err
		}
		base.cleanup()
		return before, after, nil
	default:
		return nil, nil, fmt.Errorf("provide either --before and --after, or --archive with --from and --to")
	}
}

func loadArchiveSnapshot(archivePath string, at time.Time, identities []age.Identity) (*archiveSnapshot, error) {
	ar, err := archivepkg.OpenWithIdentities(archivePath, identities)
	if err != nil {
		return nil, fmt.Errorf("opening archive %q: %w", archivePath, err)
	}
	cs, err := store.LoadStore(ar)
	if err != nil {
		_ = ar.Close()
		return nil, fmt.Errorf("loading archive %q: %w", archivePath, err)
	}
	shot := &archiveSnapshot{
		ar:       ar,
		store:    cs,
		meta:     cs.Metadata,
		snapshot: make(map[string]json.RawMessage, len(cs.Index)),
	}
	for path := range cs.Index {
		if strings.Contains(path, "?as=Table") {
			continue
		}
		body, code, err := cs.Latest(path, at)
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
	return k8spath.Parse(path)
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
