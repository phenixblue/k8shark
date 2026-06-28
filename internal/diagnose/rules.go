package diagnose

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/server"
)

// ── workload: pod health ─────────────────────────────────────────────────────

// reasonRule maps a container/pod issue reason to a finding template. ok=false
// means the reason is benign/transient and should not be reported.
func reasonRule(reason string) (ruleID, severity, title, suggestion string, ok bool) {
	switch reason {
	case "CrashLoopBackOff":
		return "pod.crashloopbackoff", SeverityCritical, "CrashLoopBackOff", "Container is repeatedly crashing — check its logs and previous exit code.", true
	case "OOMKilled":
		return "pod.oomkilled", SeverityCritical, "OOMKilled", "Container exceeded its memory limit — raise limits or fix the leak.", true
	case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
		return "pod.image-pull", SeverityCritical, "Image pull failure", "Verify the image name/tag and registry credentials.", true
	case "CreateContainerConfigError", "CreateContainerError":
		return "pod.config-error", SeverityCritical, "Container config error", "A referenced ConfigMap/Secret or key is likely missing.", true
	case "Error":
		return "pod.container-error", SeverityWarning, "Container terminated with error", "Inspect the container logs and termination message.", true
	case "Failed":
		return "pod.failed", SeverityWarning, "Pod in Failed phase", "Inspect pod status and events for the failure cause.", true
	case "Unknown":
		return "pod.unknown", SeverityWarning, "Pod in Unknown phase", "The node may be unreachable — check node status.", true
	case "ContainerCreating", "PodInitializing", "Completed", "":
		return "", "", "", "", false // benign / transient
	default:
		return "pod." + sanitizeReason(reason), SeverityWarning, reason, "Inspect the pod for details on this condition.", true
	}
}

func podHealthFindings(store *server.CaptureStore, at time.Time) []Finding {
	g := newGrouper()
	forEachResource(store, at, "pods", func(ns, path string, items []json.RawMessage) {
		for _, raw := range items {
			var m objMeta
			_ = json.Unmarshal(raw, &m)
			for _, reason := range ClassifyPod(raw).Issues {
				ruleID, sev, title, sug, ok := reasonRule(reason)
				if !ok {
					continue
				}
				owner := m.owner()
				key := ruleID + "|" + ns + "|" + owner
				g.add(key, Finding{
					RuleID: ruleID, Severity: sev, Category: "workload", Title: title,
					Object:     ObjectRef{Kind: "Pod", Namespace: ns, Name: owner, APIPath: path},
					Evidence:   reason,
					Suggestion: sug,
				})
			}
		}
	})
	return g.findings()
}

func sanitizeReason(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// ── scheduling: unschedulable pods ───────────────────────────────────────────

func schedulingFindings(store *server.CaptureStore, at time.Time) []Finding {
	g := newGrouper()
	forEachResource(store, at, "pods", func(ns, path string, items []json.RawMessage) {
		for _, raw := range items {
			var p struct {
				objMeta
				Status struct {
					Phase      string `json:"phase"`
					Conditions []struct {
						Type, Status, Reason, Message string
					} `json:"conditions"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &p) != nil || p.Status.Phase != "Pending" {
				continue
			}
			for _, c := range p.Status.Conditions {
				if c.Type == "PodScheduled" && c.Status == "False" {
					msg := c.Message
					if msg == "" {
						msg = c.Reason
					}
					// Key on owner too so distinct workloads with the same generic
					// message ("0/N nodes available…") aren't merged into one finding.
					key := "scheduling.unschedulable|" + ns + "|" + p.owner() + "|" + msg
					g.add(key, Finding{
						RuleID: "scheduling.unschedulable", Severity: SeverityWarning, Category: "scheduling",
						Title:      "Pod cannot be scheduled",
						Object:     ObjectRef{Kind: "Pod", Namespace: ns, Name: p.owner(), APIPath: path},
						Evidence:   msg,
						Suggestion: "Add capacity or adjust resource requests, node selectors, affinity, or tolerations.",
					})
				}
			}
		}
	})
	return g.findings()
}

// ── storage: unbound PVCs ────────────────────────────────────────────────────

func pvcFindings(store *server.CaptureStore, at time.Time) []Finding {
	g := newGrouper()
	forEachResource(store, at, "persistentvolumeclaims", func(ns, path string, items []json.RawMessage) {
		for _, raw := range items {
			var pvc struct {
				objMeta
				Spec struct {
					StorageClassName *string `json:"storageClassName"`
				} `json:"spec"`
				Status struct {
					Phase string `json:"phase"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &pvc) != nil {
				continue
			}
			if pvc.Status.Phase == "Bound" || pvc.Status.Phase == "" {
				continue
			}
			sc := "(default)"
			if pvc.Spec.StorageClassName != nil {
				sc = *pvc.Spec.StorageClassName
			}
			// Key per claim — each PVC is a distinct object, don't collapse them.
			key := "storage.pvc-unbound|" + ns + "|" + pvc.Metadata.Name
			g.add(key, Finding{
				RuleID: "storage.pvc-unbound", Severity: SeverityWarning, Category: "storage",
				Title:      "PersistentVolumeClaim not bound",
				Object:     ObjectRef{Kind: "PersistentVolumeClaim", Namespace: ns, Name: pvc.Metadata.Name, APIPath: path},
				Evidence:   fmt.Sprintf("phase=%s, storageClass=%s", pvc.Status.Phase, sc),
				Suggestion: "Ensure a matching StorageClass and provisioner (or a suitable PV) exist.",
			})
		}
	})
	return g.findings()
}

// ── cluster: control-plane / node version skew ───────────────────────────────

func versionSkewFindings(store *server.CaptureStore, at time.Time) []Finding {
	cpMinor, okCP := parseMinor(store.Metadata.KubernetesVersion)
	if !okCP {
		return nil
	}
	var out []Finding
	forEachResource(store, at, "nodes", func(_, path string, items []json.RawMessage) {
		for _, raw := range items {
			var node struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Status struct {
					NodeInfo struct {
						KubeletVersion string `json:"kubeletVersion"`
					} `json:"nodeInfo"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &node) != nil {
				continue
			}
			nMinor, ok := parseMinor(node.Status.NodeInfo.KubeletVersion)
			if !ok {
				continue
			}
			skew := cpMinor - nMinor
			if skew < 0 {
				skew = -skew
			}
			if skew < 3 { // within the supported version-skew window
				continue
			}
			out = append(out, Finding{
				RuleID: "cluster.version-skew", Severity: SeverityWarning, Category: "cluster",
				Title:  "Node/control-plane version skew",
				Object: ObjectRef{Kind: "Node", Name: node.Metadata.Name, APIPath: path},
				Evidence: fmt.Sprintf("control plane %s, kubelet %s (%d minor versions apart)",
					store.Metadata.KubernetesVersion, node.Status.NodeInfo.KubeletVersion, skew),
				Suggestion: "Bring the node's kubelet within the supported skew (≤2 minor versions) of the control plane.",
			})
		}
	})
	return out
}

// parseMinor extracts the minor version from a "vMAJOR.MINOR.PATCH" string.
func parseMinor(v string) (int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, false
	}
	// trim any suffix like "27+" or "27-eks".
	minor := parts[1]
	for i, r := range minor {
		if r < '0' || r > '9' {
			minor = minor[:i]
			break
		}
	}
	n, err := strconv.Atoi(minor)
	if err != nil {
		return 0, false
	}
	return n, true
}
