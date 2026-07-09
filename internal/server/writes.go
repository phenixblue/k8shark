package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
)

// handleWrite services a create/update/patch/delete against the in-memory overlay
// (writable replay). It is only reached when h.overlay != nil; read-only replay
// keeps returning 405 for writes.
func (h *handler) handleWrite(w http.ResponseWriter, r *http.Request, path string) {
	h.overlay.syncEpoch(h.clock) // reset-on-loop before touching state

	group, version, resource, namespace, name, sub := parseWritePath(strings.TrimSuffix(path, "/"))
	if resource == "" {
		h.writeStatus(w, http.StatusBadRequest, "unsupported write path: "+path)
		return
	}

	switch r.Method {
	case http.MethodPost:
		h.overlayCreate(w, r, group, version, resource, namespace)
	case http.MethodPut:
		h.overlayReplace(w, r, group, version, resource, namespace, name, sub)
	case http.MethodPatch:
		h.overlayPatch(w, r, group, version, resource, namespace, name, sub)
	case http.MethodDelete:
		h.overlayDelete(w, group, version, resource, namespace, name)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST, PUT, PATCH, DELETE")
		h.writeStatus(w, http.StatusMethodNotAllowed, "unsupported method "+r.Method)
	}
}

// replayFloorRV is the replay resourceVersion as-of the clock for the object's
// list path(s), so an overlay write's RV always exceeds the current replay RV.
func (h *handler) replayFloorRV(group, version, resource, namespace string) int64 {
	at := h.at
	if h.clock != nil {
		at = h.clock.Now()
	}
	floor := rvAsOf(h.timelineFor(listPathFor(group, version, resource, namespace)), at)
	if namespace != "" { // a cluster-wide watcher of the same resource has its own floor
		if c := rvAsOf(h.timelineFor(listPathFor(group, version, resource, "")), at); c > floor {
			floor = c
		}
	}
	return floor
}

func (h *handler) overlayCreate(w http.ResponseWriter, r *http.Request, group, version, resource, namespace string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBytes))
	if err != nil {
		h.writeStatus(w, http.StatusBadRequest, "reading body: "+err.Error())
		return
	}
	name := metaString(body, "name")
	if name == "" {
		if gn := metaString(body, "generateName"); gn != "" {
			name = gn + uuid.New().String()[:5]
		}
	}
	if name == "" {
		h.writeStatus(w, http.StatusBadRequest, "metadata.name or metadata.generateName is required")
		return
	}
	if bodyNS := metaString(body, "namespace"); bodyNS != "" && namespace == "" {
		namespace = bodyNS
	}

	rv := h.overlay.nextRV(h.replayFloorRV(group, version, resource, namespace))
	obj := mergeMeta(body, map[string]any{
		"name":              name,
		"namespace":         namespace,
		"uid":               uuid.New().String(),
		"resourceVersion":   strconv.FormatInt(rv, 10),
		"creationTimestamp": h.nowRFC3339(),
		"generation":        1,
	})
	h.overlay.store(group, version, resource, namespace, name, obj, rv)
	writeJSON(w, http.StatusCreated, json.RawMessage(obj))
}

func (h *handler) overlayReplace(w http.ResponseWriter, r *http.Request, group, version, resource, namespace, name, sub string) {
	if name == "" {
		h.writeStatus(w, http.StatusBadRequest, "object name is required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBytes))
	if err != nil {
		h.writeStatus(w, http.StatusBadRequest, "reading body: "+err.Error())
		return
	}
	current := h.currentObject(group, version, resource, namespace, name)
	var next json.RawMessage
	if sub == "status" && current != nil {
		next = replaceField(current, "status", body) // status subresource: only status changes
	} else {
		next = body
	}
	next = h.stampUpdate(next, current, group, version, resource, namespace, name)
	writeJSON(w, http.StatusOK, json.RawMessage(next))
}

func (h *handler) overlayPatch(w http.ResponseWriter, r *http.Request, group, version, resource, namespace, name, sub string) {
	if name == "" {
		h.writeStatus(w, http.StatusBadRequest, "object name is required")
		return
	}
	patch, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBytes))
	if err != nil {
		h.writeStatus(w, http.StatusBadRequest, "reading body: "+err.Error())
		return
	}
	current := h.currentObject(group, version, resource, namespace, name)
	if current == nil {
		h.writeStatus(w, http.StatusNotFound, "object not found: "+name)
		return
	}
	next, perr := applyPatch(current, patch, r.Header.Get("Content-Type"))
	if perr != nil {
		h.writeStatus(w, http.StatusUnprocessableEntity, "applying patch: "+perr.Error())
		return
	}
	next = h.stampUpdate(next, current, group, version, resource, namespace, name)
	writeJSON(w, http.StatusOK, json.RawMessage(next))
}

