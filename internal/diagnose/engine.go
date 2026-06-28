package diagnose

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/server"
)

// Options configures a diagnose run.
type Options struct {
	At          time.Time // zero = latest snapshot
	MinSeverity string    // "" or "info" = all; "warning"/"critical" filter
	Category    string    // "" = all categories
}

// Run analyzes the capture in store and returns a ranked Report.
func Run(store *server.CaptureStore, opts Options) Report {
	at := opts.At
	if at.IsZero() {
		at = store.Metadata.CapturedUntil
	}

	var fs []Finding
	fs = append(fs, podHealthFindings(store, at)...)
	fs = append(fs, schedulingFindings(store, at)...)
	fs = append(fs, pvcFindings(store, at)...)
	fs = append(fs, versionSkewFindings(store, at)...)
	fs = append(fs, missingResourceFindings(store, at)...)
	fs = append(fs, replicaFindings(store, at)...)
	fs = append(fs, nodeConditionFindings(store, at)...)
	fs = append(fs, deprecatedAPIFindings(store)...)

	// Every finding represents at least one object; normalize so count is a
	// stable, always-present field for JSON/CI consumers.
	for i := range fs {
		if fs[i].Count == 0 {
			fs[i].Count = 1
		}
	}

	// Filter.
	min := opts.MinSeverity
	if min == "" {
		min = SeverityInfo
	}
	filtered := fs[:0:0]
	for _, f := range fs {
		if !SeverityAtLeast(f.Severity, min) {
			continue
		}
		if opts.Category != "" && f.Category != opts.Category {
			continue
		}
		filtered = append(filtered, f)
	}
	sortFindings(filtered)

	rep := Report{SchemaVersion: SchemaVersion, CaptureID: store.Metadata.CaptureID, Findings: filtered}
	if !opts.At.IsZero() {
		rep.At = opts.At.UTC().Format(time.RFC3339)
	}
	for _, f := range filtered {
		switch f.Severity {
		case SeverityCritical:
			rep.Summary.Critical++
		case SeverityWarning:
			rep.Summary.Warning++
		default:
			rep.Summary.Info++
		}
	}
	return rep
}

// ── shared helpers ───────────────────────────────────────────────────────────

// forEachResource calls fn for every captured list of the given resource
// (skipping Table/query variants), passing the namespace, list path, and items.
func forEachResource(store *server.CaptureStore, at time.Time, resource string, fn func(ns, path string, items []json.RawMessage)) {
	for path := range store.Index {
		if strings.Contains(path, "?") {
			continue
		}
		_, _, res, ns := parseAPIPath(path)
		if res != resource {
			continue
		}
		body, code, err := store.ReconstructAt(path, at)
		if err != nil || code != 200 || len(body) == 0 {
			continue
		}
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(body, &list) != nil {
			continue
		}
		fn(ns, path, list.Items)
	}
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
		} else {
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

type objMeta struct {
	Metadata struct {
		Name            string `json:"name"`
		Namespace       string `json:"namespace"`
		OwnerReferences []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
}

// owner returns a stable grouping key + display name for an object: its first
// ownerReference name, else its own name.
func (m objMeta) owner() string {
	if len(m.Metadata.OwnerReferences) > 0 && m.Metadata.OwnerReferences[0].Name != "" {
		return m.Metadata.OwnerReferences[0].Name
	}
	return m.Metadata.Name
}

// grouper accumulates findings keyed by (rule, namespace, owner), counting
// affected objects and keeping the first as representative.
type grouper struct {
	order []string
	byKey map[string]*Finding
}

func newGrouper() *grouper { return &grouper{byKey: map[string]*Finding{}} }

func (g *grouper) add(key string, f Finding) {
	if existing, ok := g.byKey[key]; ok {
		existing.Count++
		return
	}
	f.Count = 1
	g.byKey[key] = &f
	g.order = append(g.order, key)
}

func (g *grouper) findings() []Finding {
	out := make([]Finding, 0, len(g.order))
	for _, k := range g.order {
		out = append(out, *g.byKey[k])
	}
	return out
}
