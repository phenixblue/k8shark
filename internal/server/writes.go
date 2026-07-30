package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	kstore "github.com/phenixblue/k8shark/internal/store"
)

// handleWrite services a create/update/patch/delete against the in-memory overlay
// (writable replay). It is only reached when h.overlay != nil; read-only replay
// keeps returning 405 for writes.
func (h *handler) handleWrite(w http.ResponseWriter, r *http.Request, path string) {
	h.syncEpoch() // reset-on-loop (re-synthesizes the scheduling node if needed)

	group, version, resource, namespace, name, sub := parseWritePath(strings.TrimSuffix(path, "/"))
	if resource == "" {
		h.writeStatus(w, http.StatusBadRequest, "unsupported write path: "+path)
		return
	}

	// A namespace deleted in the overlay takes its contents with it: any write to
	// an object in it (create, update, patch, or delete) is a 404, since the
	// namespace and everything in it are logically gone. Deleting the namespace
	// object itself has namespace=="" here, so it isn't caught by this check.
	if namespace != "" && h.overlay.isNamespaceDeleted(namespace) {
		writeJSON(w, http.StatusNotFound, notFoundStatus("", "namespaces", namespace))
		return
	}

	switch r.Method {
	case http.MethodPost:
		if name != "" { // create is a collection operation
			w.Header().Set("Allow", allowedMethods(name, sub))
			h.writeStatus(w, http.StatusMethodNotAllowed, "POST creates at a collection path, not an item path")
			return
		}
		h.overlayCreate(w, r, group, version, resource, namespace)
	case http.MethodPut:
		if name == "" {
			h.writeStatus(w, http.StatusBadRequest, "PUT requires an object name")
			return
		}
		if sub == "scale" {
			h.overlayScaleWrite(w, r, group, version, resource, namespace, name)
			return
		}
		h.overlayReplace(w, r, group, version, resource, namespace, name, sub)
	case http.MethodPatch:
		if name == "" {
			h.writeStatus(w, http.StatusBadRequest, "PATCH requires an object name")
			return
		}
		if sub == "scale" {
			h.overlayScaleWrite(w, r, group, version, resource, namespace, name)
			return
		}
		h.overlayPatch(w, r, group, version, resource, namespace, name, sub)
	case http.MethodDelete:
		if name == "" { // deletecollection: parseWritePath guarantees sub == "" here
			h.overlayDeleteCollection(w, r, group, version, resource, namespace)
			return
		}
		if sub != "" {
			w.Header().Set("Allow", allowedMethods(name, sub))
			h.writeStatus(w, http.StatusMethodNotAllowed, "cannot DELETE a subresource")
			return
		}
		h.overlayDelete(w, group, version, resource, namespace, name)
	default:
		w.Header().Set("Allow", allowedMethods(name, sub))
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
	body, ok := h.readObjectBody(w, r)
	if !ok {
		return
	}
	if gvk, known := kindForResource(schema.GroupVersion{Group: group, Version: version}, resource); known {
		body = defaultObject(gvk, body)
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
	// The effective namespace comes from the request path; a body namespace must
	// match it (a namespaced resource is created via its namespaced collection
	// path), rejecting "selecting" a namespace via the body on a cluster path.
	if h.identityMismatch(w, body, name, namespace) {
		return
	}

	// Create semantics: fail if the object already exists (in the overlay or the
	// replayed state), matching the kube-apiserver's 409 AlreadyExists.
	if h.currentObject(group, version, resource, namespace, name) != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"apiVersion": "v1", "kind": "Status", "status": "Failure", "reason": "AlreadyExists",
			"message": resource + " " + name + " already exists", "code": http.StatusConflict,
		})
		return
	}

	obj := h.storeNewObject(group, version, resource, namespace, name, body)
	writeJSON(w, http.StatusCreated, json.RawMessage(stampTypeMeta(group, version, resource, obj)))
}

