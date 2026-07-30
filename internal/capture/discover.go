package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/phenixblue/k8shark/internal/config"
)

// tableSchemaIndexKeySuffix marks a columns-only Table record: a captured
// meta.k8s.io/v1 Table with its rows stripped, keeping only columnDefinitions.
// captureCoreTableSchemas writes it for native kinds whose cluster-scoped list
// path isn't already captured as a full ?as=Table (untargeted kinds, and kinds
// targeted only in specific namespaces), so the replay server can render
// kubectl-accurate columns for objects that have no full captured Table —
// without ever persisting those kinds' object data. The sentinel cannot appear
// in real API paths.
const tableSchemaIndexKeySuffix = "?as=TableSchema"

// defaultAutoDiscoverExcludeGroups are API groups that produce no useful
// captures (internal machinery, aggregated metrics, etc.) and are always
// excluded during auto-discovery regardless of user config.
var defaultAutoDiscoverExcludeGroups = map[string]bool{
	"metrics.k8s.io":         true,
	"apiregistration.k8s.io": true,
	"authentication.k8s.io":  true,
	"authorization.k8s.io":   true,
}

// autoDiscoverResources reads the already-cached /apis discovery documents
// from e.discoveryCache (populated earlier in the same run by fetchDiscovery)
// and appends one config.Resource entry per group-version-resource tuple that
// is not already covered by an explicit config entry. The appended entries are
// then picked up by the standard poll loop in Run().
func (e *Engine) autoDiscoverResources(ctx context.Context) {
	// Build the exclude set: defaults + user overrides.
	exclude := make(map[string]bool, len(defaultAutoDiscoverExcludeGroups))
	for g := range defaultAutoDiscoverExcludeGroups {
		exclude[g] = true
	}
	for _, g := range e.cfg.AutoDiscoverExcludeGroups {
		exclude[g] = true
	}

	// Build a set of already-configured (group, version, resource) triples so
	// we don't add duplicates.
	type gvr struct{ group, version, resource string }
	configured := make(map[gvr]bool, len(e.cfg.Resources))
	directives := make([]config.Resource, 0)
	for _, r := range e.cfg.Resources {
		if r.All {
			directives = append(directives, r)
			continue
		}
		configured[gvr{r.Group, r.Version, r.Resource}] = true
	}
	if len(directives) == 0 && e.cfg.AutoDiscover {
		directives = append(directives, config.Resource{All: true, IntervalRaw: "30s", Interval: 30 * time.Second})
	}
	discoverCore := len(directives) > 0

	apisBody := e.discoveryCache["/apis"]
	if apisBody == nil {
		// Resilience: retry /apis once if initial discovery capture missed it
		// (e.g. transient API error). Without this, whole API groups can be
		// silently skipped from auto-discovery.
		if fetched, code := e.doFetch(ctx, "/apis", "", true); code == http.StatusOK && fetched != nil {
			apisBody = fetched
			e.discoveryCache["/apis"] = fetched
		} else {
			if e.verbose {
				fmt.Fprintln(os.Stderr, "  [auto-discover] /apis not in discovery cache; skipping")
			}
			return
		}
	}

	var groupList struct {
		Kind   string `json:"kind"`
		Groups []struct {
			Name     string `json:"name"`
			Versions []struct {
				GroupVersion string `json:"groupVersion"`
				Version      string `json:"version"`
			} `json:"versions"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(apisBody, &groupList); err != nil || (groupList.Kind != "" && groupList.Kind != "APIGroupList") {
		if fetched, code := e.doFetch(ctx, "/apis", "", true); code == http.StatusOK && fetched != nil {
			apisBody = fetched
			e.discoveryCache["/apis"] = fetched
			if err := json.Unmarshal(apisBody, &groupList); err != nil || (groupList.Kind != "" && groupList.Kind != "APIGroupList") {
				return
			}
		} else {
			return
		}
	}

	added := 0
	for _, g := range groupList.Groups {
		if exclude[g.Name] {
			continue
		}
		for _, gv := range g.Versions {
			gvPath := "/apis/" + gv.GroupVersion
			gvBody := e.discoveryCache[gvPath]
			if gvBody == nil {
				// Resilience: retry this group-version once if missing from cache.
				if fetched, code := e.doFetch(ctx, gvPath, "", true); code == http.StatusOK && fetched != nil {
					gvBody = fetched
					e.discoveryCache[gvPath] = fetched
				} else {
					continue
				}
			}

			var resList struct {
				Kind      string `json:"kind"`
				Resources []struct {
					Name       string `json:"name"`
					Namespaced bool   `json:"namespaced"`
				} `json:"resources"`
			}
			if err := json.Unmarshal(gvBody, &resList); err != nil || (resList.Kind != "" && resList.Kind != "APIResourceList") {
				if fetched, code := e.doFetch(ctx, gvPath, "", true); code == http.StatusOK && fetched != nil {
					gvBody = fetched
					e.discoveryCache[gvPath] = fetched
					if err := json.Unmarshal(gvBody, &resList); err != nil || (resList.Kind != "" && resList.Kind != "APIResourceList") {
						continue
					}
				} else {
					continue
				}
			}

			parts := strings.SplitN(gv.GroupVersion, "/", 2)
			group := parts[0]
			version := gv.Version
			if len(parts) == 2 {
				version = parts[1]
			}

			for _, res := range resList.Resources {
				// Skip sub-resources (contain a slash, e.g. "pods/status").
				if strings.Contains(res.Name, "/") {
					continue
				}
				key := gvr{group, version, res.Name}
				if configured[key] {
					continue
				}

				for _, d := range directives {
					if d.Scope == "cluster" && res.Namespaced {
						continue
					}
					if d.Scope == "namespaced" && !res.Namespaced {
						continue
					}

					newRes := config.Resource{
						Group:          group,
						Version:        version,
						Resource:       res.Name,
						IntervalRaw:    d.IntervalRaw,
						Interval:       d.Interval,
						Dedup:          d.Dedup,
						Watch:          d.Watch,
						AutoDiscovered: true,
					}
					if newRes.Interval == 0 {
						newRes.Interval = 30 * time.Second
						newRes.IntervalRaw = "30s"
					}
					if res.Namespaced {
						if len(d.Namespaces) > 0 {
							newRes.Namespaces = append([]string(nil), d.Namespaces...)
						} else {
							newRes.Namespaces = []string{"*"}
						}
					}

					e.cfg.Resources = append(e.cfg.Resources, newRes)
					configured[key] = true
					added++
					if e.verbose {
						fmt.Fprintf(os.Stdout, "  [auto-discover] added %s/%s/%s (namespaced=%v, scope=%s)\n",
							group, version, res.Name, res.Namespaced, firstNonEmpty(d.Scope, "all"))
					}
					break
				}
			}
		}
	}

	// all=true directives represent "capture all", which includes core/v1
	// resources like pods, services, configmaps, and nodes.
	if discoverCore {
		coreBody := e.discoveryCache["/api/v1"]
		if coreBody == nil {
			if fetched, code := e.doFetch(ctx, "/api/v1", "", true); code == http.StatusOK && fetched != nil {
				coreBody = fetched
				e.discoveryCache["/api/v1"] = fetched
			}
		}
		if coreBody != nil {
			var coreList struct {
				Kind      string `json:"kind"`
				Resources []struct {
					Name       string `json:"name"`
					Namespaced bool   `json:"namespaced"`
				} `json:"resources"`
			}
			if err := json.Unmarshal(coreBody, &coreList); err == nil && (coreList.Kind == "" || coreList.Kind == "APIResourceList") {
				for _, res := range coreList.Resources {
					if strings.Contains(res.Name, "/") {
						continue
					}
					key := gvr{"", "v1", res.Name}
					if configured[key] {
						continue
					}

					for _, d := range directives {
						if d.Scope == "cluster" && res.Namespaced {
							continue
						}
						if d.Scope == "namespaced" && !res.Namespaced {
							continue
						}

						newRes := config.Resource{
							Group:          "",
							Version:        "v1",
							Resource:       res.Name,
							IntervalRaw:    d.IntervalRaw,
							Interval:       d.Interval,
							Dedup:          d.Dedup,
							Watch:          d.Watch,
							AutoDiscovered: true,
						}
						if newRes.Interval == 0 {
							newRes.Interval = 30 * time.Second
							newRes.IntervalRaw = "30s"
						}
						if res.Namespaced {
							if len(d.Namespaces) > 0 {
								newRes.Namespaces = append([]string(nil), d.Namespaces...)
							} else {
								newRes.Namespaces = []string{"*"}
							}
						}

						e.cfg.Resources = append(e.cfg.Resources, newRes)
						configured[key] = true
						added++
						if e.verbose {
							fmt.Fprintf(os.Stdout, "  [auto-discover] added %s/%s/%s (namespaced=%v, scope=%s)\n",
								"core", "v1", res.Name, res.Namespaced, firstNonEmpty(d.Scope, "all"))
						}
						break
					}
				}
			}
		}
	}

	if e.verbose {
		fmt.Fprintf(os.Stdout, "  [auto-discover] added %d resource types\n", added)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// nativeAPIGroups are the built-in Kubernetes API groups whose Table column
// schemas captureCoreTableSchemas always records. CRD-backed groups are
// deliberately excluded: custom resources derive their columns from the CRD's
// additionalPrinterColumns at replay time, so a schema sweep would add cost
// without benefit (and could touch far more, unrelated groups). The empty
// string is the core group (/api/v1).
var nativeAPIGroups = map[string]bool{
	"":                             true,
	"apps":                         true,
	"batch":                        true,
	"autoscaling":                  true,
	"networking.k8s.io":            true,
	"policy":                       true,
	"rbac.authorization.k8s.io":    true,
	"storage.k8s.io":               true,
	"scheduling.k8s.io":            true,
	"node.k8s.io":                  true,
	"coordination.k8s.io":          true,
	"certificates.k8s.io":          true,
	"discovery.k8s.io":             true,
	"events.k8s.io":                true,
	"admissionregistration.k8s.io": true,
	"apiextensions.k8s.io":         true,
	"apiregistration.k8s.io":       true,
	"authentication.k8s.io":        true,
	"authorization.k8s.io":         true,
	"flowcontrol.apiserver.k8s.io": true,
}

// nativeSchemaKind is a list-capable native GVR discovered from the API server.
// namespaces holds concrete namespaces the capture targets for this kind (empty
// for untargeted or wildcard kinds); they serve as RBAC-allowed fallbacks when
// the cluster-wide list is forbidden.
type nativeSchemaKind struct {
	group, version, resource string
	namespaces               []string
}

// captureCoreTableSchemas records the Table columnDefinitions (rows stripped)
// for every list-capable native kind that isn't already being captured, so the
// replay server can render kubectl-accurate columns/`-o wide` for objects it
// has no full captured Table for (writable-overlay creations, or kinds/captures
// the user didn't target). It fetches one row (?limit=1) purely to obtain the
// server-side columnDefinitions, then discards the rows — so it never persists
// object data for kinds (e.g. Secrets) the user didn't ask to capture.
//
// Runs once; safe to run concurrently with polling (storeRecord is mutex-guarded).
func (e *Engine) captureCoreTableSchemas(ctx context.Context) {
	kinds := e.nativeListableKinds()
	if len(kinds) == 0 {
		return
	}

	const schemaWorkers = 8
	sem := make(chan struct{}, schemaWorkers)
	var wg sync.WaitGroup
	for _, k := range kinds {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(k nativeSchemaKind) {
			defer wg.Done()
			// Respect cancellation while waiting for a worker slot, so SIGINT/
			// SIGTERM isn't delayed by blocked goroutines (this runs in Run's wg).
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			e.fetchTableSchema(ctx, k)
		}(k)
	}
	wg.Wait()
}

// nativeListableKinds enumerates list-capable, non-subresource GVRs in the
// native API groups from the discovery documents cached by fetchDiscovery.
// Reads discoveryCache under the lock; callers run after fetchDiscovery has
// populated it.
//
// A GVR is skipped only when the capture already polls its cluster-scoped list
// path (a config entry with no explicit namespaces) — that yields a cluster-path
// ?as=Table already. GVRs captured only in specific namespaces (or via wildcard
// demux, which stores per-namespace Tables) have no cluster-path Table, so we
// still record a columns-only schema there — otherwise overlay-created objects
// in an uncaptured namespace, for a native kind without a built-in printer,
// would fall back to generic columns.
func (e *Engine) nativeListableKinds() []nativeSchemaKind {
	type gvr struct{ group, version, resource string }
	clusterCaptured := make(map[gvr]bool, len(e.cfg.Resources))
	nsByGVR := make(map[gvr][]string)
	for _, r := range e.cfg.Resources {
		if r.All {
			continue
		}
		key := gvr{r.Group, r.Version, r.Resource}
		if len(r.Namespaces) == 0 {
			clusterCaptured[key] = true
			continue
		}
		// Concrete namespaces are RBAC-allowed fallbacks for the schema fetch
		// (skip the "*" wildcard — that path polls cluster-wide already).
		for _, ns := range r.Namespaces {
			if ns != "" && ns != "*" {
				nsByGVR[key] = append(nsByGVR[key], ns)
			}
		}
	}

	e.mu.Lock()
	coreBody := e.discoveryCache["/api/v1"]
	apisBody := e.discoveryCache["/apis"]
	e.mu.Unlock()

	var out []nativeSchemaKind
	add := func(group, version string, body []byte) {
		for _, r := range listableResources(body) {
			key := gvr{group, version, r}
			if clusterCaptured[key] {
				continue
			}
			out = append(out, nativeSchemaKind{group, version, r, nsByGVR[key]})
		}
	}

	// Core group.
	if nativeAPIGroups[""] {
		add("", "v1", coreBody)
	}

	// Native API groups.
	var groupList struct {
		Kind   string `json:"kind"`
		Groups []struct {
			Name     string `json:"name"`
			Versions []struct {
				GroupVersion string `json:"groupVersion"`
				Version      string `json:"version"`
			} `json:"versions"`
		} `json:"groups"`
	}
	// Guard against a non-APIGroupList /apis cache (e.g. a Status object from a
	// transient discovery error), which would parse to zero groups and silently
	// skip schema capture for every non-core native group.
	if err := json.Unmarshal(apisBody, &groupList); err != nil || (groupList.Kind != "" && groupList.Kind != "APIGroupList") {
		if e.verbose {
			fmt.Fprintln(os.Stderr, "  [schema] /apis discovery unavailable or invalid; capturing core-group schemas only")
		}
	} else {
		for _, g := range groupList.Groups {
			if !nativeAPIGroups[g.Name] {
				continue
			}
			for _, gv := range g.Versions {
				e.mu.Lock()
				gvBody := e.discoveryCache["/apis/"+gv.GroupVersion]
				e.mu.Unlock()
				version := gv.Version
				if version == "" {
					if parts := strings.SplitN(gv.GroupVersion, "/", 2); len(parts) == 2 {
						version = parts[1]
					}
				}
				add(g.Name, version, gvBody)
			}
		}
	}
	return out
}

// listableResources parses an APIResourceList body and returns the names of
// resources that support `list` and are not sub-resources (no "/").
func listableResources(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	var rl struct {
		Kind      string `json:"kind"`
		Resources []struct {
			Name  string   `json:"name"`
			Verbs []string `json:"verbs"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &rl); err != nil || (rl.Kind != "" && rl.Kind != "APIResourceList") {
		return nil
	}
	var out []string
	for _, r := range rl.Resources {
		if strings.Contains(r.Name, "/") {
			continue
		}
		listable := false
		for _, v := range r.Verbs {
			if v == "list" {
				listable = true
				break
			}
		}
		if listable {
			out = append(out, r.Name)
		}
	}
	return out
}

// fetchTableSchema fetches a single-row Table for a kind and stores a
// columns-only record (rows stripped) at the kind's cluster-path schema key.
// It tries the cluster-wide list first; if that fails (e.g. cluster-wide list is
// RBAC-forbidden while specific namespaces are allowed), it falls back to the
// kind's targeted namespaces — columnDefinitions are namespace-independent, so
// the schema is still stored under the cluster path. RBAC denials and non-Table
// responses are skipped silently.
func (e *Engine) fetchTableSchema(ctx context.Context, k nativeSchemaKind) {
	clusterPath := buildAPIPath(k.group, k.version, k.resource, "")

	// Candidate list paths: cluster-wide (one fetch covers all namespaces), then
	// any targeted namespace as an RBAC-allowed fallback.
	paths := []string{clusterPath}
	for _, ns := range k.namespaces {
		paths = append(paths, buildAPIPath(k.group, k.version, k.resource, ns))
	}

	for _, p := range paths {
		// ?limit=1 minimizes data transfer; we only want the columnDefinitions.
		body, code := e.rawFetch(ctx, p+"?limit=1", tableIndexKeySuffix)
		if body == nil || code != http.StatusOK {
			continue
		}
		schema, ok := stripTableRows(body)
		if !ok {
			continue // not a Table (kind doesn't support server-side printing)
		}
		// Store at the cluster path so the replay server finds it for any namespace.
		e.storeRecord(clusterPath+tableSchemaIndexKeySuffix, schema, code, false)
		if e.verbose {
			fmt.Fprintf(os.Stdout, "  [schema] %s%s\n", clusterPath, tableSchemaIndexKeySuffix)
		}
		return
	}
}

// stripTableRows returns a meta.k8s.io/v1 Table body carrying only the
// columnDefinitions from the input Table (rows removed). ok is false when the
// input isn't a Table with column definitions.
func stripTableRows(body []byte) ([]byte, bool) {
	var t struct {
		APIVersion        string          `json:"apiVersion"`
		Kind              string          `json:"kind"`
		ColumnDefinitions json.RawMessage `json:"columnDefinitions"`
	}
	// Require an actual Table (not just any payload that happens to carry a
	// columnDefinitions field), so we never persist a bogus schema record.
	if err := json.Unmarshal(body, &t); err != nil || t.Kind != "Table" || len(t.ColumnDefinitions) == 0 {
		return nil, false
	}
	out, err := json.Marshal(map[string]any{
		"apiVersion":        firstNonEmpty(t.APIVersion, "meta.k8s.io/v1"),
		"kind":              firstNonEmpty(t.Kind, "Table"),
		"metadata":          map[string]any{"resourceVersion": "0"},
		"columnDefinitions": t.ColumnDefinitions,
		"rows":              []any{},
	})
	if err != nil {
		return nil, false
	}
	return out, true
}
