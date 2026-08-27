package anonymize

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/phenixblue/k8shark/internal/capture"
)

// rewriteNamespaceInObject rewrites the namespace identity in a single
// decoded JSON object (not a list), using alias for whatever occurrences it
// finds. kind is the object's own Kind, passed down by the caller rather
// than re-extracted here — the same convention internal/redact/fields.go
// uses.
//
// A Namespace object's OWN identity is its metadata.name — Namespace is
// cluster-scoped, so it has no metadata.namespace field at all. Every other
// Kind's membership in a namespace lives in ITS OWN metadata.namespace
// field instead. Getting this backwards doesn't error, it just silently
// anonymizes nothing for that object — which is exactly why it's called out
// here in a comment rather than left to be discovered via a failing
// round-trip test.
//
// Event objects carry a second, independent namespace reference:
// involvedObject.namespace, naming the namespace of whatever the Event is
// about. That is in addition to the Event's own metadata.namespace (an
// Event is itself namespaced, so the generic case above already covers
// that), not instead of it.
func rewriteNamespaceInObject(obj map[string]interface{}, kind string, alias func(string) string) bool {
	meta, _ := obj["metadata"].(map[string]interface{})
	if meta == nil {
		return false
	}

	modified := false
	if kind == "Namespace" {
		if name, ok := meta["name"].(string); ok && name != "" {
			meta["name"] = alias(name)
			modified = true
		}
	} else if ns, ok := meta["namespace"].(string); ok && ns != "" {
		meta["namespace"] = alias(ns)
		modified = true
	}

	if kind == "Event" {
		if involved, ok := obj["involvedObject"].(map[string]interface{}); ok {
			if ns, ok := involved["namespace"].(string); ok && ns != "" {
				involved["namespace"] = alias(ns)
				modified = true
			}
		}
	}

	return modified
}

// rewriteNamespaceInRecord decodes rec's body, rewrites every namespace
// occurrence it recognizes using alias, and re-encodes the body if anything
// changed. seen accumulates every distinct original namespace name
// encountered — used only for the reported count (Result.NamespacesRenamed),
// not for the aliasing itself, so its accumulation order has no bearing on
// determinism.
//
// Handles both a single-object response and a List response's items[],
// mirroring internal/redact/fields.go's ApplyRules list-handling. Records
// that don't decode into a JSON object at all — a meta.k8s.io/v1 Table
// response's rows[], or a discovery/OpenAPI document — are left untouched,
// matching internal/redact's existing precedent for the same case. Catching
// a namespace name inside an opaque Table cell is deliberately out of scope
// for the archive-rewrite path in this milestone: Table rows have no field
// names, so it needs a different, value-pattern-based approach layered on
// top of this schema-aware one, not a fix to this function.
func rewriteNamespaceInRecord(rec *capture.Record, alias func(string) string, seen map[string]bool) (bool, error) {
	var obj map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
		return false, nil
	}

	kind, _ := obj["kind"].(string)
	modified := false

	track := func(original string) string {
		seen[original] = true
		return alias(original)
	}

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
			if rewriteNamespaceInObject(item, ik, track) {
				items[i] = item
				modified = true
			}
		}
	} else if rewriteNamespaceInObject(obj, kind, track) {
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