// storeNewObject stamps and stores a brand-new object identity in the overlay,
// applying the same create-time side effects regardless of whether the create
// arrived via POST (overlayCreate) or a first Server-Side Apply PATCH
// (overlayApplyCreate): the Pod scheduling/pending shim, RV/uid/creationTimestamp
// stamping, and namespace-default synthesis. Callers are responsible for any
// pre-checks (identity match, AlreadyExists) — this always stores.
func (h *handler) storeNewObject(group, version, resource, namespace, name string, body json.RawMessage) json.RawMessage {
	if group == "" && resource == "pods" {
		// The apiserver stamps a freshly created Pod with status.phase=Pending; the
		// overlay has no registry doing that. Replicate it — both for fidelity and
		// because KWOK's pod-ready stage selects on phase=Pending. See #160.
		body = ensurePodStatusPending(body)
		// Scheduling shim: a real cluster's scheduler assigns spec.nodeName, and
		// KWOK's "Pod → Running" stage only fires once a Pod is bound to a node.
		// Replay has no scheduler, so bind an unscheduled Pod here (round-robin over
		// the known nodes, synthesizing one if the capture has none).
		if h.schedulePods && podNodeName(body) == "" {
			body = h.schedulePod(body)
		}
	}
	if group == "apiextensions.k8s.io" && resource == "customresourcedefinitions" {
		// A real apiextensions-apiserver establishes a freshly created CRD
		// (status.conditions Established/NamesAccepted) within moments of
		// creation — internal to kube-apiserver, not something
		// --with-controller-manager's curated kube-controller-manager set runs.
		// Without it, kstatus (which Helm v4's --wait uses) reports the CRD
		// "InProgress: Install in progress" forever, hanging any
		// CRD-heavy chart (e.g. Istio's `base` chart) waiting for its CRDs.
		body = ensureCRDEstablished(body, h.nowRFC3339())
		// A real apiextensions-apiserver also registers the CRD's defined type
		// with the aggregated discovery document the moment it's created. The
		// store's resourceInfo is otherwise a snapshot built once from the
		// capture archive (see kstore.CaptureStore.buildResourceInfo/kstore.LoadStore), so
		// without this a CRD applied at runtime (e.g. `istioctl install`) is
		// visible via `kubectl get crd` (a plain object read) but absent from
		// `kubectl api-resources` / `istioctl analyze` (which walk discovery),
		// since /apis and /apis/<group>/<version> only ever reflect that
		// snapshot.
		h.registerCRDResourceInfo(body)
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
	// A real cluster's controllers auto-provision a `default` ServiceAccount and
	// a `kube-root-ca.crt` ConfigMap in every new namespace. The overlay has no
	// controllers, so synthesize them — otherwise clients (and the e2e framework)
	// that wait for them hang. `name` is the new namespace (cluster-scoped).
	if group == "" && resource == "namespaces" {
		h.ensureNamespaceDefaults(name)
	}
	return obj
}

// ensureNamespaceDefaults synthesizes the per-namespace objects a real cluster's
// controllers create: the `default` ServiceAccount (modern Kubernetes provisions
// no token Secret for it, so a bare object suffices) and the `kube-root-ca.crt`
// ConfigMap that the root-CA controller publishes.
func (h *handler) ensureNamespaceDefaults(namespace string) {
	h.synthesizeOverlayObject("serviceaccounts", namespace, "default",
		`{"apiVersion":"v1","kind":"ServiceAccount"}`)
	h.synthesizeOverlayObject("configmaps", namespace, "kube-root-ca.crt",
		`{"apiVersion":"v1","kind":"ConfigMap","data":{"ca.crt":"k8shark-replay-placeholder"}}`)
}

// synthesizeOverlayObject stores a synthetic core/v1 object in the overlay with
// stamped metadata, unless one already exists at that path. The final store is
// atomic (storeIfAbsent), so concurrent callers can't create the same identity
// twice with different UIDs — the currentObject fast-path also skips objects that
// already exist in the replayed state.
func (h *handler) synthesizeOverlayObject(resource, namespace, name, base string) {
	if h.currentObject("", "v1", resource, namespace, name) != nil {
		return
	}
	rv := h.overlay.nextRV(h.replayFloorRV("", "v1", resource, namespace))
	obj := mergeMeta(json.RawMessage(base), map[string]any{
		"name":              name,
		"namespace":         namespace,
		"uid":               uuid.New().String(),
		"resourceVersion":   strconv.FormatInt(rv, 10),
		"creationTimestamp": h.nowRFC3339(),
	})
	h.overlay.storeIfAbsent("", "v1", resource, namespace, name, obj, rv)
}

// syncEpoch applies the overlay's reset-on-loop and, when a reset occurred,
// re-synthesizes the scheduling node if needed. The synthetic node lives in the
// overlay, so a loop wrap would otherwise drop it — leaving a nodeless capture
// with no node for KWOK to manage until the next write. Call this instead of
// h.overlay.syncEpoch directly on read/write entry points.
func (h *handler) syncEpoch() {
	if h.overlay == nil {
		return
	}
	if h.overlay.syncEpoch(h.clock) && h.schedulePods {
		h.ensureSchedulableNode()
	}
}

func (h *handler) overlayReplace(w http.ResponseWriter, r *http.Request, group, version, resource, namespace, name, sub string) {
	if name == "" {
		h.writeStatus(w, http.StatusBadRequest, "object name is required")
		return
	}
	if sub != "" && sub != "status" {
		w.Header().Set("Allow", allowedMethods(name, sub))
		h.writeStatus(w, http.StatusMethodNotAllowed, "unsupported subresource: "+sub)
		return
	}
	body, ok := h.readObjectBody(w, r)
	if !ok {
		return
	}
	if sub != "status" {
		if gvk, known := kindForResource(schema.GroupVersion{Group: group, Version: version}, resource); known {
			body = defaultObject(gvk, body)
		}
	}
	if h.identityMismatch(w, body, name, namespace) {
		return
	}
	// PUT is update, not upsert: the object must already exist (in the overlay or
	// the replay state). This also keeps status updates on missing objects a 404,
	// matching the kube-apiserver.
	current := h.currentObject(group, version, resource, namespace, name)
	if current == nil {
		writeJSON(w, http.StatusNotFound, notFoundStatus(group, resource, name))
		return
	}
	var next json.RawMessage
	if sub == "status" {
		next = protectSpecOnly(body, current)
	} else {
		next = body
	}
	// Status updates don't bump generation (which tracks spec changes).
	next = h.stampUpdate(next, current, group, version, resource, namespace, name, sub != "status")
	writeJSON(w, http.StatusOK, json.RawMessage(stampTypeMeta(group, version, resource, next)))
}

func (h *handler) overlayPatch(w http.ResponseWriter, r *http.Request, group, version, resource, namespace, name, sub string) {
	if name == "" {
		h.writeStatus(w, http.StatusBadRequest, "object name is required")
		return
	}
	if sub != "" && sub != "status" {
		w.Header().Set("Allow", allowedMethods(name, sub))
		h.writeStatus(w, http.StatusMethodNotAllowed, "unsupported subresource: "+sub)
		return
	}
	if !supportedPatchType(r.Header.Get("Content-Type")) {
		h.writeStatus(w, http.StatusUnsupportedMediaType,
			"unsupported patch Content-Type "+strconv.Quote(r.Header.Get("Content-Type")))
		return
	}
	patch, err := io.ReadAll(io.LimitReader(r.Body, maxWriteBytes))
	if err != nil {
		h.writeStatus(w, http.StatusBadRequest, "reading body: "+err.Error())
		return
	}
	current := h.currentObject(group, version, resource, namespace, name)
	if current == nil {
		// Server-Side Apply (Content-Type: application/apply-patch+yaml) is
		// create-or-update, matching the real apiserver: a first apply to an
		// object that doesn't exist yet creates it, rather than 404ing. This is
		// how `helm install --create-namespace`, `kubectl apply --server-side`,
		// and CRD/webhook installers all provision brand-new objects — without
		// this, every first-apply to a not-yet-existing object in the overlay
		// fails with "object not found". The status subresource can't
		// create its parent object, so it keeps the 404.
		if sub == "" && patchMediaType(r.Header.Get("Content-Type")) == "application/apply-patch+yaml" {
			h.overlayApplyCreate(w, group, version, resource, namespace, name, patch)
			return
		}
		writeJSON(w, http.StatusNotFound, notFoundStatus(group, resource, name))
		return
	}
	next, perr := applyPatch(current, patch, r.Header.Get("Content-Type"), group, version, resource)
	if perr != nil {
		h.writeStatus(w, http.StatusUnprocessableEntity, "applying patch: "+perr.Error())
		return
	}
	if !isJSONObject(next) {
		h.writeStatus(w, http.StatusUnprocessableEntity, "patch did not produce a JSON object")
		return
	}
	// A patch (e.g. `kubectl apply`) can just as easily leave a defaultable
	// field unset as a create/replace can — default the patched result the
	// same way, so a controller reconciling an applied object doesn't panic
	// on a field the apiserver would have defaulted (see defaultObject).
	if sub != "status" {
		if gvk, known := kindForResource(schema.GroupVersion{Group: group, Version: version}, resource); known {
			next = defaultObject(gvk, next)
		}
	}
	if h.identityMismatch(w, next, name, namespace) {
		return
	}
	// A status-subresource patch protects .spec; don't bump generation (which
	// tracks spec changes, and spec remains unchanged either way).
	if sub == "status" {
		next = protectSpecOnly(next, current)
	}
	next = h.stampUpdate(next, current, group, version, resource, namespace, name, sub != "status")
	writeJSON(w, http.StatusOK, json.RawMessage(stampTypeMeta(group, version, resource, next)))
}

// overlayApplyCreate handles a Server-Side Apply PATCH targeting an object
// identity that doesn't exist yet: the patch's YAML body is the entire desired
// object (there's no `current` to merge onto), so it's decoded and stored
// through the same create path as a POST (storeNewObject), rather than
// jsonpatch-merged. Responds 201, matching a real apiserver's response to a
// first apply that creates an object.
func (h *handler) overlayApplyCreate(w http.ResponseWriter, group, version, resource, namespace, name string, patch []byte) {
	j, err := yaml.YAMLToJSON(patch)
	if err != nil {
		h.writeStatus(w, http.StatusBadRequest, "decoding apply patch: "+err.Error())
		return
	}
	if !isJSONObject(j) {
		h.writeStatus(w, http.StatusUnprocessableEntity, "apply patch did not produce a JSON object")
		return
	}
	body := json.RawMessage(j)
	if gvk, known := kindForResource(schema.GroupVersion{Group: group, Version: version}, resource); known {
		body = defaultObject(gvk, body)
	}
	if h.identityMismatch(w, body, name, namespace) {
		return
	}
	obj := h.storeNewObject(group, version, resource, namespace, name, body)
	writeJSON(w, http.StatusCreated, json.RawMessage(stampTypeMeta(group, version, resource, obj)))
}

// deleteOneObject tombstones a single object identity if it currently exists
// (in the overlay or the replay state as of h.at/h.clock.Now()), cascading a
// namespace delete if the identity is itself a core Namespace. Returns the
// deleted object's last-known body, or nil if there was nothing to delete —
// the identity was already gone (e.g. concurrently deleted between
// deletecollection's item scan and this call). Re-checking liveness here
// (rather than trusting an earlier list snapshot) keeps the DELETED watch
// event's body fresh and makes a repeated call for the same identity a safe
// no-op instead of a duplicate tombstone.
func (h *handler) deleteOneObject(group, version, resource, namespace, name string, floorRV int64) json.RawMessage {
	last := h.currentObject(group, version, resource, namespace, name)
	if last == nil {
		return nil
	}
	h.overlay.del(group, version, resource, namespace, name, last, floorRV)
	// Deleting a namespace cascades to its contents (no namespace controller runs
	// against the overlay): tombstone the namespace's overlay objects, and its
	// captured objects are filtered out of reads while the namespace is deleted.
	if isCoreNamespace(group, version, resource) {
		h.overlay.cascadeDeleteNamespace(name)
	}
	return last
}

func (h *handler) overlayDelete(w http.ResponseWriter, group, version, resource, namespace, name string) {
	if h.deleteOneObject(group, version, resource, namespace, name,
		h.replayFloorRV(group, version, resource, namespace)) == nil {
		writeJSON(w, http.StatusNotFound, notFoundStatus(group, resource, name))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "v1", "kind": "Status", "status": "Success",
		"details": map[string]any{"name": name, "kind": kstore.ResourceToKind(resource)},
	})
}

