package anonymize

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/phenixblue/k8shark/internal/capture"
)

// resourceKindEntry is one row of the single source of truth mapping a Kind
// to both its anonymize Category and its lowercase REST resource-type path
// segment. Kept as one table, rather than two hand-maintained maps, so the
// object-body side (keyed by Kind) and the APIPath side (keyed by the path's
// resource-type segment) can't drift apart the way alias.go's
// implementedCategories/switch pairing already has a test guarding against.
type resourceKindEntry struct {
	Kind         string
	ResourceType string
	Category     Category
}

var resourceKindTable = []resourceKindEntry{
	{"Node", "nodes", CategoryNode},
	{"Pod", "pods", CategoryPod},
	{"Deployment", "deployments", CategoryWorkload},
	{"ReplicaSet", "replicasets", CategoryWorkload},
	{"StatefulSet", "statefulsets", CategoryWorkload},
	{"DaemonSet", "daemonsets", CategoryWorkload},
	{"Job", "jobs", CategoryWorkload},
	{"CronJob", "cronjobs", CategoryWorkload},
	{"ReplicationController", "replicationcontrollers", CategoryWorkload},
}

var kindCategories = func() map[string]Category {
	m := make(map[string]Category, len(resourceKindTable))
	for _, e := range resourceKindTable {
		m[e.Kind] = e.Category
	}
	return m
}()

var resourceTypeCategories = func() map[string]Category {
	m := make(map[string]Category, len(resourceKindTable))
	for _, e := range resourceKindTable {
		m[e.ResourceType] = e.Category
	}
	return m
}()

// rewriteResourceNameInObject rewrites node/pod/workload resource-name
// occurrences in a single decoded JSON object (not a list) — kind is the
// object's own Kind, passed down by the caller, same convention as
// rewriteNamespaceInObject. enabled gates which categories are actually
// requested this run: a field belonging to a category not in enabled is
// left untouched even if it's recognized, so e.g. --categories pod alone
// doesn't also alias a Pod's spec.nodeName.
//
// Schema-aware only, matching this milestone's scope: catching a resource
// name as a substring inside free text (an annotation value, an Event
// message) or an opaque Table cell is deliberately deferred — there is no
// generic pattern for "this token is a Kubernetes name" the way there is
// for an IP literal, so recall would need a candidate set built from these
// same structured fields first (see #137's plan, Open Question 5).
func rewriteResourceNameInObject(obj map[string]interface{}, kind string, enabled map[Category]bool, excluded excludedFunc, alias func(Category, string) string) bool {
	modified := false

	if meta, ok := obj["metadata"].(map[string]interface{}); ok {
		if cat, ok := kindCategories[kind]; ok && enabled[cat] && !excluded(cat, kind, "metadata.name") {
			if name, ok := meta["name"].(string); ok && name != "" {
				meta["name"] = alias(cat, name)
				modified = true
			}
		}
		// ownerReferences[*].name: an owning Node never occurs in practice,
		// but a Pod or any workload Kind can be an owner (e.g. a Job owning
		// a Pod, a Deployment owning a ReplicaSet owning a Pod) — categorize
		// by the *reference's own* kind, not the enclosing object's.
		if ownerRefs, ok := meta["ownerReferences"].([]interface{}); ok {
			for _, raw := range ownerRefs {
				ref, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				ownerKind, _ := ref["kind"].(string)
				cat, ok := kindCategories[ownerKind]
				if !ok || !enabled[cat] || excluded(cat, kind, "metadata.ownerReferences[*].name") {
					continue
				}
				if name, ok := ref["name"].(string); ok && name != "" {
					ref["name"] = alias(cat, name)
					modified = true
				}
			}
		}
	}

	if kind == "Pod" && enabled[CategoryNode] && !excluded(CategoryNode, kind, "spec.nodeName") {
		if spec, ok := obj["spec"].(map[string]interface{}); ok {
			if nodeName, ok := spec["nodeName"].(string); ok && nodeName != "" {
				spec["nodeName"] = alias(CategoryNode, nodeName)
				modified = true
			}
		}
	}

	if kind == "Node" && enabled[CategoryNode] && !excluded(CategoryNode, kind, "status.addresses[*].address") {
		if status, ok := obj["status"].(map[string]interface{}); ok {
			if addrs, ok := status["addresses"].([]interface{}); ok {
				for _, raw := range addrs {
					addr, ok := raw.(map[string]interface{})
					if !ok || addr["type"] != "Hostname" {
						continue
					}
					if address, ok := addr["address"].(string); ok && address != "" {
						addr["address"] = alias(CategoryNode, address)
						modified = true
					}
				}
			}
		}
	}

	if kind == "Event" {
		// Same "both API shapes" requirement namespace.go's fix already
		// established for events.k8s.io/v1: involvedObject (core/v1) vs.
		// regarding/related (events.k8s.io/v1) are the same ObjectReference
		// shape under different field names.
		for _, field := range []string{"involvedObject", "regarding", "related"} {
			ref, ok := obj[field].(map[string]interface{})
			if !ok {
				continue
			}
			refKind, _ := ref["kind"].(string)
			cat, ok := kindCategories[refKind]
			if !ok || !enabled[cat] || excluded(cat, kind, field+".name") {
				continue
			}
			if name, ok := ref["name"].(string); ok && name != "" {
				ref["name"] = alias(cat, name)
				modified = true
			}
		}
		// source.host (core/v1) / deprecatedSource.host (events.k8s.io/v1,
		// carried for back-compat with the core/v1 shape) both hold the node
		// name the event was generated on.
		if enabled[CategoryNode] {
			for _, field := range []string{"source", "deprecatedSource"} {
				if excluded(CategoryNode, kind, field+".host") {
					continue
				}
				src, ok := obj[field].(map[string]interface{})
				if !ok {
					continue
				}
				if host, ok := src["host"].(string); ok && host != "" {
					src["host"] = alias(CategoryNode, host)
					modified = true
				}
			}
		}
	}

	return modified
}

