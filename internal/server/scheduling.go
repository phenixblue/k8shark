package server

import (
	"encoding/json"
	"sort"
	"time"
)

// defaultSyntheticNode is the node synthesized for scheduling when the capture
// contains none.
const defaultSyntheticNode = "kwok-node-0"

// ensureSchedulableNode synthesizes a node when the capture has none, so a
// scheduling target — and a node for KWOK to manage — exists from startup rather
// than only appearing when the first Pod is created. Idempotent: synthesizeNode
// is a no-op when the node already exists.
//
// Node presence is evaluated at the window END, not the current clock instant.
// A capture's first /api/v1/nodes snapshot commonly lands a few seconds after
// the window start (from ≈ metadata.CapturedAt, an approximation), so checking
// at `from` would wrongly see "no nodes" at startup and synthesize kwok-node-0
// for a capture that actually contains nodes (#172).
func (h *handler) ensureSchedulableNode() {
	at := h.at
	if h.clock != nil {
		_, at = h.clock.Window()
	}
	if len(h.knownNodeNamesAt(at)) == 0 {
		h.synthesizeNode(defaultSyntheticNode)
	}
}

// schedulePod binds an unscheduled Pod to a node — the scheduler replay lacks —
// picking round-robin over the known nodes (captured + overlay) and synthesizing
// a KWOK-managed node if none exist. Returns the body with spec.nodeName set.
func (h *handler) schedulePod(body json.RawMessage) json.RawMessage {
	nodes := h.knownNodeNames()
	if len(nodes) == 0 {
		h.synthesizeNode(defaultSyntheticNode)
		nodes = []string{defaultSyntheticNode}
	}
	// Take the modulo in int64, then convert the bounded [0,len) result — casting
	// the raw counter to int first could overflow negative on a 32-bit platform.
	idx := h.overlay.nextScheduleIndex() % int64(len(nodes))
	return setSpecNodeName(body, nodes[int(idx)])
}

// knownNodeNames returns the sorted names of Nodes visible in writable replay:
// those reconstructed from the capture as-of the clock, merged with the overlay
// (overlay-created nodes added, tombstoned ones removed).
func (h *handler) knownNodeNames() []string {
	at := h.at
	if h.clock != nil {
		at = h.clock.Now()
	}
	return h.knownNodeNamesAt(at)
}

// knownNodeNamesAt returns the sorted node names visible as-of the given instant
// (captured nodes reconstructed at `at`, merged with overlay writes).
func (h *handler) knownNodeNamesAt(at time.Time) []string {
	var items []json.RawMessage
	if body, code, err := h.store.ReconstructAt("/api/v1/nodes", at); err == nil && code == 200 {
		var l struct {
			Items []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(body, &l) == nil {
			items = l.Items
		}
	}
	items, _ = h.overlay.applyToList("", "v1", "nodes", "", items)
	var names []string
	for _, it := range items {
		if n := metaString(it, "name"); n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}

// synthesizeNode stores a synthetic Ready Node, annotated so a stock `kwok` run
// manages it (`kwok.x-k8s.io/node: fake`). It gives the scheduling shim a target
// when the capture has no nodes, and KWOK a node to keep Ready.
func (h *handler) synthesizeNode(name string) {
	h.synthesizeOverlayObject("nodes", "", name, syntheticNodeBase(name))
}

// syntheticNodeBase is the base body for a synthesized Node (metadata name/uid/rv
// are stamped by synthesizeOverlayObject). Built via json.Marshal so the node
// name is always correctly escaped.
func syntheticNodeBase(name string) string {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata": map[string]any{
			"annotations": map[string]any{"kwok.x-k8s.io/node": "fake"},
			"labels": map[string]any{
				"type":                   "kwok",
				"kubernetes.io/os":       "linux",
				"kubernetes.io/hostname": name,
			},
		},
		"spec": map[string]any{},
		"status": map[string]any{
			"phase":       "Running",
			"conditions":  []any{map[string]any{"type": "Ready", "status": "True", "reason": "KubeletReady"}},
			"allocatable": map[string]any{"cpu": "32", "memory": "256Gi", "pods": "110"},
			"capacity":    map[string]any{"cpu": "32", "memory": "256Gi", "pods": "110"},
		},
	}
	b, _ := json.Marshal(obj)
	return string(b)
}

// ensurePodStatusPending sets status.phase=Pending on a pod body when it has no
// phase, mirroring the apiserver's create-time default. Returns body unchanged on
// a decode/encode error or if a phase is already set.
func ensurePodStatusPending(body json.RawMessage) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body
	}
	status, ok := m["status"].(map[string]any)
	if !ok || status == nil {
		status = map[string]any{}
		m["status"] = status
	}
	if p, _ := status["phase"].(string); p != "" {
		return body // already has a phase; leave it
	}
	status["phase"] = "Pending"
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// podNodeName returns a pod body's spec.nodeName ("" if unset).
func podNodeName(body json.RawMessage) string {
	var p struct {
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(body, &p)
	return p.Spec.NodeName
}

// setSpecNodeName returns body with spec.nodeName set to node, preserving the
// rest of the object. On a decode/encode error the body is returned unchanged.
func setSpecNodeName(body json.RawMessage, node string) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body
	}
	spec, ok := m["spec"].(map[string]any)
	if !ok || spec == nil {
		spec = map[string]any{}
		m["spec"] = spec
	}
	spec["nodeName"] = node
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}
