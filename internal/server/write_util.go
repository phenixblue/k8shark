package server

import (
	"encoding/json"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
)

// patchMediaType strips any parameters from a PATCH Content-Type and lower-cases
// it (media types are case-insensitive per RFC 7231).
func patchMediaType(contentType string) string {
	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// supportedPatchType reports whether the PATCH Content-Type is one we handle;
// an unknown or empty type is rejected with 415 rather than silently merged.
func supportedPatchType(contentType string) bool {
	switch patchMediaType(contentType) {
	case "application/merge-patch+json", "application/json-patch+json",
		"application/strategic-merge-patch+json", "application/apply-patch+yaml":
		return true
	}
	return false
}

// kindForResource resolves a plural resource name to its registered built-in Kind
// by inverting the apiserver's own kind→resource convention over the scheme's
// known types. This is exact for every registered type (e.g. endpointslices →
// EndpointSlice), unlike a name-capitalization heuristic. ok is false when no
// built-in type in the group/version maps to the resource (custom resources).
func kindForResource(gv schema.GroupVersion, resource string) (schema.GroupVersionKind, bool) {
	for kind := range scheme.Scheme.KnownTypes(gv) {
		gvk := gv.WithKind(kind)
		if plural, _ := meta.UnsafeGuessKindToResource(gvk); plural.Resource == resource {
			return gvk, true
		}
	}
	return schema.GroupVersionKind{}, false
}

// stampTypeMeta returns obj with apiVersion/kind stamped, for a resource that
// maps to a known built-in Kind (a no-op via withKind if already present or
// the Kind is unknown/custom). Reads already do this (see handler.go's
// trySingleItemGet and watch_replay.go's withKind for streamed events); write
// responses need it too — client-go's typed Update/UpdateStatus calls
// round-trip an object fetched via Get/List, whose TypeMeta the apiserver
// strips on read (a well-known client-go quirk), so the request body often
// carries no kind/apiVersion at all. A real apiserver's response is always
// fully typed regardless; without stamping it here, a typed client-go decoder
// fails the response with `Object 'Kind' is missing` — exactly what broke the
// deployment controller's DeploymentStatus update path, found via the
// upstream conformance suite's "Deployment should run the lifecycle of a
// Deployment" spec.
func stampTypeMeta(group, version, resource string, obj json.RawMessage) json.RawMessage {
	gvk, ok := kindForResource(schema.GroupVersion{Group: group, Version: version}, resource)
	if !ok {
		return obj
	}
	apiVersion := version
	if group != "" {
		apiVersion = group + "/" + version
	}
	return withKind(obj, apiVersion, gvk.Kind)
}

// allowedMethods returns the Allow-header value for a write path shape, used on
// 405 responses (RFC 7231 §6.5.5): collection paths allow create; item paths
// allow the full CRUD set; the status subresource is read + update (no delete);
// any other subresource is read-only.
func allowedMethods(name, sub string) string {
	switch {
	case name == "":
		return "GET, HEAD, POST, DELETE"
	case sub == "":
		return "GET, HEAD, PUT, PATCH, DELETE"
	case sub == "status", sub == "scale":
		return "GET, HEAD, PUT, PATCH"
	default:
		return "GET, HEAD"
	}
}

// listPathFor builds the canonical list path for a GVR + namespace.
func listPathFor(group, version, resource, namespace string) string {
	base := "/api/" + version
	if group != "" {
		base = "/apis/" + group + "/" + version
	}
	if namespace != "" {
		return base + "/namespaces/" + namespace + "/" + resource
	}
	return base + "/" + resource
}

// namespacesIsScope reports whether a leading "namespaces" segment is the
// namespace-scoping keyword (/namespaces/<ns>/<resource>/…) rather than the core
// cluster-scoped "namespaces" resource itself (/api/v1/namespaces/<name>). rest
// is guaranteed to start with "namespaces" and have >= 2 elements.
func namespacesIsScope(group string, rest []string) bool {
	if group != "" {
		// Non-core groups have no core "namespaces" resource. Treat a leading
		// "namespaces" as the scoping keyword only when a namespaced resource
		// follows (.../namespaces/<ns>/<resource>); a bare .../namespaces/<name>
		// is left as an item of a (hypothetical) grouped "namespaces" resource.
		return len(rest) >= 3
	}
	switch len(rest) {
	case 2: // /api/v1/namespaces/<name> → the namespace object
		return false
	case 3: // /api/v1/namespaces/<name>/{status,finalize} → object subresource;
		//        /api/v1/namespaces/<ns>/<resource>       → namespaced list
		return rest[2] != "status" && rest[2] != "finalize"
	default: // 4+: /api/v1/namespaces/<ns>/<resource>/<name>[/<sub>]
		return true
	}
}

// parseWritePath parses a write target into GVR + namespace + name + subresource.
// name is empty for list-level (create) paths.
func parseWritePath(path string) (group, version, resource, namespace, name, subresource string) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	var rest []string
	switch {
	case len(parts) >= 3 && parts[0] == "api":
		version = parts[1]
		rest = parts[2:]
	case len(parts) >= 4 && parts[0] == "apis":
		group = parts[1]
		version = parts[2]
		rest = parts[3:]
	default:
		return
	}
	// rest is one of:
	//   [resource] | [resource name] | [resource name sub]
	//   [namespaces ns resource] | [... name] | [... name sub]
	//
	// A leading "namespaces" is the namespace-scoping keyword only when a real
	// namespaced resource follows. In the core group "namespaces" is ALSO a
	// cluster-scoped resource, so /api/v1/namespaces/<name>[/status|/finalize]
	// targets a namespace object — not a namespaced path (see namespacesIsScope).
	if len(rest) >= 2 && rest[0] == "namespaces" && namespacesIsScope(group, rest) {
		namespace = rest[1]
		rest = rest[2:]
	}
	switch len(rest) {
	case 1:
		resource = rest[0]
	case 2:
		resource, name = rest[0], rest[1]
	case 3:
		resource, name, subresource = rest[0], rest[1], rest[2]
	}
	return
}

