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
// Event objects carry a second, independent namespace reference, in
// addition to the Event's own metadata.namespace (an Event is itself
// namespaced, so the generic case above already covers that) — but which
// field holds it depends on which Events API produced the record, and a
// real cluster emits both depending on client/version:
//
//   - core/v1 Event: involvedObject.namespace
//   - events.k8s.io/v1 Event: regarding.namespace, and optionally
//     related.namespace (an additional secondary reference introduced by
//     the newer API and absent from core/v1)
//
// Both events.k8s.io/v1's "regarding" and "related" are the same
// ObjectReference shape as core/v1's "involvedObject" — different field
// *names* on the parent Event, same Namespace field within. All three are
// checked unconditionally; whichever ones a given record doesn't have are
// simply absent from the decoded map and the corresponding check is a no-op.
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
		for _, field := range []string{"involvedObject", "regarding", "related"} {
			ref, ok := obj[field].(map[string]interface{})
			if !ok {
				continue
			}
			if ns, ok := ref["namespace"].(string); ok && ns != "" {
				ref["namespace"] = alias(ns)
				modified = true
			}
		}
	}

	return modified
}

// rewriteNamespaceInRecord decodes rec's body, rewrites every namespace
// occurrence it recognizes using alias, and re-encodes the body if anything
// changed. Tracking which distinct original values were seen (for
// Result.NamespacesRenamed and for collision detection) is alias's own job
// now — see collisionTracker — not this function's; it used to take a
// separate seen map for that, which became redundant bookkeeping once the
// tracker existed to do the same thing more safely.
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
func rewriteNamespaceInRecord(rec *capture.Record, alias func(string) string) (bool, error) {
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
			if rewriteNamespaceInObject(item, ik, alias) {
				items[i] = item
				modified = true
			}
		}
	} else if rewriteNamespaceInObject(obj, kind, alias) {
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
