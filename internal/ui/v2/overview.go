package v2

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
)

const (
	sparkBucketCount = 30
	topNamespacesN   = 8
	recentEventsN    = 8
	maxIssuesShown   = 8
)

// workloadResources is the set of resource types categorised as "workloads"
// for KPI counting. Mirrors the existing /api/ui drill-down classifier.
var workloadResources = map[string]bool{
	"deployments":  true,
	"statefulsets": true,
	"daemonsets":   true,
	"jobs":         true,
	"replicasets":  true,
}

// vmResources counts as VirtualMachines on the dashboard.
var vmResources = map[string]bool{
	"virtualmachines":         true,
	"virtualmachineinstances": true,
}

func (h *Handler) serveOverview(w http.ResponseWriter, r *http.Request) {
	at := h.resolveAt(r)
	ov, err := h.buildOverview(at)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !at.IsZero() {
		ov.At = at.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, ov)
}

// resolveAt reads ?at= from the request, falling back to h.At.
func (h *Handler) resolveAt(r *http.Request) time.Time {
	q := r.URL.Query().Get("at")
	if q == "" {
		return h.At
	}
	t, err := time.Parse(time.RFC3339, q)
	if err != nil {
		return h.At
	}
	return t.UTC()
}

// buildOverview walks the index + reads pod bodies to compute everything the
// dashboard landing page needs. Designed to be called per request — first
// hit on a large archive takes ~1-3s (pod body reads dominate); subsequent
// hits ride the store's record LRU.
func (h *Handler) buildOverview(at time.Time) (*Overview, error) {
	store := h.Store
	if store == nil {
		return nil, fmt.Errorf("store not initialized")
	}

	ov := &Overview{
		Capture: CaptureMeta{
			CaptureID:         store.Metadata.CaptureID,
			CapturedAt:        store.Metadata.CapturedAt,
			CapturedUntil:     store.Metadata.CapturedUntil,
			KubernetesVersion: store.Metadata.KubernetesVersion,
			ServerAddress:     store.Metadata.ServerAddress,
			RecordCount:       store.Metadata.RecordCount,
		},
	}

	// Per-(ns, resource) counts from the index — no body reads.
	counts := store.NamespaceItemCountsAt(at)

	// Cluster-scoped resource totals from the index — at the latest record per path.
	clusterCounts := clusterScopedResourceCounts(store.Index, at)

	// Aggregate KPIs + per-resource totals.
	resourceTotals := map[string]int{}
	hasPerNS := map[string]bool{}
	for _, byRes := range counts {
		for res, n := range byRes {
			resourceTotals[res] += n
			hasPerNS[res] = true
			switch {
			case workloadResources[res]:
				ov.KPIs.Workloads += n
			case res == "pods":
				ov.KPIs.Pods += n
			case vmResources[res]:
				ov.KPIs.VirtualMachines += n
			}
		}
	}
	// Only add cluster-scoped totals for resources that don't already have
	// per-namespace records — otherwise we double-count when the engine
	// captured both the cluster-wide list (allNotFound fallback) AND the
	// per-namespace lists.
	for res, n := range clusterCounts {
		if hasPerNS[res] {
			continue
		}
		resourceTotals[res] += n
	}

	// Read pod bodies → classify health → fill UnhealthyPods + Issues.
	podHealths := h.readAllPodHealths(at)
	for _, ph := range podHealths {
		if ph.health.IsHealthy() {
			continue
		}
		ov.KPIs.UnhealthyPods++
		for _, reason := range ph.health.Issues {
			switch reason {
			case "CrashLoopBackOff":
				ov.KPIs.CrashLoopBackOff++
			case "Failed":
				ov.KPIs.Failed++
			case "OOMKilled":
				ov.KPIs.OOMKilled++
			}
		}
		if ph.health.Phase == "Pending" && !ph.health.IsHealthy() {
			ov.KPIs.Pending++
		}
	}
	ov.Issues = buildIssues(podHealths, maxIssuesShown)

	// Watch event totals + sparkline (per time bucket).
	ov.Sparkline = buildSparkline(store, at, sparkBucketCount)
	ov.KPIs.WatchEvents = ov.Sparkline.TotalEvents

	// Per-namespace summaries with health rollup.
	nsByName := map[string]*NamespaceSummary{}
	for ns, byRes := range counts {
		s := &NamespaceSummary{Name: ns}
		for res, n := range byRes {
			s.Resources += n
			if workloadResources[res] {
				s.Workloads += n
			}
			if res == "pods" {
				s.Pods += n
			}
		}
		nsByName[ns] = s
	}
	for _, ph := range podHealths {
		if ns := nsByName[ph.namespace]; ns != nil && !ph.health.IsHealthy() {
			ns.Unhealthy++
		}
	}
	ov.Namespaces = make([]NamespaceSummary, 0, len(nsByName))
	for _, s := range nsByName {
		ov.Namespaces = append(ov.Namespaces, *s)
	}
	sort.Slice(ov.Namespaces, func(i, j int) bool { return ov.Namespaces[i].Name < ov.Namespaces[j].Name })

	// Top namespaces by total resource count, capped.
	top := append([]NamespaceSummary(nil), ov.Namespaces...)
	sort.Slice(top, func(i, j int) bool { return top[i].Resources > top[j].Resources })
	if len(top) > topNamespacesN {
		top = top[:topNamespacesN]
	}
	ov.TopNamespaces = top

	// Resource tiles — sorted by count desc, but keep "Pods" / "Deployments"
	// up front when present for familiarity.
	ov.Resources = buildResourceTiles(resourceTotals)

	// Recent transitions from the watch index.
	ov.Recent = recentTransitions(store, at, recentEventsN)

	ov.KPIs.Namespaces = len(ov.Namespaces)
	return ov, nil
}

