package v2

import "time"

// CaptureMeta is the small slice of metadata the UI shows in the top bar.
type CaptureMeta struct {
	CaptureID         string    `json:"capture_id"`
	CapturedAt        time.Time `json:"captured_at"`
	CapturedUntil     time.Time `json:"captured_until"`
	KubernetesVersion string    `json:"kubernetes_version,omitempty"`
	ServerAddress     string    `json:"server_address,omitempty"`
	RecordCount       int       `json:"record_count"`
}

// Overview is the response from /v2/api/overview — everything the dashboard
// landing page needs.
type Overview struct {
	Capture       CaptureMeta        `json:"capture"`
	At            string             `json:"at,omitempty"`
	KPIs          KPIs               `json:"kpis"`
	Sparkline     SparklineData      `json:"sparkline"`
	Issues        []Issue            `json:"issues"`
	Resources     []ResourceTile     `json:"resources"`
	Namespaces    []NamespaceSummary `json:"namespaces"`
	TopNamespaces []NamespaceSummary `json:"top_namespaces"`
	Recent        []Transition       `json:"recent"`
}

// KPIs are the top-of-dashboard counters.
type KPIs struct {
	Namespaces       int `json:"namespaces"`
	Workloads        int `json:"workloads"`
	Pods             int `json:"pods"`
	UnhealthyPods    int `json:"unhealthy_pods"`
	WatchEvents      int `json:"watch_events"`
	CrashLoopBackOff int `json:"crash_loop_back_off"`
	Failed           int `json:"failed"`
	Pending          int `json:"pending"`
	OOMKilled        int `json:"oom_killed"`
	VirtualMachines  int `json:"virtual_machines"`
}

// SparklineData powers the pod-state-transitions chart on the overview.
type SparklineData struct {
	Buckets     []SparkBucket `json:"buckets"`
	TotalEvents int           `json:"total_events"`
	StartTime   time.Time     `json:"start_time"`
	EndTime     time.Time     `json:"end_time"`
}

// SparkBucket is one column in the sparkline — counts of watch events in
// a fixed time window, split by severity.
type SparkBucket struct {
	Time  time.Time `json:"time"`
	Total int       `json:"total"`
	Warn  int       `json:"warn"`
	Bad   int       `json:"bad"`
}

// Issue is one row in the "Issues to investigate" panel. Pods with the
// same failure mode and the same name prefix are grouped (Count > 1).
type Issue struct {
	Severity  string `json:"severity"` // "bad" | "warn"
	Kind      string `json:"kind"`     // short resource kind ("Pod", "Deploy", "DS", …)
	Title     string `json:"title"`
	Subtitle  string `json:"subtitle"`
	Namespace string `json:"namespace,omitempty"`
	Count     int    `json:"count,omitempty"` // ≥2 when grouped
	AgeHuman  string `json:"age,omitempty"`
	Link      string `json:"link,omitempty"` // hash route the UI navigates to on click
}

// ResourceTile is one of the small cards in the "Resources captured" grid.
type ResourceTile struct {
	Kind     string `json:"kind"`
	Resource string `json:"resource,omitempty"`
	Count    int    `json:"count"`
	Link     string `json:"link"`
}

// NamespaceSummary is a single row in the top-namespaces list and also the
// shape returned from the Namespaces tab.
type NamespaceSummary struct {
	Name      string `json:"name"`
	Resources int    `json:"resources"`
	Workloads int    `json:"workloads"`
	Pods      int    `json:"pods"`
	Unhealthy int    `json:"unhealthy,omitempty"`
}

// Transition is a single recent watch event used by the "Recent transitions"
// panel on the overview.
type Transition struct {
	Time      string `json:"time"`
	EventType string `json:"event_type"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Detail    string `json:"detail,omitempty"`
	// Path is the watch event's API list path (e.g. /api/v1/namespaces/x/pods)
	// and Resource its plural name; the UI uses them to link a transition row
	// to the pod detail or the generic object view.
	Path     string `json:"path,omitempty"`
	Resource string `json:"resource,omitempty"`
}