// overlayDeleteCollection implements Kubernetes deletecollection: it deletes
// every object currently visible for a list scope (group/version/resource,
// and namespace — empty for a cluster-scoped resource) that matches the
// request's labelSelector/fieldSelector. Always responds 200 with a Status
// Success, even when zero items matched — an empty deletecollection is not an
// error, matching the real apiserver. The request body (DeleteOptions) is
// intentionally ignored, mirroring overlayDelete/deleteOneObject.
func (h *handler) overlayDeleteCollection(w http.ResponseWriter, r *http.Request, group, version, resource, namespace string) {
	items, err := h.currentListItems(group, version, resource, namespace)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, statusObj(http.StatusInternalServerError, err.Error()))
		return
	}
	// Unlike a read (kstore.ApplySelectors/kstore.FilterItems, deliberately best-effort — a
	// malformed selector there just means "show more than intended"), a
	// malformed or unsupported selector here would mean "delete more than
	// intended" — kstore.FilterItemsStrict parses with apimachinery's real selector
	// grammar and 400s on anything malformed, rather than silently matching
	// everything.
	msg, filtered := kstore.FilterItemsStrict(items, r.URL.Query().Get("labelSelector"), r.URL.Query().Get("fieldSelector"))
	if msg != "" {
		h.writeStatus(w, http.StatusBadRequest, msg)
		return
	}
	items = filtered

	// floors caches replayFloorRV per namespace: almost always one namespace (the
	// request's), but a cluster-wide request against a namespaced resource (see
	// the fallback below `namespace == ""`) can span several — each needs its own
	// floor so a delete's RV exceeds that specific namespace's watchers, not just
	// the request scope's.
	floors := map[string]int64{}
	for _, it := range items {
		name := metaString(it, "name")
		if name == "" {
			continue // malformed/nameless item — nothing to key a delete on
		}
		ns := namespace
		if ns == "" {
			ns = metaString(it, "namespace") // cluster-scoped resource, or a cluster-wide request spanning namespaces
		}
		floor, ok := floors[ns]
		if !ok {
			floor = h.replayFloorRV(group, version, resource, ns)
			floors[ns] = floor
		}
		// deleteOneObject's return is deliberately ignored: an item already gone
		// (e.g. concurrently deleted) is a silent no-op, matching deletecollection's
		// best-effort-over-a-listed-set semantics rather than a transaction.
		h.deleteOneObject(group, version, resource, ns, name, floor)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "v1", "kind": "Status", "status": "Success",
		"details": map[string]any{"kind": kstore.ResourceToKind(resource)},
	})
}

