package diagnose

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/server"
)

// ── workload: missing resource requests/limits ───────────────────────────────

func missingResourceFindings(store *server.CaptureStore, at time.Time) []Finding {
	g := newGrouper()
	forEachResource(store, at, "pods", func(ns, path string, items []json.RawMessage) {
		for _, raw := range items {
			var pod struct {
				objMeta
				Spec struct {
					Containers []struct {
						Resources struct {
							Requests map[string]string `json:"requests"`
							Limits   map[string]string `json:"limits"`
						} `json:"resources"`
					} `json:"containers"`
				} `json:"spec"`
			}
			if json.Unmarshal(raw, &pod) != nil || len(pod.Spec.Containers) == 0 {
				continue
			}
			noReq, noLim := false, false
			for _, c := range pod.Spec.Containers {
				if len(c.Resources.Requests) == 0 {
					noReq = true
				}
				if len(c.Resources.Limits) == 0 {
					noLim = true
				}
			}
			owner := pod.owner()
			if noReq {
				g.add("workload.no-requests|"+ns+"|"+owner, Finding{
					RuleID: "workload.no-requests", Severity: SeverityWarning, Category: "workload",
					Title:      "Container without resource requests",
					Object:     ObjectRef{Kind: "Pod", Namespace: ns, Name: owner, APIPath: path},
					Evidence:   "a container has no resources.requests",
					Suggestion: "Set CPU/memory requests so the scheduler can place the pod and give it a QoS class.",
				})
			}
			if noLim {
				g.add("workload.no-limits|"+ns+"|"+owner, Finding{
					RuleID: "workload.no-limits", Severity: SeverityInfo, Category: "workload",
					Title:      "Container without resource limits",
					Object:     ObjectRef{Kind: "Pod", Namespace: ns, Name: owner, APIPath: path},
					Evidence:   "a container has no resources.limits",
					Suggestion: "Set memory/CPU limits to bound noisy-neighbor and OOM risk.",
				})
			}
		}
	})
	return g.findings()
}

// ── workload: replica/availability shortfall ─────────────────────────────────

