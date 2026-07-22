package v2

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// PodDetail is what /v2/api/pod?ns=…&name=… returns — everything the pod
// drill-down view shows.
type PodDetail struct {
	Name             string             `json:"name"`
	Namespace        string             `json:"namespace"`
	At               string             `json:"at,omitempty"`
	Capture          CaptureMeta        `json:"capture"`
	Hero             PodHero            `json:"hero"`
	KPIs             PodKPIs            `json:"kpis"`
	Containers       []PodContainerCard `json:"containers"`
	Events           []PodEvent         `json:"events"`
	History          []PodTransition    `json:"history"`
	Related          PodRelated         `json:"related"`
	Metadata         PodMetadata        `json:"metadata"`
	RestartSparkline []SparkBucket      `json:"restart_sparkline"`
}

type PodHero struct {
	Phase    string `json:"phase"`
	Severity string `json:"severity"`
	Reason   string `json:"reason,omitempty"`
	Subtitle string `json:"subtitle"`
}

type PodKPIs struct {
	Phase        string `json:"phase"`
	Restarts     int    `json:"restarts"`
	AgeHuman     string `json:"age"`
	Ready        string `json:"ready"`
	Node         string `json:"node,omitempty"`
	PodIP        string `json:"pod_ip,omitempty"`
	QoSClass     string `json:"qos_class,omitempty"`
	UnreadyCause string `json:"unready_cause,omitempty"`
}

type PodContainerCard struct {
	Name           string             `json:"name"`
	Role           string             `json:"role"` // "main" | "init" | "side"
	Image          string             `json:"image,omitempty"`
	State          string             `json:"state"`
	StateBadge     string             `json:"state_badge"`
	Severity       string             `json:"severity"`
	RestartCount   int                `json:"restart_count"`
	StartedAt      string             `json:"started_at,omitempty"`
	LastTerminated string             `json:"last_terminated,omitempty"`
	LastExitCode   int                `json:"last_exit_code,omitempty"`
	Resources      ContainerResources `json:"resources"`
	Probes         []string           `json:"probes,omitempty"`
	Ports          []string           `json:"ports,omitempty"`
	LogPreview     []string           `json:"log_preview,omitempty"`
	LogPath        string             `json:"log_path,omitempty"`
	HasPreviousLog bool               `json:"has_previous_log,omitempty"`
}

type ContainerResources struct {
	CPURequest    string `json:"cpu_request,omitempty"`
	CPULimit      string `json:"cpu_limit,omitempty"`
	MemoryRequest string `json:"memory_request,omitempty"`
	MemoryLimit   string `json:"memory_limit,omitempty"`
}

type PodEvent struct {
	Severity string `json:"severity"`
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	Source   string `json:"source,omitempty"`
	Time     string `json:"time"`
	Count    int    `json:"count,omitempty"`
}

type PodTransition struct {
	Time      string `json:"time"`
	EventType string `json:"event_type"`
	Detail    string `json:"detail,omitempty"`
}

type PodRelated struct {
	Owner       *RelatedItem  `json:"owner,omitempty"`
	Workload    *RelatedItem  `json:"workload,omitempty"`
	SiblingPods int           `json:"sibling_pods"`
	ConfigMaps  []RelatedItem `json:"config_maps,omitempty"`
	Secrets     []RelatedItem `json:"secrets,omitempty"`
	PVCs        []RelatedItem `json:"pvcs,omitempty"`
}

type RelatedItem struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Link string `json:"link,omitempty"`
}

type PodMetadata struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Conditions  []PodCondition    `json:"conditions,omitempty"`
	CreatedAt   string            `json:"created_at,omitempty"`
}