// podHealthEntry is the internal tuple used during overview aggregation.
type podHealthEntry struct {
	namespace string
	name      string
	prefix    string // for grouping similar pods
	health    PodHealth
}

// readAllPodHealths walks every per-namespace pod LIST in the index, reads
// the latest record at or before `at`, and classifies each pod's health.
func (h *Handler) readAllPodHealths(at time.Time) []podHealthEntry {
	store := h.Store
	var out []podHealthEntry

	for path, entry := range store.Index {
		if entry == nil || len(entry.Seqs) == 0 {
			continue
		}
		if strings.Contains(path, "?") {
			continue // skip Table records and other query-suffix variants
		}
		_, _, resource, ns := parseAPIPath(path)
		if resource != "pods" || ns == "" {
			continue
		}

		body, code, err := store.ReconstructAt(path, at)
		if err != nil || code != http.StatusOK || len(body) == 0 {
			continue
		}
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			continue
		}
		for _, raw := range list.Items {
			name := getName(raw)
			if name == "" {
				continue
			}
			ph := ClassifyPod(raw)
			out = append(out, podHealthEntry{
				namespace: ns,
				name:      name,
				prefix:    PodNamePrefix(name),
				health:    ph,
			})
		}
	}
	return out
}

// getName reads metadata.name from a raw Kubernetes object body.
func getName(raw json.RawMessage) string {
	var m struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	return m.Metadata.Name
}