// currentListItems returns the merged items for a list scope as of the
// replay clock: the captured base (if any — a 404/empty capture is not an
// error, just zero items) with the overlay applied (overlay wins; tombstones
// removed; overlay-only creates appended), with items in an overlay-deleted
// namespace dropped. This is overlayDeleteCollection's item source — the same
// merge that mergeOverlayList performs for LIST responses, but returning
// items directly since there's no HTTP list body to build here.
func (h *handler) currentListItems(group, version, resource, namespace string) ([]json.RawMessage, error) {
	at := h.at
	if h.clock != nil {
		at = h.clock.Now()
	}
	items, err := h.reconstructListItems(listPathFor(group, version, resource, namespace), at)
	if err != nil {
		return nil, err
	}
	switch {
	case items == nil && namespace != "":
		// The namespaced list was never captured on its own path (e.g. only the
		// cluster-scoped path was captured) — fall back to it and filter by
		// namespace, mirroring serveResource's read-path fallback (handler.go) so
		// deletecollection sees the same items a GET/LIST would, rather than
		// silently no-oping on captured data it can't see.
		clusterItems, cerr := h.reconstructListItems(listPathFor(group, version, resource, ""), at)
		if cerr != nil {
			return nil, cerr
		}
		for _, it := range clusterItems {
			if metaString(it, "namespace") == namespace {
				items = append(items, it)
			}
		}
	case items == nil && namespace == "":
		// The cluster-wide list was never captured on its own path either — fall
		// back to aggregating it from per-namespace captures, mirroring
		// serveResource's AggregateAcrossNamespaces fallback, so a cluster-wide
		// deletecollection (e.g. DELETE /api/v1/pods) sees the same items a
		// cluster-wide GET/LIST would.
		aggBody, aggCode, aerr := h.store.AggregateAcrossNamespaces(listPathFor(group, version, resource, ""), at)
		if aerr != nil {
			return nil, aerr
		}
		if aggCode == 200 {
			var list struct {
				Items []json.RawMessage `json:"items"`
			}
			if json.Unmarshal(aggBody, &list) == nil {
				items = list.Items
			}
		}
	}

	items, _ = h.overlay.applyToList(group, version, resource, namespace, items)
	return dropDeletedNamespaceItems(items, h.overlay.deletedNamespaces()), nil
}

