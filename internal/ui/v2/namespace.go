package v2

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// NamespaceDetail is the response from /v2/api/namespace?ns=… — everything
// the ns drill-down page needs.
type NamespaceDetail struct {
	Name      string            `json:"name"`
	At        string            `json:"at,omitempty"`
	Capture   CaptureMeta       `json:"capture"`
	KPIs      NamespaceKPIs     `json:"kpis"`
	Sparkline SparklineData     `json:"sparkline"`
	Issues    []Issue           `json:"issues"`
	Workloads []ResourceRow     `json:"workloads"`
	Pods      []PodRow          `json:"pods"`
	VMs       []ResourceRow     `json:"vms"`
	Resources []ResourceTile    `json:"resources"`
	Metadata  NamespaceMetadata `json:"metadata"`
}

type NamespaceKPIs struct {
	Workloads       int `json:"workloads"`
	Pods            int `json:"pods"`
	UnhealthyPods   int `json:"unhealthy_pods"`
	VirtualMachines int `json:"virtual_machines"`
	ConfigMaps      int `json:"configmaps"`
	Secrets         int `json:"secrets"`
	Resources       int `json:"resources"`
}

// ResourceRow is a compact row in the workloads / VMs lists.
type ResourceRow struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Restarts int    `json:"restarts,omitempty"`
	Age      string `json:"age,omitempty"`
	Link     string `json:"link,omitempty"`
}

// PodRow is the per-pod row in the namespace drill-down pods table.
type PodRow struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Status   string `json:"status"`
	Severity string `json:"severity"`
	Ready    string `json:"ready"`
	Restarts int    `json:"restarts"`
	Age      string `json:"age,omitempty"`
	Link     string `json:"link"`
}