func (h *handler) overlayDelete(w http.ResponseWriter, group, version, resource, namespace, name string) {
	if name == "" {
		h.writeStatus(w, http.StatusBadRequest, "object name is required")
		return
	}
	last := h.currentObject(group, version, resource, namespace, name)
	if last == nil {
		h.writeStatus(w, http.StatusNotFound, "object not found: "+name)
		return
	}
	h.overlay.del(group, version, resource, namespace, name, last, h.replayFloorRV(group, version, resource, namespace))
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "v1", "kind": "Status", "status": "Success",
		"details": map[string]any{"name": name, "kind": resource},
	})
}

// stampUpdate assigns a fresh RV (and preserves uid/creationTimestamp from the
// current object), stores the object, and returns it.
func (h *handler) stampUpdate(next, current json.RawMessage, group, version, resource, namespace, name string) json.RawMessage {
	updates := map[string]any{"name": name, "namespace": namespace}
	if current != nil {
		if uid := metaString(current, "uid"); uid != "" {
			updates["uid"] = uid
		}
		if ct := metaString(current, "creationTimestamp"); ct != "" {
			updates["creationTimestamp"] = ct
		}
	}
	newRV := h.overlay.nextRV(h.replayFloorRV(group, version, resource, namespace))
	updates["resourceVersion"] = strconv.FormatInt(newRV, 10)
	obj := mergeMeta(next, updates)
	h.overlay.store(group, version, resource, namespace, name, obj, newRV)
	return obj
}

// currentObject returns the object as merged for reads: the overlay copy if
// present (nil if tombstoned), else the replay object as-of the clock.
func (h *handler) currentObject(group, version, resource, namespace, name string) json.RawMessage {
	if e, ok := h.overlay.get(group, version, resource, namespace, name); ok {
		if e.deleted {
			return nil
		}
		return e.obj
	}
	at := h.at
	if h.clock != nil {
		at = h.clock.Now()
	}
	body, code := h.trySingleItemGet(listPathFor(group, version, resource, namespace)+"/"+name, at)
	if code != 200 {
		return nil
	}
	return body
}

func (h *handler) nowRFC3339() string {
	if h.clock != nil {
		return h.clock.Now().UTC().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// maxWriteBytes caps request bodies accepted by the overlay.
const maxWriteBytes = 8 << 20 // 8 MiB

// applyPatch applies a patch of the given content type to the current object.
// PR-1 supports JSON merge patch and JSON patch (RFC 6902); strategic-merge and
// server-side apply fall back to a JSON merge patch for now (schema-driven
// strategic-merge and SSA land in later PRs).
func applyPatch(current, patch []byte, contentType string) ([]byte, error) {
	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch strings.TrimSpace(ct) {
	case "application/json-patch+json":
		p, err := jsonpatch.DecodePatch(patch)
		if err != nil {
			return nil, err
		}
		return p.Apply(current)
	default: // merge-patch, strategic-merge (fallback), apply-patch (fallback)
		return jsonpatch.MergePatch(current, patch)
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
	if len(rest) >= 2 && rest[0] == "namespaces" {
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

// mergeMeta returns obj with the given metadata fields set/overwritten.
func mergeMeta(obj json.RawMessage, updates map[string]any) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(obj, &m); err != nil {
		return obj
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := m["metadata"]; ok {
		_ = json.Unmarshal(raw, &meta)
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

// replaceField returns base with top-level field set to the same field taken
// from src (used for the status subresource: only status changes).
func replaceField(base json.RawMessage, field string, src json.RawMessage) json.RawMessage {
	var b map[string]json.RawMessage
	if err := json.Unmarshal(base, &b); err != nil {
		return base
	}
	var s map[string]json.RawMessage
	if err := json.Unmarshal(src, &s); err != nil {
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
