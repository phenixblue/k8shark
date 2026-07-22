package v2

import (
	"fmt"
	"net/http"
	"sort"
	"time"
)

// ClusterPodRow is a single pod in the cluster-wide pods list. It is PodRow
// plus the namespace (and an Unhealthy flag matching the overview's
// unhealthy_pods KPI definition) so the list can be filtered client-side.
type ClusterPodRow struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Phase     string            `json:"phase"`
	Status    string            `json:"status"`
	Severity  string            `json:"severity"`
	Ready     string            `json:"ready"`
	Restarts  int               `json:"restarts"`
	Age       string            `json:"age,omitempty"`
	Link      string            `json:"link"`
	Unhealthy bool              `json:"unhealthy"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// ClusterWorkloadRow is a single workload in the cluster-wide workloads list:
// ResourceRow plus its namespace.
type ClusterWorkloadRow struct {
	Namespace string            `json:"namespace"`
	Kind      string            `json:"kind"`
	Resource  string            `json:"resource"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Severity  string            `json:"severity"`
	Restarts  int               `json:"restarts,omitempty"`
	Age       string            `json:"age,omitempty"`
	Link      string            `json:"link,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// PodsList is the response from /v2/api/pods.
type PodsList struct {
	At        string          `json:"at,omitempty"`
	Capture   CaptureMeta     `json:"capture"`
	Total     int             `json:"total"`
	Unhealthy int             `json:"unhealthy"`
	Pods      []ClusterPodRow `json:"pods"`
}

// WorkloadsList is the response from /v2/api/workloads.
type WorkloadsList struct {
	At        string               `json:"at,omitempty"`
	Capture   CaptureMeta          `json:"capture"`
	Total     int                  `json:"total"`
	Workloads []ClusterWorkloadRow `json:"workloads"`
}

func (h *Handler) captureMeta() CaptureMeta {
	m := h.Store.Metadata
	return CaptureMeta{
		CaptureID:         m.CaptureID,
		CapturedAt:        m.CapturedAt,
		CapturedUntil:     m.CapturedUntil,
		KubernetesVersion: m.KubernetesVersion,
		ServerAddress:     m.ServerAddress,
		RecordCount:       m.RecordCount,
	}
}

// serveAllPods returns every pod across all namespaces at the resolved time,
// sorted unhealthy-first. The frontend filters by namespace / health.
func (h *Handler) serveAllPods(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	at := h.resolveAt(r)
	rows, unhealthy := h.loadAllPodRows(at)
	resp := PodsList{
		Capture:   h.captureMeta(),
		Total:     len(rows),
		Unhealthy: unhealthy,
		Pods:      rows,
	}
	if !at.IsZero() {
		resp.At = at.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// serveAllWorkloads returns every workload (Deployment/StatefulSet/DaemonSet/
// ReplicaSet/Job/CronJob) across all namespaces at the resolved time.
func (h *Handler) serveAllWorkloads(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	at := h.resolveAt(r)
	rows := h.loadAllWorkloadRows(at)
	resp := WorkloadsList{
		Capture:   h.captureMeta(),
		Total:     len(rows),
		Workloads: rows,
	}
	if !at.IsZero() {
		resp.At = at.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// loadAllPodRows walks every per-namespace pod LIST in the index (plus any
// overlay-only namespace) and builds a flat, sorted slice of rows. Returns the
// rows and the count of unhealthy pods (matching the overview unhealthy_pods
// KPI: pods whose health has issues).
func (h *Handler) loadAllPodRows(at time.Time) ([]ClusterPodRow, int) {
	var rows []ClusterPodRow
	unhealthy := 0
	for _, path := range h.resourcePathsFor("pods") {
		_, _, _, ns := parseAPIPath(path)
		if ns == "" {
			continue
		}
		for _, raw := range h.reconstructMergedItems(path, at) {
			name := getName(raw)
			if name == "" {
				continue
			}
			ph := ClassifyPod(raw)
			sev, status := podSeverityAndStatus(ph)
			uh := !ph.IsHealthy()
			if uh {
				unhealthy++
			}
			rows = append(rows, ClusterPodRow{
				Namespace: ns,
				Name:      name,
				Phase:     ph.Phase,
				Status:    status,
				Severity:  sev,
				Ready:     fmt.Sprintf("%d/%d", ph.Ready, ph.Total),
				Restarts:  ph.Restarts,
				Age:       humanAge(getCreationTimestamp(raw), at),
				Link:      podLink(ns, name),
				Unhealthy: uh,
				Labels:    getLabels(raw),
			})
		}
	}
	sortClusterRows(rows)
	return rows, unhealthy
}

// loadAllWorkloadRows walks every per-namespace workload LIST in the index
// (plus any overlay-only namespace) and builds a flat, sorted slice of rows.
func (h *Handler) loadAllWorkloadRows(at time.Time) []ClusterWorkloadRow {
	// resource → (full kind, short label) for the workload kinds we surface.
	kinds := map[string]struct{ kind, short string }{
		"deployments":  {"Deployment", "Deploy"},
		"statefulsets": {"StatefulSet", "SS"},
		"daemonsets":   {"DaemonSet", "DS"},
		"replicasets":  {"ReplicaSet", "RS"},
		"jobs":         {"Job", "Job"},
		"cronjobs":     {"CronJob", "CronJob"},
	}
	var rows []ClusterWorkloadRow
	for resource, k := range kinds {
		for _, path := range h.resourcePathsFor(resource) {
			_, _, _, ns := parseAPIPath(path)
			if ns == "" {
				continue
			}
			for _, raw := range h.reconstructMergedItems(path, at) {
				row := classifyWorkload(k.short, k.kind, raw, at)
				rows = append(rows, ClusterWorkloadRow{
					Namespace: ns,
					Kind:      k.kind,
					Resource:  resource,
					Name:      row.Name,
					Status:    row.Status,
					Severity:  row.Severity,
					Restarts:  row.Restarts,
					Age:       row.Age,
					Link:      "#/ns/" + escapeHash(ns),
					Labels:    getLabels(raw),
				})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Severity != rows[j].Severity {
			return podSeverityRank(rows[i].Severity) < podSeverityRank(rows[j].Severity)
		}
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].Name < rows[j].Name
	})
	return rows
}

func sortClusterRows(rows []ClusterPodRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Severity != rows[j].Severity {
			return podSeverityRank(rows[i].Severity) < podSeverityRank(rows[j].Severity)
		}
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].Name < rows[j].Name
	})
}