// ── small JSON helpers ──────────────────────────────────────────────────────

// metaString reads metadata.<field> as a string ("" if absent/non-string).
func metaString(obj json.RawMessage, field string) string {
	var m struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(obj, &m); err != nil {
		return ""
	}
	raw, ok := m.Metadata[field]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// metaInt reads metadata.<field> as an int64 (0 if absent/non-number).
func metaInt(obj json.RawMessage, field string) int64 {
	var m struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(obj, &m); err != nil {
		return 0
	}
	raw, ok := m.Metadata[field]
	if !ok {
		return 0
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0
	}
	return n
}

// dropDeletedNamespaceItems removes items whose metadata.namespace is in dns
// (see overlay.deletedNamespaces) — used to cascade a namespace delete into
// read results and into deletecollection's item set, for both captured and
// overlay-created items.
func dropDeletedNamespaceItems(items []json.RawMessage, dns map[string]struct{}) []json.RawMessage {
	if len(dns) == 0 {
		return items
	}
	kept := items[:0]
	for _, it := range items {
		if _, gone := dns[metaString(it, "namespace")]; !gone {
			kept = append(kept, it)
		}
	}
	return kept
}

// isJSONObject reports whether b is a JSON object ("{...}"), rejecting null,
// arrays, and scalars — so client write bodies can't be e.g. "null".
func isJSONObject(b []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil
}

// mergeMeta returns obj with the given metadata fields set/overwritten. It is
// nil-safe: a null object or null metadata is treated as an empty object.
func mergeMeta(obj json.RawMessage, updates map[string]any) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(obj, &m); err != nil {
		return obj
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := m["metadata"]; ok {
		if err := json.Unmarshal(raw, &meta); err != nil || meta == nil {
			meta = map[string]json.RawMessage{}
		}
	}
	for k, v := range updates {
		if s, ok := v.(string); ok && s == "" {
			continue // don't write empty namespace/uid/etc.
		}
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		meta[k] = b
	}
	m["metadata"], _ = json.Marshal(meta)
	out, err := json.Marshal(m)
	if err != nil {
		return obj
	}
	return out
}

// protectSpecOnly returns next with .spec forced back to current's .spec,
// otherwise unchanged — the status subresource's real apiserver semantics
// (see e.g. pkg/registry/apps/deployment/strategy.go's
// deploymentStatusStrategy.PrepareForUpdate): only .spec is universally
// protected against a status-subresource write, while .status and .metadata
// (annotations, and often labels — this varies slightly per resource type
// upstream, but protecting only .spec is the one rule that holds everywhere)
// pass through from the submitted body. This matters in practice: the
// deployment controller sets `deployment.kubernetes.io/revision` on the
// Deployment itself via UpdateStatus (a full-object PUT to .../status), not a
// spec/metadata write — an earlier version of this code protected all of
// metadata too, which silently dropped that annotation and broke revision
// tracking for every Deployment reconciled by --with-controller-manager.
func protectSpecOnly(next, current json.RawMessage) json.RawMessage {
	return replaceField(next, "spec", current)
}

// replaceField returns base with top-level field set to the same field taken
// from src.
func replaceField(base json.RawMessage, field string, src json.RawMessage) json.RawMessage {
	var b map[string]json.RawMessage
	if err := json.Unmarshal(base, &b); err != nil || b == nil {
		return base
	}
	var s map[string]json.RawMessage
	if err := json.Unmarshal(src, &s); err != nil || s == nil {
		return base
	}
	if v, ok := s[field]; ok {
		b[field] = v
	} else {
		delete(b, field)
	}
	out, err := json.Marshal(b)
	if err != nil {
		return base
	}
	return out
}