// reconstructListItems reconstructs a captured list at `at` and returns its
// items. Returns nil (not an error) when nothing was captured at that exact
// path (a non-200 reconstruction), or when the 200 body isn't list-shaped
// (e.g. a Table-format or other non-list snapshot — kstore.CaptureStore.ReconstructAt
// is deliberately tolerant of those and returns them unchanged, so failing to
// decode "items" here is best-effort, not a hard error) — either way,
// currentListItems' overlay merge still applies on top. A genuine store error
// (decompression failure, etc.) still propagates.
func (h *handler) reconstructListItems(path string, at time.Time) ([]json.RawMessage, error) {
	body, code, err := h.store.ReconstructAt(path, at)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, nil // genuinely not captured at this path — callers fall back
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil || list.Items == nil {
		// A 200 response that isn't list-shaped (e.g. a captured Table snapshot)
		// or has no "items" field is a captured empty list, not "not captured" —
		// a non-nil empty slice, so currentListItems' nil-triggered
		// cluster/aggregation fallback doesn't kick in here. A GET/LIST on the
		// same path wouldn't fall back either (serveResource only falls back on
		// an actual 404), so deletecollection must operate on the same item set.
		return []json.RawMessage{}, nil
	}
	return list.Items, nil
}

// identityMismatch writes a 400 and returns true when an object body's
// metadata.name/metadata.namespace (when set) disagrees with the request path,
// matching the kube-apiserver's rejection of mismatched identities.
func (h *handler) identityMismatch(w http.ResponseWriter, obj json.RawMessage, name, namespace string) bool {
	if bn := metaString(obj, "name"); bn != "" && bn != name {
		h.writeStatus(w, http.StatusBadRequest,
			fmt.Sprintf("metadata.name %q does not match the request path name %q", bn, name))
		return true
	}
	if bns := metaString(obj, "namespace"); bns != "" && bns != namespace {
		h.writeStatus(w, http.StatusBadRequest,
			fmt.Sprintf("metadata.namespace %q does not match the request path namespace %q", bns, namespace))
		return true
	}
	return false
}