func replicaFindings(store *server.CaptureStore, at time.Time) []Finding {
	var out []Finding
	for _, rk := range []struct{ resource, kind string }{
		{"deployments", "Deployment"}, {"statefulsets", "StatefulSet"}, {"replicasets", "ReplicaSet"},
	} {
		forEachResource(store, at, rk.resource, func(ns, path string, items []json.RawMessage) {
			for _, raw := range items {
				var w struct {
					objMeta
					Spec struct {
						Replicas *int `json:"replicas"`
					} `json:"spec"`
					Status struct {
						ReadyReplicas     int `json:"readyReplicas"`
						AvailableReplicas int `json:"availableReplicas"`
					} `json:"status"`
				}
				if json.Unmarshal(raw, &w) != nil {
					continue
				}
				desired := 1
				if w.Spec.Replicas != nil {
					desired = *w.Spec.Replicas
				}
				ready := w.Status.ReadyReplicas
				if w.Status.AvailableReplicas > ready {
					ready = w.Status.AvailableReplicas
				}
				if desired > 0 && ready < desired {
					out = append(out, Finding{
						RuleID: "workload.replicas-unavailable", Severity: SeverityWarning, Category: "workload",
						Title:      rk.kind + " not fully available",
						Object:     ObjectRef{Kind: rk.kind, Namespace: ns, Name: w.Metadata.Name, APIPath: path},
						Evidence:   fmt.Sprintf("%d/%d replicas ready", ready, desired),
						Suggestion: "Check the managed pods for scheduling, image, or crash issues.",
						Count:      1,
					})
				}
			}
		})
	}
	// DaemonSets use a different status shape.
	forEachResource(store, at, "daemonsets", func(ns, path string, items []json.RawMessage) {
		for _, raw := range items {
			var ds struct {
				objMeta
				Status struct {
					DesiredNumberScheduled int `json:"desiredNumberScheduled"`
					NumberReady            int `json:"numberReady"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &ds) != nil {
				continue
			}
			if ds.Status.DesiredNumberScheduled > 0 && ds.Status.NumberReady < ds.Status.DesiredNumberScheduled {
				out = append(out, Finding{
					RuleID: "workload.replicas-unavailable", Severity: SeverityWarning, Category: "workload",
					Title:      "DaemonSet not fully available",
					Object:     ObjectRef{Kind: "DaemonSet", Namespace: ns, Name: ds.Metadata.Name, APIPath: path},
					Evidence:   fmt.Sprintf("%d/%d nodes ready", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled),
					Suggestion: "Check the DaemonSet pods on the affected nodes.",
					Count:      1,
				})
			}
		}
	})
	return out
}

// ── node: conditions ─────────────────────────────────────────────────────────

func nodeConditionFindings(store *server.CaptureStore, at time.Time) []Finding {
	var out []Finding
	forEachResource(store, at, "nodes", func(_, path string, items []json.RawMessage) {
		for _, raw := range items {
			var node struct {
				Metadata struct{ Name string } `json:"metadata"`
				Status   struct {
					Conditions []struct{ Type, Status string } `json:"conditions"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &node) != nil {
				continue
			}
			ref := ObjectRef{Kind: "Node", Name: node.Metadata.Name, APIPath: path}
			for _, c := range node.Status.Conditions {
				switch {
				case c.Type == "Ready" && c.Status != "True":
					out = append(out, Finding{
						RuleID: "node.not-ready", Severity: SeverityCritical, Category: "node",
						Title: "Node not Ready", Object: ref,
						Evidence:   "Ready condition is " + c.Status,
						Suggestion: "Investigate the kubelet, network, or node health.",
						Count:      1,
					})
				case (c.Type == "DiskPressure" || c.Type == "MemoryPressure" || c.Type == "PIDPressure") && c.Status == "True":
					out = append(out, Finding{
						RuleID: "node.pressure", Severity: SeverityWarning, Category: "node",
						Title: "Node under " + c.Type, Object: ref,
						Evidence:   c.Type + " is True",
						Suggestion: "Free resources on the node or reschedule workloads; pods may be evicted.",
						Count:      1,
					})
				}
			}
		}
	})
	return out
}

// ── cluster: deprecated/removed API usage (index-based, no body reads) ────────

// deprecatedGroupVersions maps a captured group/version to the release that
// removed it (or "deprecated" guidance). Index-driven, so it flags usage even
// without reading object bodies.
var deprecatedGroupVersions = map[string]string{
	"extensions/v1beta1":                "removed in v1.22",
	"apps/v1beta1":                      "removed in v1.16",
	"apps/v1beta2":                      "removed in v1.16",
	"networking.k8s.io/v1beta1":         "removed in v1.22",
	"rbac.authorization.k8s.io/v1beta1": "removed in v1.22",
	"policy/v1beta1":                    "removed in v1.25 (PodSecurityPolicy) / moved",
	"batch/v1beta1":                     "removed in v1.25 (CronJob)",
	"autoscaling/v2beta1":               "removed in v1.25",
	"autoscaling/v2beta2":               "removed in v1.26",
	"storage.k8s.io/v1beta1":            "deprecated; use storage.k8s.io/v1",
}

func deprecatedAPIFindings(store *server.CaptureStore) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for path := range store.Index {
		if strings.Contains(path, "?") {
			continue
		}
		group, version, resource, _ := parseAPIPath(path)
		if group == "" {
			continue // core/v1 is never deprecated
		}
		gv := group + "/" + version
		note, ok := deprecatedGroupVersions[gv]
		if !ok {
			continue
		}
		key := gv + "/" + resource
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, Finding{
			RuleID: "cluster.deprecated-api", Severity: SeverityWarning, Category: "cluster",
			Title:      "Deprecated API in use",
			Object:     ObjectRef{Kind: resource, APIPath: path},
			Evidence:   fmt.Sprintf("%s %s — %s", gv, resource, note),
			Suggestion: "Migrate manifests/controllers to the current stable API version.",
		})
	}
	return out
}