// rewriteResourceNameInRecord decodes rec's body, rewrites every node/pod/
// workload name occurrence rewriteResourceNameInObject recognizes, and
// re-encodes the body if anything changed. Mirrors
// rewriteNamespaceInRecord's list-handling exactly (see its own doc comment
// for why a non-JSON or Table/discovery body is left untouched rather than
// erroring).
func rewriteResourceNameInRecord(rec *capture.Record, enabled map[Category]bool, excluded excludedFunc, alias func(Category, string) string) (bool, error) {
	var obj map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
		return false, nil
	}

	kind, _ := obj["kind"].(string)
	modified := false

	if strings.HasSuffix(kind, "List") {
		items, _ := obj["items"].([]interface{})
		itemKind := strings.TrimSuffix(kind, "List")
		for i, itemRaw := range items {
			item, ok := itemRaw.(map[string]interface{})
			if !ok {
				continue
			}
			ik := itemKind
			if k, ok := item["kind"].(string); ok && k != "" {
				ik = k
			}
			if rewriteResourceNameInObject(item, ik, enabled, excluded, alias) {
				items[i] = item
				modified = true
			}
		}
	} else if rewriteResourceNameInObject(obj, kind, enabled, excluded, alias) {
		modified = true
	}

	if !modified {
		return false, nil
	}
	newBody, err := json.Marshal(obj)
	if err != nil {
		return false, fmt.Errorf("re-marshaling record: %w", err)
	}
	rec.ResponseBody = newBody
	return true, nil
}

// rewriteResourceNameInPath replaces the object-name segment of a captured
// API path with alias(category, original), when that path names a single
// object of a category in enabled. Unlike rewriteNamespaceInPath's
// literal-segment search (safe there because "namespaces" is a fixed
// keyword), this walks the path's actual api/apis+group+version[+namespaces
// pair]+resourceType+name structure — a resource-type segment like "pods"
// can otherwise coincidentally equal a real namespace *value* elsewhere in
// the same path, and a literal search for "pods" would then mistake that
// value for the resource-type segment itself.
//
//	/api/v1/nodes/<name>                                (cluster-scoped GET)
//	/api/v1/namespaces/<ns>/pods/<name>                 (namespaced GET)
//	/apis/apps/v1/namespaces/<ns>/deployments/<name>    (group/version form)
//	/api/v1/namespaces/<ns>/pods/<name>/log             (<name> still rewritten; only "log" is untouched)
//
// Returns the path unchanged (ok=false) for a list path (nothing follows
// the resource-type segment), a resource type this package doesn't
// recognize, or a recognized one whose category isn't in enabled.
func rewriteResourceNameInPath(apiPath string, enabled map[Category]bool, alias func(Category, string) string) (string, bool) {
	parts := strings.Split(apiPath, "/")

	i := 1
	if i >= len(parts) {
		return apiPath, false
	}
	switch parts[i] {
	case "api":
		i += 2 // "api", <version>
	case "apis":
		i += 3 // "apis", <group>, <version>
	default:
		return apiPath, false
	}
	if i >= len(parts) {
		return apiPath, false
	}
	if parts[i] == "namespaces" {
		i += 2 // "namespaces", <ns>
	}
	if i >= len(parts) {
		return apiPath, false
	}

	cat, ok := resourceTypeCategories[parts[i]]
	if !ok || !enabled[cat] {
		return apiPath, false
	}
	nameIdx := i + 1
	if nameIdx >= len(parts) || parts[nameIdx] == "" {
		return apiPath, false
	}
	parts[nameIdx] = alias(cat, parts[nameIdx])
	return strings.Join(parts, "/"), true
}