// stampUpdate assigns a fresh RV (and preserves uid/creationTimestamp from the
// current object), stores the object, and returns it. bumpGen controls whether
// metadata.generation advances — a spec change bumps it; a status update does not.
func (h *handler) stampUpdate(next, current json.RawMessage, group, version, resource, namespace, name string, bumpGen bool) json.RawMessage {
	updates := map[string]any{"name": name, "namespace": namespace}
	curGen := metaInt(current, "generation")
	switch {
	case bumpGen && curGen > 0:
		updates["generation"] = curGen + 1
	case bumpGen:
		updates["generation"] = int64(1)
	case curGen > 0:
		updates["generation"] = curGen // preserve on status updates
	}
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

// objectForRead resolves a single object for a read (GET-style) request,
// correctly whether or not the overlay is enabled. currentObject always calls
// h.overlay.get, which panics on a nil *overlay — safe only on the write path,
// which is reached exclusively when h.overlay != nil. This is the read-path
// equivalent, used by serveScale (GET works in read-only replay too, unlike
// scale writes).
func (h *handler) objectForRead(group, version, resource, namespace, name string, at time.Time) (json.RawMessage, bool) {
	if h.overlay != nil {
		obj := h.currentObject(group, version, resource, namespace, name)
		return obj, obj != nil
	}
	body, code := h.trySingleItemGet(listPathFor(group, version, resource, namespace)+"/"+name, at)
	return body, code == 200
}

func (h *handler) nowRFC3339() string {
	if h.clock != nil {
		return h.clock.Now().UTC().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// maxWriteBytes caps request bodies accepted by the overlay.
const maxWriteBytes = 8 << 20 // 8 MiB
