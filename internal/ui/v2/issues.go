package v2

import (
	"fmt"
	"sort"
	"strings"
)

// groupKey identifies a deduplicated bucket of unhealthy pods.
type groupKey struct {
	ns     string
	prefix string
	reason string
}

// groupVal accumulates the pods that fell into one groupKey.
type groupVal struct {
	count    int
	exemplar podHealthEntry
	restarts int
}

// buildIssues groups unhealthy pods into a short, deduplicated list suitable
// for the dashboard "Issues to investigate" card.
//
// Grouping rules:
//   - pods in the same namespace with the same name prefix (computed by
//     PodNamePrefix — strips ReplicaSet hash + per-replica suffix) and the
//     same dominant issue reason collapse into a single grouped issue
//     with Count > 1.
//   - within a group, the link points to the first matching pod.
//
// Severity:
//   - "bad"  for CrashLoopBackOff, OOMKilled, Failed, ImagePullBackOff, etc.
//   - "warn" for everything else.
//
// Returns the top `limit` groups by severity then by Count desc.
func buildIssues(pods []podHealthEntry, limit int) []Issue {
	if len(pods) == 0 {
		return nil
	}
	groups := map[groupKey]*groupVal{}
	for _, p := range pods {
		if p.health.IsHealthy() {
			continue
		}
		reason := dominantIssue(p.health.Issues)
		k := groupKey{ns: p.namespace, prefix: p.prefix, reason: reason}
		g, ok := groups[k]
		if !ok {
			g = &groupVal{exemplar: p}
			groups[k] = g
		}
		g.count++
		g.restarts += p.health.Restarts
	}
	if len(groups) == 0 {
		return nil
	}

	out := make([]Issue, 0, len(groups))
	for k, g := range groups {
		sev := severityFor(k.reason)
		title := g.exemplar.name
		if g.count > 1 {
			title = k.prefix
		}
		sub := subtitleFor(k.reason, g)
		out = append(out, Issue{
			Severity:  sev,
			Kind:      shortKindForPod(),
			Title:     title,
			Subtitle:  sub,
			Namespace: k.ns,
			Count:     g.count,
			Link:      podLink(g.exemplar.namespace, g.exemplar.name),
		})
	}
	// Bad first, then by count desc, then by namespace+title for stable order.
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Severity == "bad") != (out[j].Severity == "bad") {
			return out[i].Severity == "bad"
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Title < out[j].Title
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// dominantIssue picks the most actionable reason from a pod's issue list.
// Order matters: a pod that is CrashLoopBackOff is interesting for that
// reason, not for the underlying Error or OOMKilled (which we still capture
// in the per-container card on the drilldown).
func dominantIssue(issues []string) string {
	if len(issues) == 0 {
		return "Unknown"
	}
	priority := []string{
		"CrashLoopBackOff",
		"OOMKilled",
		"ImagePullBackOff", "ErrImagePull",
		"CreateContainerError", "CreateContainerConfigError",
		"InvalidImageName",
		"Error",
		"Failed",
		"Unknown",
	}
	have := map[string]bool{}
	for _, x := range issues {
		have[x] = true
	}
	for _, p := range priority {
		if have[p] {
			return p
		}
	}
	return issues[0]
}

func severityFor(reason string) string {
	switch reason {
	case "CrashLoopBackOff", "OOMKilled", "Failed", "Error", "ImagePullBackOff", "ErrImagePull",
		"CreateContainerError", "CreateContainerConfigError", "InvalidImageName":
		return "bad"
	}
	return "warn"
}

func subtitleFor(reason string, g *groupVal) string {
	switch reason {
	case "CrashLoopBackOff":
		if g.count > 1 {
			return fmt.Sprintf("CrashLoopBackOff · %d restarts across %d pods", g.restarts, g.count)
		}
		return fmt.Sprintf("CrashLoopBackOff · %d restarts", g.restarts)
	case "OOMKilled":
		return fmt.Sprintf("OOMKilled · %d restarts", g.restarts)
	case "ImagePullBackOff", "ErrImagePull":
		return reason
	case "Failed":
		return "Pod phase = Failed"
	}
	return reason
}

func shortKindForPod() string { return "Pod" }

func podLink(ns, name string) string {
	return "#/ns/" + escapeHash(ns) + "/pod/" + escapeHash(name)
}

// escapeHash encodes path segments destined for the hash router. Avoids the
// usual encodeURIComponent set but keeps the string visually readable.
func escapeHash(s string) string {
	r := strings.NewReplacer("/", "%2F", "#", "%23", "?", "%3F")
	return r.Replace(s)
}

// kindFromResource is a small dictionary keeping the dashboard tiles and
// recent-transitions labels human-readable. Falls back to titlecasing the
// resource name.
func kindFromResource(resource string) string {
	known := map[string]string{
		"deployments":               "Deployment",
		"statefulsets":              "StatefulSet",
		"daemonsets":                "DaemonSet",
		"jobs":                      "Job",
		"cronjobs":                  "CronJob",
		"replicasets":               "ReplicaSet",
		"pods":                      "Pod",
		"services":                  "Service",
		"configmaps":                "ConfigMap",
		"secrets":                   "Secret",
		"persistentvolumeclaims":    "PersistentVolumeClaim",
		"persistentvolumes":         "PersistentVolume",
		"nodes":                     "Node",
		"ingresses":                 "Ingress",
		"customresourcedefinitions": "CRD",
		"virtualmachines":           "VirtualMachine",
		"virtualmachineinstances":   "VirtualMachineInstance",
		"events":                    "Event",
		"storageclasses":            "StorageClass",
		"namespaces":                "Namespace",
		"endpoints":                 "Endpoints",
		"componentstatuses":         "ComponentStatus",
		"serviceaccounts":           "ServiceAccount",
		"replicationcontrollers":    "ReplicationController",
		"networkpolicies":           "NetworkPolicy",
		"poddisruptionbudgets":      "PodDisruptionBudget",
	}
	if k, ok := known[resource]; ok {
		return k
	}
	if resource == "" {
		return ""
	}
	return titleCase(singularize(resource))
}

// singularize converts a plural resource name to a best-effort singular using
// common English rules (authoritative kinds come from API discovery; this is
// only a fallback for callers that have just the resource name).
func singularize(s string) string {
	switch {
	case strings.HasSuffix(s, "ies"):
		return s[:len(s)-3] + "y" // policies -> policy
	case strings.HasSuffix(s, "sses"), strings.HasSuffix(s, "shes"), strings.HasSuffix(s, "ches"),
		strings.HasSuffix(s, "xes"), strings.HasSuffix(s, "zes"), strings.HasSuffix(s, "ses"):
		return s[:len(s)-2] // statuses -> status, ingresses -> ingress, classes -> class
	case strings.HasSuffix(s, "s") && len(s) > 1:
		return s[:len(s)-1]
	default:
		return s
	}
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