type PodCondition struct {
	Type     string `json:"type"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Severity string `json:"severity"`
}

func (h *Handler) servePod(w http.ResponseWriter, r *http.Request) {
	ns := r.URL.Query().Get("ns")
	name := r.URL.Query().Get("name")
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "missing ns or name query parameter")
		return
	}
	at := h.resolveAt(r)
	d, err := h.buildPodDetail(ns, name, at)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d == nil {
		writeError(w, http.StatusNotFound, "pod not found in capture")
		return
	}
	if !at.IsZero() {
		d.At = at.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, d)
}

// buildPodDetail reads the pod object + per-container logs + events + watch
// history and assembles them into a single PodDetail blob.
func (h *Handler) buildPodDetail(ns, name string, at time.Time) (*PodDetail, error) {
	store := h.Store
	if store == nil {
		return nil, fmt.Errorf("store not initialized")
	}

	// Find the pod's raw body inside the pods list, merged with any overlay write.
	listPath := "/api/v1/namespaces/" + ns + "/pods"
	var raw json.RawMessage
	for _, it := range h.reconstructMergedItems(listPath, at) {
		if getName(it) == name {
			raw = it
			break
		}
	}
	if raw == nil {
		return nil, nil
	}

	d := &PodDetail{
		Name:      name,
		Namespace: ns,
		Capture: CaptureMeta{
			CaptureID:         store.Metadata.CaptureID,
			CapturedAt:        store.Metadata.CapturedAt,
			CapturedUntil:     store.Metadata.CapturedUntil,
			KubernetesVersion: store.Metadata.KubernetesVersion,
			ServerAddress:     store.Metadata.ServerAddress,
			RecordCount:       store.Metadata.RecordCount,
		},
	}

	var pod podObject
	_ = json.Unmarshal(raw, &pod)

	ph := ClassifyPod(raw)
	d.Hero = buildPodHero(ph)

	// KPIs
	d.KPIs = PodKPIs{
		Phase:    ph.Phase,
		Restarts: ph.Restarts,
		AgeHuman: humanAge(pod.Metadata.CreationTimestamp, at),
		Ready:    fmt.Sprintf("%d / %d", ph.Ready, ph.Total),
		Node:     pod.Spec.NodeName,
		PodIP:    pod.Status.PodIP,
		QoSClass: pod.Status.QoSClass,
	}

	// Container cards (init first, main next).
	d.Containers = buildContainerCards(pod, ph, ns, name, h)

	// Events for this pod.
	d.Events = h.loadPodEvents(ns, name, at, 10)

	// Watch-event history for this pod (filtered by name).
	d.History = h.loadPodHistory(ns, name, at, 20)

	// Related resources.
	d.Related = buildRelated(pod, ns, name, h.Store, at)

	// Metadata + conditions.
	d.Metadata = buildPodMetadata(pod)

	// Restart sparkline: bucket the pod's MODIFIED watch events.
	d.RestartSparkline = h.podRestartSparkline(ns, name, sparkBucketCount)

	return d, nil
}

func buildPodHero(ph PodHealth) PodHero {
	hero := PodHero{Phase: ph.Phase, Severity: "good"}
	if !ph.IsHealthy() {
		hero.Severity = "bad"
		hero.Reason = ph.Issues[0]
	} else if ph.Ready < ph.Total {
		hero.Severity = "warn"
		hero.Reason = "Some containers not ready"
	}
	if hero.Reason == "" {
		hero.Subtitle = fmt.Sprintf("%d/%d containers ready · %d restarts", ph.Ready, ph.Total, ph.Restarts)
	} else {
		hero.Subtitle = fmt.Sprintf("%s · %d restarts", hero.Reason, ph.Restarts)
	}
	return hero
}

// containerSpecByName maps a container name to its spec, used to enrich the
// PodContainerCard with image / resources / probes / ports.
type containerSpecByName map[string]containerSpec

func buildContainerCards(pod podObject, ph PodHealth, ns, name string, h *Handler) []PodContainerCard {
	specs := make(containerSpecByName)
	for _, c := range pod.Spec.InitContainers {
		specs[c.Name] = c
	}
	for _, c := range pod.Spec.Containers {
		specs[c.Name] = c
	}

	cards := make([]PodContainerCard, 0, len(ph.Containers))
	for _, c := range ph.Containers {
		card := PodContainerCard{
			Name:           c.Name,
			Role:           containerRole(c),
			Image:          c.Image,
			State:          c.State,
			Severity:       containerSeverity(c),
			RestartCount:   c.RestartCount,
			LastTerminated: c.LastTerminated,
			LastExitCode:   c.LastExitCode,
		}
		card.StateBadge = containerStateBadge(c)
		if spec, ok := specs[c.Name]; ok {
			card.Image = firstNonEmpty(card.Image, spec.Image)
			card.Resources = ContainerResources{
				CPURequest:    spec.Resources.Requests["cpu"],
				CPULimit:      spec.Resources.Limits["cpu"],
				MemoryRequest: spec.Resources.Requests["memory"],
				MemoryLimit:   spec.Resources.Limits["memory"],
			}
			card.Probes = describeProbes(spec)
			card.Ports = describePorts(spec)
		}

		// Captured logs: read the per-container log records if present.
		logPath := "/api/v1/namespaces/" + ns + "/pods/" + name + "/log?container=" + c.Name
		previousPath := logPath + "&previous=true"
		card.LogPreview, _ = h.readLogTail(logPath, 8)
		_, card.HasPreviousLog = h.Store.Index[previousPath]
		card.LogPath = "#/logs?ns=" + ns + "&pod=" + name + "&container=" + c.Name

		cards = append(cards, card)
	}
	return cards
}

func containerRole(c ContainerHealth) string {
	if c.IsInit {
		return "init"
	}
	if isSidecarName(c.Name) {
		return "side"
	}
	return "main"
}

func isSidecarName(name string) bool {
	low := strings.ToLower(name)
	for _, hint := range []string{"istio-proxy", "linkerd-proxy", "sidecar", "envoy", "fluent", "vector", "log-router"} {
		if strings.Contains(low, hint) {
			return true
		}
	}
	return false
}

func containerSeverity(c ContainerHealth) string {
	if c.State == "Waiting" {
		switch c.StateReason {
		case "CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerError", "CreateContainerConfigError":
			return "bad"
		}
		return "warn"
	}
	if c.State == "Terminated" && !c.Ready {
		if c.IsInit && c.LastExitCode == 0 {
			return "good"
		}
		return "bad"
	}
	if c.RestartCount > 0 {
		return "warn"
	}
	return "good"
}

func containerStateBadge(c ContainerHealth) string {
	switch c.State {
	case "Running":
		if c.RestartCount > 0 {
			return fmt.Sprintf("Running · %d restarts", c.RestartCount)
		}
		return "Running · 0 restarts"
	case "Waiting":
		if c.StateReason != "" {
			return fmt.Sprintf("%s · %d restarts", c.StateReason, c.RestartCount)
		}
		return fmt.Sprintf("Waiting · %d restarts", c.RestartCount)
	case "Terminated":
		if c.StateReason != "" {
			return fmt.Sprintf("Terminated · %s · exit %d", c.StateReason, c.LastExitCode)
		}
		return fmt.Sprintf("Terminated · exit %d", c.LastExitCode)
	}
	return "Unknown"
}

func describeProbes(spec containerSpec) []string {
	var out []string
	if spec.LivenessProbe != nil {
		out = append(out, "liveness "+probeDescription(*spec.LivenessProbe))
	}
	if spec.ReadinessProbe != nil {
		out = append(out, "readiness "+probeDescription(*spec.ReadinessProbe))
	}
	if spec.StartupProbe != nil {
		out = append(out, "startup "+probeDescription(*spec.StartupProbe))
	}
	return out
}

func probeDescription(p probe) string {
	switch {
	case p.HTTPGet != nil:
		return fmt.Sprintf("HTTP %s :%v%s", strings.ToUpper(firstNonEmpty(p.HTTPGet.Scheme, "GET")), p.HTTPGet.Port, p.HTTPGet.Path)
	case p.TCPSocket != nil:
		return fmt.Sprintf("TCP :%v", p.TCPSocket.Port)
	case p.Exec != nil:
		return "exec " + strings.Join(p.Exec.Command, " ")
	}
	return "configured"
}

func describePorts(spec containerSpec) []string {
	var out []string
	for _, p := range spec.Ports {
		if p.Name != "" {
			out = append(out, fmt.Sprintf("%d/%s (%s)", p.ContainerPort, firstNonEmpty(p.Protocol, "TCP"), p.Name))
		} else {
			out = append(out, fmt.Sprintf("%d/%s", p.ContainerPort, firstNonEmpty(p.Protocol, "TCP")))
		}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// readLogTail reads the captured log record (a JSON-encoded string) and
// returns the last `n` lines. Returns false when no log was captured.
func (h *Handler) readLogTail(indexKey string, n int) ([]string, bool) {
	body, code, err := h.Store.Latest(indexKey, h.At)
	if err != nil || code != http.StatusOK || len(body) == 0 {
		return nil, false
	}
	var text string
	if err := json.Unmarshal(body, &text); err != nil {
		// older archives stored raw text
		text = string(body)
	}
	if text == "" {
		return nil, false
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, true
}

// loadPodEvents scans captured Event resources for ones whose involvedObject
// matches this pod.
func (h *Handler) loadPodEvents(ns, name string, at time.Time, limit int) []PodEvent {
	listPath := "/api/v1/namespaces/" + ns + "/events"
	var out []PodEvent
	for _, raw := range h.reconstructMergedItems(listPath, at) {
		var ev eventObject
		if err := json.Unmarshal(raw, &ev); err != nil {
			continue
		}
		if ev.InvolvedObject.Kind != "Pod" || ev.InvolvedObject.Namespace != ns || ev.InvolvedObject.Name != name {
			continue
		}
		t := ev.LastTimestamp
		if t.IsZero() {
			t = ev.EventTime
		}
		if t.IsZero() {
			t = ev.FirstTimestamp
		}
		sev := "normal"
		if strings.EqualFold(ev.Type, "Warning") {
			sev = "warn"
		}
		if isBadEventReason(ev.Reason) {
			sev = "bad"
		}
		out = append(out, PodEvent{
			Severity: sev,
			Reason:   ev.Reason,
			Message:  ev.Message,
			Source:   firstNonEmpty(ev.Source.Component, ev.ReportingController),
			Time:     t.UTC().Format(time.RFC3339),
			Count:    ev.Count,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func isBadEventReason(reason string) bool {
	switch reason {
	case "BackOff", "Killing", "Killed", "Failed", "FailedScheduling", "FailedMount", "FailedCreate", "OOMKilled", "Evicted":
		return true
	}
	return false
}

// loadPodHistory walks WatchIndex entries for the pod list path and returns
// the watch events whose embedded object matches this pod.
func (h *Handler) loadPodHistory(ns, name string, at time.Time, limit int) []PodTransition {
	listPath := "/api/v1/namespaces/" + ns + "/pods"
	wi := h.Store.WatchIndex[listPath]
	if wi == nil {
		return nil
	}

	var out []PodTransition
	for i := range wi.Seqs {
		t := wi.Times[i]
		if !at.IsZero() && t.After(at) {
			continue
		}
		rec, err := h.Store.ReadRecord(listPath, wi.Seqs[i])
		if err != nil {
			continue
		}
		if getName(rec.ResponseBody) != name {
			continue
		}
		evType := ""
		if i < len(wi.EventTypes) {
			evType = wi.EventTypes[i]
		}
		ph := ClassifyPod(rec.ResponseBody)
		detail := ph.Phase
		if !ph.IsHealthy() {
			detail = ph.Issues[0]
		}
		out = append(out, PodTransition{
			Time:      t.UTC().Format(time.RFC3339),
			EventType: evType,
			Detail:    detail,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// podRestartSparkline bins MODIFIED watch events for this pod into a small
// time-bucketed sparkline.
func (h *Handler) podRestartSparkline(ns, name string, buckets int) []SparkBucket {
	store := h.Store
	start := store.Metadata.CapturedAt.UTC()
	end := store.Metadata.CapturedUntil.UTC()
	if !end.After(start) || buckets <= 0 {
		return nil
	}
	step := end.Sub(start) / time.Duration(buckets)
	if step <= 0 {
		step = time.Second
	}
	cells := make([]SparkBucket, buckets)
	for i := range cells {
		cells[i].Time = start.Add(time.Duration(i) * step)
	}

	listPath := "/api/v1/namespaces/" + ns + "/pods"
	wi := store.WatchIndex[listPath]
	if wi == nil {
		return cells
	}
	for i, t := range wi.Times {
		rec, err := store.ReadRecord(listPath, wi.Seqs[i])
		if err != nil {
			continue
		}
		if getName(rec.ResponseBody) != name {
			continue
		}
		idx := int(t.Sub(start) / step)
		if idx < 0 {
			idx = 0
		} else if idx >= buckets {
			idx = buckets - 1
		}
		cells[idx].Total++
		et := ""
		if i < len(wi.EventTypes) {
			et = wi.EventTypes[i]
		}
		if et == "DELETED" {
			cells[idx].Bad++
		} else if et == "MODIFIED" {
			cells[idx].Warn++
		}
	}
	return cells
}

// buildRelated walks the pod's ownerReferences + volumes to surface related
// objects that the user might want to click into next.
func buildRelated(pod podObject, ns, name string, store interface{}, _ time.Time) PodRelated {
	rel := PodRelated{}
	for _, o := range pod.Metadata.OwnerReferences {
		rel.Owner = &RelatedItem{Kind: o.Kind, Name: o.Name, Link: ownerLink(o, ns)}
		break
	}
	for _, v := range pod.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name != "" {
			rel.ConfigMaps = append(rel.ConfigMaps, RelatedItem{Kind: "ConfigMap", Name: v.ConfigMap.Name, Link: objectLink(apiListPath("", "v1", "configmaps", ns), v.ConfigMap.Name)})
		}
		if v.Secret != nil && v.Secret.SecretName != "" {
			rel.Secrets = append(rel.Secrets, RelatedItem{Kind: "Secret", Name: v.Secret.SecretName, Link: objectLink(apiListPath("", "v1", "secrets", ns), v.Secret.SecretName)})
		}
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != "" {
			rel.PVCs = append(rel.PVCs, RelatedItem{Kind: "PersistentVolumeClaim", Name: v.PersistentVolumeClaim.ClaimName, Link: objectLink(apiListPath("", "v1", "persistentvolumeclaims", ns), v.PersistentVolumeClaim.ClaimName)})
		}
	}
	// SiblingPods placeholder — would need to count pods with the same owner.
	return rel
}

func buildPodMetadata(pod podObject) PodMetadata {
	out := PodMetadata{
		Labels:      pod.Metadata.Labels,
		Annotations: pod.Metadata.Annotations,
		CreatedAt:   pod.Metadata.CreationTimestamp.UTC().Format(time.RFC3339),
	}
	for _, c := range pod.Status.Conditions {
		sev := "neutral"
		if c.Status == "True" {
			sev = "good"
		} else if c.Status == "False" {
			sev = "bad"
		}
		out.Conditions = append(out.Conditions, PodCondition{
			Type:     c.Type,
			Status:   c.Status,
			Reason:   c.Reason,
			Severity: sev,
		})
	}
	return out
}

// ── Pod object subset ───────────────────────────────────────────────────────

type podObject struct {
	Metadata podMetadataObj `json:"metadata"`
	Spec     struct {
		NodeName       string          `json:"nodeName"`
		Containers     []containerSpec `json:"containers"`
		InitContainers []containerSpec `json:"initContainers"`
		Volumes        []volume        `json:"volumes"`
	} `json:"spec"`
	Status struct {
		Phase      string         `json:"phase"`
		PodIP      string         `json:"podIP"`
		QoSClass   string         `json:"qosClass"`
		Conditions []podCondition `json:"conditions"`
	} `json:"status"`
}

type podMetadataObj struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	OwnerReferences   []ownerRef        `json:"ownerReferences"`
}

type ownerRef struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	APIVersion string `json:"apiVersion"`
}

type containerSpec struct {
	Name           string               `json:"name"`
	Image          string               `json:"image"`
	Ports          []containerPort      `json:"ports"`
	Resources      resourceRequirements `json:"resources"`
	LivenessProbe  *probe               `json:"livenessProbe,omitempty"`
	ReadinessProbe *probe               `json:"readinessProbe,omitempty"`
	StartupProbe   *probe               `json:"startupProbe,omitempty"`
}

type containerPort struct {
	Name          string `json:"name"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

type resourceRequirements struct {
	Requests map[string]string `json:"requests"`
	Limits   map[string]string `json:"limits"`
}

type probe struct {
	HTTPGet   *probeHTTPGet   `json:"httpGet,omitempty"`
	TCPSocket *probeTCPSocket `json:"tcpSocket,omitempty"`
	Exec      *probeExec      `json:"exec,omitempty"`
}

type probeHTTPGet struct {
	Path   string `json:"path"`
	Port   any    `json:"port"`
	Scheme string `json:"scheme"`
}

type probeTCPSocket struct {
	Port any `json:"port"`
}

type probeExec struct {
	Command []string `json:"command"`
}

type volume struct {
	Name                  string              `json:"name"`
	ConfigMap             *volumeConfigMap    `json:"configMap,omitempty"`
	Secret                *volumeSecret       `json:"secret,omitempty"`
	PersistentVolumeClaim *volumePersistentVC `json:"persistentVolumeClaim,omitempty"`
}

type volumeConfigMap struct {
	Name string `json:"name"`
}
type volumeSecret struct {
	SecretName string `json:"secretName"`
}
type volumePersistentVC struct {
	ClaimName string `json:"claimName"`
}

type podCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type eventObject struct {
	Reason         string    `json:"reason"`
	Message        string    `json:"message"`
	Type           string    `json:"type"`
	FirstTimestamp time.Time `json:"firstTimestamp"`
	LastTimestamp  time.Time `json:"lastTimestamp"`
	EventTime      time.Time `json:"eventTime"`
	Count          int       `json:"count"`
	Source         struct {
		Component string `json:"component"`
	} `json:"source"`
	ReportingController string `json:"reportingController"`
	InvolvedObject      struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		FieldPath string `json:"fieldPath"`
	} `json:"involvedObject"`
}