type NamespaceMetadata struct {
	CreatedAt   string            `json:"created_at,omitempty"`
	AgeHuman    string            `json:"age,omitempty"`
	Phase       string            `json:"phase,omitempty"` // Active / Terminating
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func (h *Handler) serveNamespace(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("ns")
	if ns == "" {
		writeError(w, http.StatusBadRequest, "missing ns query parameter")
		return
	}
	at := h.resolveAt(r)
	d, err := h.buildNamespaceDetail(ns, at)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !at.IsZero() {
		d.At = at.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) buildNamespaceDetail(ns string, at time.Time) (*NamespaceDetail, error) {
	store := h.Store
	if store == nil {
		return nil, fmt.Errorf("store not initialized")
	}

	d := &NamespaceDetail{
		Name: ns,
		Capture: CaptureMeta{
			CaptureID:         store.Metadata.CaptureID,
			CapturedAt:        store.Metadata.CapturedAt,
			CapturedUntil:     store.Metadata.CapturedUntil,
			KubernetesVersion: store.Metadata.KubernetesVersion,
			ServerAddress:     store.Metadata.ServerAddress,
			RecordCount:       store.Metadata.RecordCount,
		},
	}

	// Per-resource counts scoped to this ns (no body reads).
	byRes := store.NamespaceItemCountsAt(at)[ns]
	resTotal := map[string]int{}
	for res, n := range byRes {
		resTotal[res] = n
		d.KPIs.Resources += n
		switch {
		case workloadResources[res]:
			d.KPIs.Workloads += n
		case res == "pods":
			d.KPIs.Pods += n
		case vmResources[res]:
			d.KPIs.VirtualMachines += n
		case res == "configmaps":
			d.KPIs.ConfigMaps += n
		case res == "secrets":
			d.KPIs.Secrets += n
		}
	}

	// Load pods, classify, fill PodRow list + Unhealthy counter + Issues.
	pods, podRows := h.loadPodsForNS(ns, at)
	for _, p := range pods {
		if !p.health.IsHealthy() {
			d.KPIs.UnhealthyPods++
		}
	}
	sort.SliceStable(podRows, func(i, j int) bool {
		// Failing pods float to the top, otherwise alphabetical.
		if podRows[i].Severity != podRows[j].Severity {
			return podSeverityRank(podRows[i].Severity) < podSeverityRank(podRows[j].Severity)
		}
		return podRows[i].Name < podRows[j].Name
	})
	d.Pods = podRows
	d.Issues = buildIssues(pods, 8)

	// Workloads + VMs (read the per-ns list bodies and turn into compact rows).
	d.Workloads = h.loadWorkloadRowsForNS(ns, at)
	d.VMs = h.loadVMRowsForNS(ns, at)

	// Resource tiles: every non-workload, non-pod, non-vm resource.
	d.Resources = buildNamespaceTiles(resTotal)

	// Pod-state sparkline scoped to this namespace.
	d.Sparkline = h.sparklineForNS(ns, sparkBucketCount)

	// Namespace metadata.
	d.Metadata = h.loadNamespaceMetadata(ns, at)

	return d, nil
}

// loadPodsForNS reads pods in the given namespace and returns both the
// internal podHealthEntry slice (for issue grouping) and a UI-shaped PodRow
// slice (for the drill-down table).
func (h *Handler) loadPodsForNS(ns string, at time.Time) ([]podHealthEntry, []PodRow) {
	listPath := "/api/v1/namespaces/" + ns + "/pods"
	body, code, err := h.Store.ReconstructAt(listPath, at)
	if err != nil || code != http.StatusOK || len(body) == 0 {
		return nil, nil
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, nil
	}
	var (
		entries []podHealthEntry
		rows    []PodRow
	)
	for _, raw := range list.Items {
		name := getName(raw)
		if name == "" {
			continue
		}
		ph := ClassifyPod(raw)
		entries = append(entries, podHealthEntry{namespace: ns, name: name, prefix: PodNamePrefix(name), health: ph})

		sev, status := podSeverityAndStatus(ph)
		rows = append(rows, PodRow{
			Name:     name,
			Phase:    ph.Phase,
			Status:   status,
			Severity: sev,
			Ready:    fmt.Sprintf("%d/%d", ph.Ready, ph.Total),
			Restarts: ph.Restarts,
			Age:      humanAge(getCreationTimestamp(raw), at),
			Link:     podLink(ns, name),
		})
	}
	return entries, rows
}

func podSeverityAndStatus(ph PodHealth) (string, string) {
	if len(ph.Issues) > 0 {
		// Pick the first issue as the human-readable reason.
		return "bad", ph.Issues[0]
	}
	if ph.Phase == "Running" && ph.Ready < ph.Total {
		return "warn", "NotReady"
	}
	if ph.Phase == "Pending" {
		return "warn", "Pending"
	}
	return "good", ph.Phase
}

func podSeverityRank(sev string) int {
	switch sev {
	case "bad":
		return 0
	case "warn":
		return 1
	default:
		return 2
	}
}

// loadWorkloadRowsForNS walks the workload resource paths for this ns and
// builds a compact row per item.
func (h *Handler) loadWorkloadRowsForNS(ns string, at time.Time) []ResourceRow {
	groups := []struct {
		path  string
		kind  string
		short string // short label shown in the row
	}{
		{"/apis/apps/v1/namespaces/" + ns + "/deployments", "Deployment", "Deploy"},
		{"/apis/apps/v1/namespaces/" + ns + "/statefulsets", "StatefulSet", "SS"},
		{"/apis/apps/v1/namespaces/" + ns + "/daemonsets", "DaemonSet", "DS"},
		{"/apis/apps/v1/namespaces/" + ns + "/replicasets", "ReplicaSet", "RS"},
		{"/apis/batch/v1/namespaces/" + ns + "/jobs", "Job", "Job"},
		{"/apis/batch/v1/namespaces/" + ns + "/cronjobs", "CronJob", "CronJob"},
	}
	var out []ResourceRow
	for _, g := range groups {
		out = append(out, h.loadWorkloadGroup(g.path, g.kind, g.short, at)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return podSeverityRank(out[i].Severity) < podSeverityRank(out[j].Severity)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (h *Handler) loadWorkloadGroup(path, kind, short string, at time.Time) []ResourceRow {
	body, code, err := h.Store.ReconstructAt(path, at)
	if err != nil || code != http.StatusOK || len(body) == 0 {
		return nil
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil
	}
	out := make([]ResourceRow, 0, len(list.Items))
	for _, raw := range list.Items {
		out = append(out, classifyWorkload(short, kind, raw, at))
	}
	return out
}

// classifyWorkload reads spec.replicas + status.{readyReplicas,…} and turns
// the result into a single human-readable row.
func classifyWorkload(short, kind string, raw json.RawMessage, at time.Time) ResourceRow {
	var w struct {
		Metadata struct {
			Name              string    `json:"name"`
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			Replicas               int `json:"replicas"`
			ReadyReplicas          int `json:"readyReplicas"`
			AvailableReplicas      int `json:"availableReplicas"`
			NumberReady            int `json:"numberReady"`
			DesiredNumberScheduled int `json:"desiredNumberScheduled"`
			Succeeded              int `json:"succeeded"`
			Active                 int `json:"active"`
			Failed                 int `json:"failed"`
		} `json:"status"`
	}
	_ = json.Unmarshal(raw, &w)

	row := ResourceRow{
		Kind:     short,
		Name:     w.Metadata.Name,
		Age:      humanAge(w.Metadata.CreationTimestamp, at),
		Severity: "good",
	}
	switch kind {
	case "Deployment", "StatefulSet", "ReplicaSet":
		desired := 0
		if w.Spec.Replicas != nil {
			desired = *w.Spec.Replicas
		}
		row.Status = fmt.Sprintf("%d/%d Ready", w.Status.ReadyReplicas, desired)
		row.Severity = readinessSeverity(w.Status.ReadyReplicas, desired)
	case "DaemonSet":
		row.Status = fmt.Sprintf("%d/%d Ready", w.Status.NumberReady, w.Status.DesiredNumberScheduled)
		row.Severity = readinessSeverity(w.Status.NumberReady, w.Status.DesiredNumberScheduled)
	case "Job":
		if w.Status.Failed > 0 {
			row.Status = fmt.Sprintf("Failed: %d", w.Status.Failed)
			row.Severity = "bad"
		} else if w.Status.Active > 0 {
			row.Status = fmt.Sprintf("Active: %d", w.Status.Active)
			row.Severity = "warn"
		} else {
			row.Status = fmt.Sprintf("Succeeded: %d", w.Status.Succeeded)
		}
	case "CronJob":
		row.Status = "Scheduled"
	}
	return row
}

func readinessSeverity(ready, desired int) string {
	if desired == 0 {
		return "neutral"
	}
	if ready == 0 {
		return "bad"
	}
	if ready < desired {
		return "warn"
	}
	return "good"
}

// loadVMRowsForNS reads kubevirt VirtualMachine + VirtualMachineInstance
// lists when present.
func (h *Handler) loadVMRowsForNS(ns string, at time.Time) []ResourceRow {
	groups := []struct {
		path, kind, short string
	}{
		{"/apis/kubevirt.io/v1/namespaces/" + ns + "/virtualmachines", "VirtualMachine", "VM"},
		{"/apis/kubevirt.io/v1/namespaces/" + ns + "/virtualmachineinstances", "VirtualMachineInstance", "VMI"},
	}
	var out []ResourceRow
	for _, g := range groups {
		body, code, err := h.Store.ReconstructAt(g.path, at)
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
			var v struct {
				Metadata struct {
					Name              string    `json:"name"`
					CreationTimestamp time.Time `json:"creationTimestamp"`
				} `json:"metadata"`
				Status struct {
					PrintableStatus string `json:"printableStatus"`
					Phase           string `json:"phase"`
				} `json:"status"`
			}
			_ = json.Unmarshal(raw, &v)
			status := v.Status.PrintableStatus
			if status == "" {
				status = v.Status.Phase
			}
			sev := "good"
			if status != "" && status != "Running" {
				sev = "warn"
			}
			out = append(out, ResourceRow{
				Kind:     g.short,
				Name:     v.Metadata.Name,
				Status:   status,
				Severity: sev,
				Age:      humanAge(v.Metadata.CreationTimestamp, at),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// loadNamespaceMetadata reads the namespace object from /api/v1/namespaces
// (the cluster-wide list) and surfaces labels/annotations/age/phase.
func (h *Handler) loadNamespaceMetadata(ns string, at time.Time) NamespaceMetadata {
	body, code, err := h.Store.ReconstructAt("/api/v1/namespaces", at)
	if err != nil || code != http.StatusOK || len(body) == 0 {
		return NamespaceMetadata{}
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return NamespaceMetadata{}
	}
	for _, raw := range list.Items {
		var n struct {
			Metadata struct {
				Name              string            `json:"name"`
				CreationTimestamp time.Time         `json:"creationTimestamp"`
				Labels            map[string]string `json:"labels"`
				Annotations       map[string]string `json:"annotations"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			continue
		}
		if n.Metadata.Name != ns {
			continue
		}
		return NamespaceMetadata{
			CreatedAt:   n.Metadata.CreationTimestamp.UTC().Format(time.RFC3339),
			AgeHuman:    humanAge(n.Metadata.CreationTimestamp, at),
			Phase:       n.Status.Phase,
			Labels:      n.Metadata.Labels,
			Annotations: n.Metadata.Annotations,
		}
	}
	return NamespaceMetadata{}
}

// buildNamespaceTiles drops the workload/pod/vm resources (rendered as
// separate cards) and turns the rest of the per-ns counts into tiles.
func buildNamespaceTiles(byRes map[string]int) []ResourceTile {
	out := make([]ResourceTile, 0, len(byRes))
	for res, n := range byRes {
		if n <= 0 {
			continue
		}
		if workloadResources[res] || vmResources[res] || res == "pods" {
			continue
		}
		out = append(out, ResourceTile{
			Kind:  kindFromResource(res),
			Count: n,
			Link:  "#/namespaces",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// humanAge converts a creationTimestamp + reference time into a short human
// duration string like "14d" or "3h12m". Returns "" for zero timestamps.
func humanAge(created, ref time.Time) string {
	if created.IsZero() {
		return ""
	}
	if ref.IsZero() {
		ref = time.Now().UTC()
	}
	d := ref.Sub(created)
	if d < 0 {
		return "0s"
	}
	switch {
	case d > 48*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d > 1*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	case d > 1*time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

// getCreationTimestamp parses metadata.creationTimestamp from a raw object.
func getCreationTimestamp(raw json.RawMessage) time.Time {
	var m struct {
		Metadata struct {
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return time.Time{}
	}
	return m.Metadata.CreationTimestamp
}

// buildSparklineForNS gets the real implementation now that the file
// compiles with a placeholder. Replace inline above to keep imports tidy.
//
// We need access to the store to walk its WatchIndex. Hoist the func to
// Handler so we can do that.
func (h *Handler) sparklineForNS(ns string, buckets int) SparklineData {
	store := h.Store
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
	prefix := "/api/v1/namespaces/" + ns + "/"
	apisPrefix := "/apis/"
	apisNsSegment := "/namespaces/" + ns + "/"
	total := 0
	for path, wi := range store.WatchIndex {
		if wi == nil {
			continue
		}
		isThisNS := strings.HasPrefix(path, prefix) ||
			(strings.HasPrefix(path, apisPrefix) && strings.Contains(path, apisNsSegment))
		if !isThisNS {
			continue
		}
		isPodPath := strings.Contains(path, "/pods")
		for i, t := range wi.Times {
			total++
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
				cells[idx].Warn++
			}
		}
	}
	return SparklineData{Buckets: cells, TotalEvents: total, StartTime: start, EndTime: end}
}