// parseAPIPath is a local mirror of server.parseAPIPath (unexported there).
// Returns (group, version, resource, namespace).
func parseAPIPath(path string) (group, version, resource, namespace string) {
	// Strip any query suffix.
	if i := strings.Index(path, "?"); i >= 0 {
		path = path[:i]
	}
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

// clusterScopedResourceCounts walks paths with no namespace segment.
func clusterScopedResourceCounts(idx capture.Index, at time.Time) map[string]int {
	out := map[string]int{}
	for path, entry := range idx {
		if entry == nil || len(entry.Seqs) == 0 {
			continue
		}
		if strings.Contains(path, "?") {
			continue
		}
		_, _, resource, ns := parseAPIPath(path)
		if resource == "" || ns != "" {
			continue
		}
		// Pick the latest Counts entry at or before `at`.
		i := latestIndex(entry, at)
		if i < 0 || i >= len(entry.Counts) {
			continue
		}
		if entry.Counts[i] > 0 {
			out[resource] += entry.Counts[i]
		}
	}
	return out
}

// latestIndex returns the slice index into entry.Seqs that corresponds to
// the most recent record at or before `at`. Returns -1 if `at` precedes the
// first record. When at is zero the last index is returned.
func latestIndex(entry *capture.IndexEntry, at time.Time) int {
	if entry == nil || len(entry.Seqs) == 0 {
		return -1
	}
	if at.IsZero() || len(entry.Times) != len(entry.Seqs) {
		return len(entry.Seqs) - 1
	}
	pos := sort.Search(len(entry.Times), func(i int) bool {
		return entry.Times[i].After(at)
	})
	if pos == 0 {
		return -1
	}
	return pos - 1
}

// buildSparkline groups watch-event timestamps into sparkBucketCount equal
// buckets across the capture window.
func buildSparkline(store *server.CaptureStore, _ time.Time, buckets int) SparklineData {
	start := store.Metadata.CapturedAt.UTC()
	end := store.Metadata.CapturedUntil.UTC()
	if !end.After(start) || buckets <= 0 {
		return SparklineData{StartTime: start, EndTime: end}
	}

	step := end.Sub(start) / time.Duration(buckets)
	if step <= 0 {
		step = time.Second
	}

	cells := make([]SparkBucket, buckets)
	for i := range cells {
		cells[i].Time = start.Add(time.Duration(i) * step)
	}

	total := 0
	for path, wi := range store.WatchIndex {
		if wi == nil {
			continue
		}
		isPodPath := strings.Contains(path, "/pods")
		for i, t := range wi.Times {
			total++
			if !t.After(start) {
				cells[0].Total++
				if isPodPath && i < len(wi.EventTypes) && wi.EventTypes[i] == "DELETED" {
					cells[0].Bad++
				}
				continue
			}
			idx := int(t.Sub(start) / step)
			if idx < 0 {
				idx = 0
			} else if idx >= buckets {
				idx = buckets - 1
			}
			cells[idx].Total++
			if !isPodPath || i >= len(wi.EventTypes) {
				continue
			}
			switch wi.EventTypes[i] {
			case "DELETED":
				cells[idx].Bad++
			case "MODIFIED":
				// Heuristic: a fast burst of MODIFIED events on the same pod is
				// often a restart loop. We just lift the WARN bar a notch.
				cells[idx].Warn++
			}
		}
	}
	return SparklineData{Buckets: cells, TotalEvents: total, StartTime: start, EndTime: end}
}

// recentTransitions returns the last `n` watch events captured anywhere in
// the archive (across all paths), most recent first.
func recentTransitions(store *server.CaptureStore, at time.Time, n int) []Transition {
	type entry struct {
		t         time.Time
		eventType string
		path      string
		seq       int
	}
	var all []entry
	for path, wi := range store.WatchIndex {
		if wi == nil {
			continue
		}
		for i, t := range wi.Times {
			if !at.IsZero() && t.After(at) {
				continue
			}
			et := ""
			if i < len(wi.EventTypes) {
				et = wi.EventTypes[i]
			}
			seq := i
			if i < len(wi.Seqs) {
				seq = wi.Seqs[i]
			}
			all = append(all, entry{t: t, eventType: et, path: path, seq: seq})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.After(all[j].t) })
	if len(all) > n {
		all = all[:n]
	}
	out := make([]Transition, 0, len(all))
	for _, e := range all {
		_, _, resource, ns := parseAPIPath(e.path)
		t := Transition{
			Time:      e.t.UTC().Format(time.RFC3339),
			EventType: e.eventType,
			Kind:      kindFromResource(resource),
			Namespace: ns,
		}
		// Cheap read of the event body to pull the object name.
		rec, err := store.ReadRecord(e.path, e.seq)
		if err == nil {
			t.Name = getName(rec.ResponseBody)
		}
		out = append(out, t)
	}
	return out
}

// buildResourceTiles converts the resource→count map into a sorted slice of
// tiles for the dashboard's "Resources captured" card. Pods/Deployments/
// Services/ConfigMaps appear in a familiar order when present; the remainder
// is sorted by count desc, with a "+N more" tail-tile if necessary.
func buildResourceTiles(resourceTotals map[string]int) []ResourceTile {
	if len(resourceTotals) == 0 {
		return nil
	}
	type kv struct {
		res, kind string
		count     int
	}
	var rest []kv
	preferred := []string{"deployments", "pods", "services", "configmaps", "secrets", "persistentvolumeclaims", "customresourcedefinitions", "virtualmachines"}
	seen := map[string]bool{}
	out := make([]ResourceTile, 0, len(resourceTotals))
	for _, res := range preferred {
		if c, ok := resourceTotals[res]; ok && c > 0 {
			out = append(out, ResourceTile{Kind: kindFromResource(res), Count: c, Link: "#/namespaces"})
			seen[res] = true
		}
	}
	for res, c := range resourceTotals {
		if seen[res] || c <= 0 {
			continue
		}
		rest = append(rest, kv{res, kindFromResource(res), c})
	}
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].count != rest[j].count {
			return rest[i].count > rest[j].count
		}
		return rest[i].res < rest[j].res
	})
	// Show the next 4 explicitly then collapse the rest into a "+N more" tile.
	const showRest = 4
	for i, r := range rest {
		if i >= showRest {
			break
		}
		out = append(out, ResourceTile{Kind: r.kind, Count: r.count, Link: "#/namespaces"})
	}
	if len(rest) > showRest {
		out = append(out, ResourceTile{Kind: fmt.Sprintf("+ %d more…", len(rest)-showRest), Count: 0, Link: "#/namespaces"})
	}
	return out
}
