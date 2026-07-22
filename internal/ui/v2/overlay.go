package v2

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/server"
)

// mergeOverlay merges the overlay's writes for path's scope over
// capturedItems (the base list, already read and parsed by the caller, or nil
// when the path was never captured) — see server.Server.MergeOverlayList.
// Callers that already read the captured body for another reason (e.g. to
// pull List-envelope Kind/apiVersion hints) should call this directly instead
// of reconstructMergedItems, to avoid reading and re-parsing it twice.
func (h *Handler) mergeOverlay(path string, capturedItems []json.RawMessage) []json.RawMessage {
	if h.Overlay == nil {
		return capturedItems
	}
	group, version, resource, ns := parseAPIPath(path)
	return h.Overlay.MergeOverlayList(group, version, resource, ns, capturedItems)
}

// reconstructMergedItems reconstructs the list body at path and merges any
// writable-overlay writes over it (see mergeOverlay). This is the one-stop
// read for every list view in this package that doesn't already need the raw
// captured body for something else: it returns overlay-created items even for
// a path the capture never recorded at all (a brand-new namespace, or a
// resource kind that didn't exist until a CRD was installed at runtime), since
// mergeOverlay tolerates a nil/empty base. Returns nil if there is nothing to
// show either way.
func (h *Handler) reconstructMergedItems(path string, at time.Time) []json.RawMessage {
	var items []json.RawMessage
	if body, code, err := h.Store.ReconstructAt(path, at); err == nil && code == http.StatusOK && len(body) > 0 {
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(body, &list) == nil {
			items = list.Items
		}
	}
	return h.mergeOverlay(path, items)
}

// resourcePathsFor returns every distinct API list path for a resource plural
// name (e.g. "pods") — every namespaced/cluster-scoped path the capture's
// index has for it, plus (when overlay is present) any overlay-only scope
// with no index entry at all, such as a namespace created purely by a
// `helm install --create-namespace` mid-replay. Callers that already
// hardcode a resource's list path(s) per namespace (namespace.go's workload/
// VM/pod groups) don't need this — reconstructMergedItems alone is enough,
// since it tolerates an uncaptured path. This is for callers that otherwise
// discover paths by walking the index (lists.go, overview.go). A caller
// needing several resources at once (e.g. every workload kind) should use
// resourcePathsForResources instead, to scan the index only once.
func (h *Handler) resourcePathsFor(resource string) []string {
	return h.resourcePathsForResources(map[string]bool{resource: true})[resource]
}

// resourcePathsForResources is resourcePathsFor for several resources at
// once: it scans the index (and overlay scopes) exactly once, grouping
// matching paths by resource, instead of once per resource — the cluster-wide
// workload list would otherwise re-walk the whole index per workload kind.
func (h *Handler) resourcePathsForResources(resources map[string]bool) map[string][]string {
	out := map[string][]string{}
	seen := map[string]bool{}
	for path, entry := range h.Store.Index {
		if entry == nil || len(entry.Seqs) == 0 || strings.Contains(path, "?") {
			continue
		}
		_, _, res, _ := parseAPIPath(path)
		if !resources[res] || seen[path] {
			continue
		}
		seen[path] = true
		out[res] = append(out[res], path)
	}
	if h.Overlay != nil {
		for _, sc := range h.Overlay.OverlayScopes() {
			if !resources[sc.Resource] {
				continue
			}
			path := apiListPath(sc.Group, sc.Version, sc.Resource, sc.Namespace)
			if seen[path] {
				continue
			}
			seen[path] = true
			out[sc.Resource] = append(out[sc.Resource], path)
		}
	}
	return out
}

// mergeOverlayNamespaceCounts folds overlay-only per-namespace resource counts
// into counts (as returned by CaptureStore.NamespaceItemCountsAt), so a
// namespace or resource kind that exists only because of overlay writes still
// shows up in namespace/workload/pod KPI totals. It only fills in counts the
// capture recorded as zero: a resource kind the namespace already had
// captured records for is left as-is rather than re-read and re-summed here,
// which would need a full list-body read per resource kind across every
// namespace — the exact cost NamespaceItemCountsAt exists to avoid on the
// overview page. The per-namespace drill-down (namespace.go) reads real
// bodies and is exact; this is a best-effort summary.
func mergeOverlayNamespaceCounts(counts map[string]map[string]int, scopes []server.OverlayScope) {
	for _, sc := range scopes {
		if sc.Namespace == "" || sc.Count == 0 {
			continue
		}
		byRes, ok := counts[sc.Namespace]
		if !ok {
			byRes = map[string]int{}
			counts[sc.Namespace] = byRes
		}
		if byRes[sc.Resource] == 0 {
			byRes[sc.Resource] = sc.Count
		}
	}
}

// mergeOverlayClusterCounts is mergeOverlayNamespaceCounts for cluster-scoped
// resources (e.g. a CRD created at runtime), folding into the map returned by
// clusterScopedResourceCounts. Same best-effort semantics.
func mergeOverlayClusterCounts(counts map[string]int, scopes []server.OverlayScope) {
	for _, sc := range scopes {
		if sc.Namespace != "" || sc.Count == 0 {
			continue
		}
		if counts[sc.Resource] == 0 {
			counts[sc.Resource] = sc.Count
		}
	}
}
