package server

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
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
		h.writeStatus(w, http.StatusNotFound, "namespace "+namespace+" was deleted in the writable overlay")
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
		// capture archive (see CaptureStore.buildResourceInfo/LoadStore), so
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

// defaultSyntheticNode is the node synthesized for scheduling when the capture
// contains none.
const defaultSyntheticNode = "kwok-node-0"

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

// ensureCRDEstablished synthesizes the status a real apiextensions-apiserver
// stamps on a newly created CustomResourceDefinition — NamesAccepted and
// Established conditions, acceptedNames, and storedVersions — matching what
// kstatus (and `kubectl wait --for condition=Established`) look for. now is
// used as every timestamp field's value (this is a one-shot synthesis at
// creation, not a real transition history).
func ensureCRDEstablished(body json.RawMessage, now string) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body
	}
	spec, _ := m["spec"].(map[string]any)
	names, ok := spec["names"].(map[string]any)
	if !ok || names == nil {
		names = map[string]any{} // non-nil: a real CRD's acceptedNames is never omitted
	}
	storedVersions := []string{} // non-nil: a real CRD's storedVersions is never omitted
	if versions, ok := spec["versions"].([]any); ok {
		for _, v := range versions {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if storage, _ := vm["storage"].(bool); storage {
				if n, ok := vm["name"].(string); ok {
					storedVersions = append(storedVersions, n)
				}
			}
		}
	} else if v, ok := spec["version"].(string); ok && v != "" {
		// Legacy apiextensions.k8s.io/v1beta1 CRDs could specify a single
		// top-level spec.version instead of the spec.versions list v1
		// introduced; treat it as the (only) stored version.
		storedVersions = append(storedVersions, v)
	}
	m["status"] = map[string]any{
		"acceptedNames": names,
		"conditions": []map[string]any{
			{
				"type": "NamesAccepted", "status": "True",
				"reason": "NoConflicts", "message": "no conflicts found",
				"lastTransitionTime": now,
			},
			{
				"type": "Established", "status": "True",
				"reason": "InitialNamesAccepted", "message": "the initial names have been accepted",
				"lastTransitionTime": now,
			},
		},
		"storedVersions": storedVersions,
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// registerCRDResourceInfo parses a freshly created CustomResourceDefinition's
// spec and registers its defined group/version/resource/kind with the store's
// discovery metadata (CaptureStore.mergeResourceInfo), so /apis and
// /apis/<group>/<version> — and therefore `kubectl api-resources` and any
// client that walks discovery (e.g. `istioctl analyze`) — reflect it
// immediately, rather than only once the archive is reloaded. Best-effort: a
// malformed/unparseable CRD body is silently skipped rather than failing the
// create — the CRD object itself is still stored either way; only its
// discovery visibility is affected.
func (h *handler) registerCRDResourceInfo(body json.RawMessage) {
	type crdVersion struct {
		Name   string `json:"name"`
		Served bool   `json:"served"`
	}
	var crd struct {
		Spec struct {
			Group string `json:"group"`
			Names struct {
				Kind       string   `json:"kind"`
				Singular   string   `json:"singular"`
				Plural     string   `json:"plural"`
				ShortNames []string `json:"shortNames"`
			} `json:"names"`
			Scope    string       `json:"scope"`
			Versions []crdVersion `json:"versions"`
			Version  string       `json:"version"` // legacy apiextensions.k8s.io/v1beta1
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &crd); err != nil {
		return
	}
	if crd.Spec.Group == "" || crd.Spec.Names.Plural == "" {
		return
	}
	// mergeResourceInfo treats namespaced as authoritative and overwrites any
	// existing value, so an empty/unrecognized spec.scope (a malformed body)
	// must be skipped entirely rather than defaulting to namespaced=true.
	var namespaced bool
	switch crd.Spec.Scope {
	case "Namespaced":
		namespaced = true
	case "Cluster":
		namespaced = false
	default:
		return
	}
	versions := crd.Spec.Versions
	if len(versions) == 0 && crd.Spec.Version != "" {
		// Legacy v1beta1 CRDs specify a single top-level spec.version instead
		// of the spec.versions list v1 introduced; treat it as the (only)
		// served version, mirroring ensureCRDEstablished's storedVersions
		// fallback above.
		versions = []crdVersion{{Name: crd.Spec.Version, Served: true}}
	}
	for _, v := range versions {
		if !v.Served || v.Name == "" {
			continue
		}
		h.store.mergeResourceInfo(crd.Spec.Group, v.Name, crd.Spec.Names.Plural, namespaced,
			crd.Spec.Names.Kind, crd.Spec.Names.Singular, crd.Spec.Names.ShortNames)
	}
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
		h.writeStatus(w, http.StatusNotFound, "object not found: "+name)
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
		h.writeStatus(w, http.StatusNotFound, "object not found: "+name)
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
		h.writeStatus(w, http.StatusNotFound, "object not found: "+name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": "v1", "kind": "Status", "status": "Success",
		"details": map[string]any{"name": name, "kind": resourceToKind(resource)},
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
	// Unlike a read (applySelectors/filterItems, deliberately best-effort — a
	// malformed selector there just means "show more than intended"), a
	// malformed or unsupported selector here would mean "delete more than
	// intended" — filterItemsStrict parses with apimachinery's real selector
	// grammar and 400s on anything malformed, rather than silently matching
	// everything.
	msg, filtered := filterItemsStrict(items, r.URL.Query().Get("labelSelector"), r.URL.Query().Get("fieldSelector"))
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
		"details": map[string]any{"kind": resourceToKind(resource)},
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
// (e.g. a Table-format or other non-list snapshot — CaptureStore.ReconstructAt
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

// scaleSubresourceResources are the built-in Kinds with a real /scale
// subresource on a live apiserver (Deployment, ReplicaSet, StatefulSet,
// ReplicationController) — anything else 404s, matching upstream.
var scaleSubresourceResources = map[string]bool{
	"deployments":            true,
	"replicasets":            true,
	"statefulsets":           true,
	"replicationcontrollers": true,
}

// scaleSelectorString renders a resource's .spec.selector as the label-query
// string HPA (and kubectl) read from a Scale's status.selector — handling
// both selector shapes real Kubernetes types use: Deployment/ReplicaSet/
// StatefulSet's structured metav1.LabelSelector ({matchLabels,
// matchExpressions}), and ReplicationController's plain map[string]string.
// Trying the map shape first is required, not just sufficient: unmarshaling a
// flat map like {"app":"foo"} into a LabelSelector struct silently succeeds
// with a zero-value result (unknown JSON fields are ignored on structs), so
// checking LabelSelector first would always win and produce an empty
// selector for every ReplicationController. A real LabelSelector's fields
// nest an object/array under "matchLabels"/"matchExpressions", which cannot
// decode into a map[string]string value, so that shape reliably fails first.
func scaleSelectorString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var flat map[string]string
	if err := json.Unmarshal(raw, &flat); err == nil {
		return labels.SelectorFromSet(flat).String()
	}
	var sel metav1.LabelSelector
	if err := json.Unmarshal(raw, &sel); err != nil {
		return ""
	}
	selector, err := metav1.LabelSelectorAsSelector(&sel)
	if err != nil {
		return ""
	}
	return selector.String()
}

// scaleObject synthesizes an autoscaling/v1 Scale representation of a
// Deployment/ReplicaSet/StatefulSet/ReplicationController — the real
// apiserver does the same conversion server-side for its generic scale
// subresource (e.g. pkg/registry/apps/deployment/storage's scaleClient).
func scaleObject(namespace, name string, obj json.RawMessage) (json.RawMessage, error) {
	var o struct {
		Metadata struct {
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Spec struct {
			Replicas *int32          `json:"replicas"`
			Selector json.RawMessage `json:"selector"`
		} `json:"spec"`
		Status struct {
			Replicas int32 `json:"replicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(obj, &o); err != nil {
		return nil, err
	}
	var replicas int32
	if o.Spec.Replicas != nil {
		replicas = *o.Spec.Replicas
	}
	// HPA (and kubectl) reads status.selector to count matching Pods directly,
	// rather than via the scaled resource's own selector field.
	selectorStr := scaleSelectorString(o.Spec.Selector)
	return json.Marshal(map[string]any{
		"apiVersion": "autoscaling/v1",
		"kind":       "Scale",
		"metadata": map[string]any{
			"name":            name,
			"namespace":       namespace,
			"uid":             o.Metadata.UID,
			"resourceVersion": o.Metadata.ResourceVersion,
		},
		"spec":   map[string]any{"replicas": replicas},
		"status": map[string]any{"replicas": o.Status.Replicas, "selector": selectorStr},
	})
}

// scaleReplicas extracts .spec.replicas from a Scale-shaped body (the PUT
// body, or a patch already applied onto a synthesized current Scale).
func scaleReplicas(body []byte) (int32, error) {
	var s struct {
		Spec struct {
			Replicas int32 `json:"replicas"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return 0, err
	}
	return s.Spec.Replicas, nil
}

// setSpecReplicas returns body with spec.replicas set, preserving the rest of
// the object. On a decode/encode error the body is returned unchanged.
func setSpecReplicas(body json.RawMessage, replicas int32) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return body
	}
	spec, ok := m["spec"].(map[string]any)
	if !ok || spec == nil {
		spec = map[string]any{}
		m["spec"] = spec
	}
	spec["replicas"] = replicas
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// overlayScaleWrite handles PUT/PATCH .../scale: only .spec.replicas on the
// underlying object is settable this way (matching the real apiserver's scale
// subresource — every other field, including the rest of .spec, is read-only
// through this path), so unlike overlayReplace/overlayPatch there's no
// defaultObject/identityMismatch handling here — those apply to the
// underlying resource's own body shape, not a Scale request.
func (h *handler) overlayScaleWrite(w http.ResponseWriter, r *http.Request, group, version, resource, namespace, name string) {
	if !scaleSubresourceResources[resource] {
		h.writeStatus(w, http.StatusNotFound, resource+" has no scale subresource")
		return
	}
	current := h.currentObject(group, version, resource, namespace, name)
	if current == nil {
		h.writeStatus(w, http.StatusNotFound, "object not found: "+name)
		return
	}

	var replicas int32
	if r.Method == http.MethodPut {
		body, ok := h.readObjectBody(w, r)
		if !ok {
			return
		}
		rep, err := scaleReplicas(body)
		if err != nil {
			h.writeStatus(w, http.StatusBadRequest, "decoding scale: "+err.Error())
			return
		}
		replicas = rep
	} else { // PATCH
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
		currentScale, err := scaleObject(namespace, name, current)
		if err != nil {
			h.writeStatus(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Scale has no listType/patchMergeKey fields to strategically merge, so
		// treat a strategic-merge-patch request the same as a plain merge patch
		// rather than resolving it against the underlying resource's own Kind
		// (which has an entirely different shape than Scale).
		patched, perr := jsonMergeOrPatch(currentScale, patch, r.Header.Get("Content-Type"))
		if perr != nil {
			h.writeStatus(w, http.StatusUnprocessableEntity, "applying patch: "+perr.Error())
			return
		}
		rep, err := scaleReplicas(patched)
		if err != nil {
			h.writeStatus(w, http.StatusUnprocessableEntity, "patch did not produce a valid scale object")
			return
		}
		replicas = rep
	}

	next := setSpecReplicas(current, replicas)
	next = h.stampUpdate(next, current, group, version, resource, namespace, name, true) // scaling is a spec change
	scale, err := scaleObject(namespace, name, next)
	if err != nil {
		h.writeStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(scale))
}

// jsonMergeOrPatch applies patch to current per contentType, supporting JSON
// Patch (RFC 6902), Server-Side Apply (application/apply-patch+yaml — the
// body is YAML, unlike every other supported type here, so it needs
// converting before merging same as the main write path's applyPatch), and
// treating anything else (merge-patch, strategic-merge, unspecified) as a
// plain JSON merge patch.
func jsonMergeOrPatch(current, patch []byte, contentType string) ([]byte, error) {
	switch patchMediaType(contentType) {
	case "application/json-patch+json":
		p, err := jsonpatch.DecodePatch(patch)
		if err != nil {
			return nil, err
		}
		return p.Apply(current)
	case "application/apply-patch+yaml":
		j, err := yaml.YAMLToJSON(patch)
		if err != nil {
			return nil, err
		}
		return jsonpatch.MergePatch(current, j)
	default:
		return jsonpatch.MergePatch(current, patch)
	}
}

func (h *handler) nowRFC3339() string {
	if h.clock != nil {
		return h.clock.Now().UTC().Format(time.RFC3339)
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// maxWriteBytes caps request bodies accepted by the overlay.
const maxWriteBytes = 8 << 20 // 8 MiB

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

// applyPatch applies a patch of the given (already-validated) content type onto
// an existing current object (the create-on-first-apply case is handled
// earlier, by overlayApplyCreate, before this is reached). Supports JSON merge
// patch, JSON patch (RFC 6902), and strategic-merge patch (for built-in types,
// via their registered schema). Server-side apply still falls back to a JSON
// merge patch for now (real SSA field management lands in a later PR).
func applyPatch(current, patch []byte, contentType, group, version, resource string) ([]byte, error) {
	switch patchMediaType(contentType) {
	case "application/json-patch+json":
		p, err := jsonpatch.DecodePatch(patch)
		if err != nil {
			return nil, err
		}
		return p.Apply(current)
	case "application/strategic-merge-patch+json":
		return strategicMergePatch(current, patch, group, version, resource)
	case "application/apply-patch+yaml":
		// Server-side apply bodies are YAML; convert to JSON, then merge as an
		// interim (real SSA field management lands in a later PR).
		j, err := yaml.YAMLToJSON(patch)
		if err != nil {
			return nil, err
		}
		return jsonpatch.MergePatch(current, j)
	default: // merge-patch
		return jsonpatch.MergePatch(current, patch)
	}
}

// strategicMergePatch applies a strategic-merge patch using the schema of the
// object's built-in type: lists with a patch merge key (e.g. containers by name)
// are merged element-wise rather than wholesale-replaced, matching the
// kube-apiserver. Strategic merge is only defined for built-in types — the real
// apiserver has no strategy metadata for custom resources — so a GVK that isn't
// in the scheme falls back to a plain JSON merge patch.
//
// The GVK is derived from the request path's group/version/resource, not the
// stored object body: a replayed object reconstructed from a captured LIST has no
// apiVersion/kind (the apiserver strips TypeMeta from list items), so the body is
// not a reliable type source.
func strategicMergePatch(current, patch []byte, group, version, resource string) ([]byte, error) {
	if gvk, ok := kindForResource(schema.GroupVersion{Group: group, Version: version}, resource); ok {
		if obj, err := scheme.Scheme.New(gvk); err == nil {
			return strategicpatch.StrategicMergePatch(current, patch, obj)
		}
	}
	// Unknown/custom type: no strategy metadata, so merge like the apiserver's
	// fallback for resources without a strategic-merge strategy.
	return jsonpatch.MergePatch(current, patch)
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

// defaultObject applies Kubernetes API defaulting to body — e.g. an empty
// Deployment.spec.strategy.type becomes "RollingUpdate" — matching what a real
// apiserver does on every write. The overlay has no apiserver behind it, so
// without this a freshly created object keeps whatever zero values the client
// didn't set; a real controller (e.g. kube-controller-manager's deployment
// controller) assumes defaulting already happened and errors on the zero value
// ("unexpected deployment strategy type: \"\"") instead of treating it as
// "use the default". Resources outside client-go's built-in scheme (CRDs) are
// returned unchanged — there's no way to know their defaults without the CRD's
// schema, which the overlay doesn't have.
//
// scheme.Scheme.Default only runs defaulters registered on the client-side
// scheme, which — for the built-in types this project vendors
// (k8s.io/api, not the full k8s.io/kubernetes apiserver) — is effectively
// none of the fields real controllers care about; the actual
// SetDefaults_Deployment-style functions live in k8s.io/kubernetes's internal
// packages, which aren't meant to be imported as a library. applyKnownDefaults
// hand-covers the specific, long-stable defaults our curated
// --with-controller-manager controllers rely on instead.
func defaultObject(gvk schema.GroupVersionKind, body json.RawMessage) json.RawMessage {
	typed, err := scheme.Scheme.New(gvk)
	if err != nil {
		return body
	}
	if err := json.Unmarshal(body, typed); err != nil {
		return body
	}
	// Marshal the client's own object back out before defaulting (rather than
	// reusing body directly) so the merge patch computed below only reflects
	// fields defaulting actually changed, not differences between body's
	// exact bytes/field order and typed's. Round-tripping through the typed
	// struct at all would normally risk silently dropping fields the
	// vendored k8s.io/api types don't know about (e.g. a newer API field) or
	// explicitly-sent zero values omitempty would elide — but since this
	// undefaulted marshal is only ever diffed against the defaulted one, not
	// returned, neither loss ends up in the result: a field defaulting
	// doesn't touch is absent from the diff either way, and the merge patch
	// below is applied onto the original body, preserving every field body
	// actually had.
	before, err := json.Marshal(typed)
	if err != nil {
		return body
	}
	scheme.Scheme.Default(typed)
	applyKnownDefaults(typed)
	after, err := json.Marshal(typed)
	if err != nil {
		return body
	}
	patch, err := jsonpatch.CreateMergePatch(before, after)
	if err != nil {
		return body
	}
	defaulted, err := jsonpatch.MergePatch(body, patch)
	if err != nil {
		return body
	}
	return defaulted
}

// applyKnownDefaults hand-applies the handful of long-stable Kubernetes API
// defaults that the controllers --with-controller-manager enables (see
// cmd/controllermanager.go) assume are already in place: a zero-valued
// strategy/update-strategy/concurrency-policy reads as "invalid", not "use the
// default", to those controllers. Defaulting a *Type without also defaulting
// its matching *RollingUpdate sub-struct is not enough — real
// kube-controller-manager code unconditionally dereferences that pointer
// (e.g. deployment_util.go's NewRSNewReplicas reads
// Spec.Strategy.RollingUpdate.MaxSurge), so a Type of "RollingUpdate" with a
// nil RollingUpdate struct panics deep inside the controller instead of
// erroring cleanly. The same function also dereferences
// *deployment.Spec.Replicas unconditionally, so a manifest that omits
// `replicas` (relying on the apiserver's default of 1, as e.g. Istio's charts
// do) panics the same way — hence Deployment/StatefulSet/ReplicaSet.Replicas
// are defaulted here too.

// isIPv6Address reports whether s parses as an IPv6 address. Used to infer a
// Service's address family from an explicit ClusterIP/ClusterIPs value
// rather than always assuming IPv4; returns false for "" and "None" (a
// headless Service), which correctly fall through to the IPv4 default.
func isIPv6Address(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() == nil
}

// syntheticLoadBalancerIP deterministically derives a fake external address
// for a LoadBalancer Service from its identity, landing in TEST-NET-3
// (203.0.113.0/24 — reserved by RFC 5737 for documentation/example use, so it
// can never collide with anything real). Deterministic rather than a counter
// so the same Service gets the same address across repeated writes instead of
// a new one every time defaultObject runs.
func syntheticLoadBalancerIP(namespace, name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(namespace + "/" + name))
	return fmt.Sprintf("203.0.113.%d", 1+h.Sum32()%254)
}

// syntheticClusterIP deterministically derives a fake ClusterIP for a Service
// from its identity, landing in 10.96.0.0/12 — the conventional kubeadm
// service-cluster-ip-range, so it looks at home next to a typical capture's
// real ClusterIPs. Deterministic (like syntheticLoadBalancerIP) rather than a
// counter, so the same Service keeps the same address across repeated writes.
func syntheticClusterIP(namespace, name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte("clusterip/" + namespace + "/" + name))
	sum := h.Sum32()
	// 10.96.0.0/12 spans 10.96.0.0-10.111.255.255: only the low 4 bits of the
	// second octet vary (96 = 0110_0000 through 111 = 0110_1111).
	return fmt.Sprintf("10.%d.%d.%d", 96+sum%16, (sum>>8)%256, 1+(sum>>16)%254)
}

func applyKnownDefaults(obj runtime.Object) {
	switch o := obj.(type) {
	case *corev1.Service:
		if o.Spec.Type == "" {
			o.Spec.Type = corev1.ServiceTypeClusterIP
		}
		if o.Spec.SessionAffinity == "" {
			o.Spec.SessionAffinity = corev1.ServiceAffinityNone
		}
		// The real apiserver's IP allocator assigns every Service a ClusterIP —
		// even LoadBalancer/NodePort ones — as soon as it's created; the overlay
		// has no IPAM, so nothing else ever populates this. kstatus's readiness
		// check for a LoadBalancer Service (which Helm v4's `--wait` uses via
		// sigs.k8s.io/cli-utils) specifically requires spec.clusterIP to be
		// non-empty — NOT status.loadBalancer.ingress — so a Service that never
		// gets one hangs `helm install --wait` forever regardless of the
		// synthesized external address below. A headless Service
		// (clusterIP: "None", set explicitly by the client) and ExternalName
		// Services are left alone — they don't get one on a real cluster either.
		if o.Spec.Type != corev1.ServiceTypeExternalName && o.Spec.ClusterIP == "" {
			ip := syntheticClusterIP(o.Namespace, o.Name)
			o.Spec.ClusterIP = ip
			if len(o.Spec.ClusterIPs) == 0 {
				o.Spec.ClusterIPs = []string{ip}
			}
		}
		if len(o.Spec.IPFamilies) == 0 {
			// The endpoint/endpointslice controllers index IPFamilies[0]
			// unconditionally; a real apiserver always populates this from the
			// cluster's configured service-cluster-ip-range. Infer the primary
			// family from the primary address only — ClusterIP if set, else
			// ClusterIPs[0] — never by scanning every ClusterIPs entry: for a
			// dual-stack Service with an IPv4 ClusterIP and an IPv6 secondary in
			// ClusterIPs, scanning all entries would pick IPv6 and produce
			// IPFamilies[0] inconsistent with the primary ClusterIP, sending the
			// endpoint controller down the wrong address family. IPv4 remains the
			// fallback when nothing indicates IPv6, matching every capture this
			// project has captured so far.
			family := corev1.IPv4Protocol
			switch {
			case o.Spec.ClusterIP != "" && o.Spec.ClusterIP != corev1.ClusterIPNone:
				if isIPv6Address(o.Spec.ClusterIP) {
					family = corev1.IPv6Protocol
				}
			case len(o.Spec.ClusterIPs) > 0:
				if isIPv6Address(o.Spec.ClusterIPs[0]) {
					family = corev1.IPv6Protocol
				}
			}
			o.Spec.IPFamilies = []corev1.IPFamily{family}
		}
		// A real cloud-controller-manager (or an on-prem equivalent like MetalLB)
		// eventually assigns a LoadBalancer Service an external address;
		// --with-controller-manager's curated set deliberately excludes
		// cloud-provider controllers (see cmd/controllermanager.go — no real cloud
		// provider to ask), so nothing else in the overlay ever populates this
		// field. Without it, `helm install --wait` and any other client polling
		// for a LoadBalancer Service to become ready hangs forever. Synthesize an
		// address deterministically from the Service's identity (rather than a
		// counter) so repeated writes to the same Service don't reassign it.
		if o.Spec.Type == corev1.ServiceTypeLoadBalancer && len(o.Status.LoadBalancer.Ingress) == 0 {
			o.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
				{IP: syntheticLoadBalancerIP(o.Namespace, o.Name)},
			}
		}
	case *appsv1.Deployment:
		if o.Spec.Replicas == nil {
			o.Spec.Replicas = ptr.To(int32(1))
		}
		if o.Spec.Strategy.Type == "" {
			o.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
		}
		if o.Spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType && o.Spec.Strategy.RollingUpdate == nil {
			maxUnavailable := intstr.FromString("25%")
			maxSurge := intstr.FromString("25%")
			o.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
				MaxUnavailable: &maxUnavailable,
				MaxSurge:       &maxSurge,
			}
		}
	case *appsv1.ReplicaSet:
		if o.Spec.Replicas == nil {
			o.Spec.Replicas = ptr.To(int32(1))
		}
	case *appsv1.DaemonSet:
		if o.Spec.UpdateStrategy.Type == "" {
			o.Spec.UpdateStrategy.Type = appsv1.RollingUpdateDaemonSetStrategyType
		}
		if o.Spec.UpdateStrategy.Type == appsv1.RollingUpdateDaemonSetStrategyType && o.Spec.UpdateStrategy.RollingUpdate == nil {
			maxUnavailable := intstr.FromString("25%")
			maxSurge := intstr.FromInt32(0)
			o.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateDaemonSet{
				MaxUnavailable: &maxUnavailable,
				MaxSurge:       &maxSurge,
			}
		}
		// pkg/controller/daemon/update.go's cleanupHistory dereferences
		// *ds.Spec.RevisionHistoryLimit unconditionally (`toKeep :=
		// int(*ds.Spec.RevisionHistoryLimit)`) — unlike the deployment
		// controller's equivalent cleanup, which nil-checks first
		// (HasRevisionHistoryLimit). A nil value here crashes the whole
		// daemonset controller goroutine (client-go's crash handler logs it,
		// then repanics — this isn't a swallowed panic like the others in this
		// function, it takes the controller-manager process down).
		if o.Spec.RevisionHistoryLimit == nil {
			o.Spec.RevisionHistoryLimit = ptr.To(int32(10))
		}
	case *appsv1.StatefulSet:
		if o.Spec.Replicas == nil {
			o.Spec.Replicas = ptr.To(int32(1))
		}
		if o.Spec.UpdateStrategy.Type == "" {
			o.Spec.UpdateStrategy.Type = appsv1.RollingUpdateStatefulSetStrategyType
		}
		// pkg/controller/statefulset/stateful_set_control.go's truncateHistory
		// dereferences *set.Spec.RevisionHistoryLimit unconditionally
		// (`historyLimit := int(*set.Spec.RevisionHistoryLimit)`) — same
		// unguarded-pointer class as DaemonSet's cleanupHistory above, and
		// likewise fatal to the whole controller-manager process, not just
		// that one StatefulSet's reconcile.
		if o.Spec.RevisionHistoryLimit == nil {
			o.Spec.RevisionHistoryLimit = ptr.To(int32(10))
		}
		if o.Spec.PodManagementPolicy == "" {
			o.Spec.PodManagementPolicy = appsv1.OrderedReadyPodManagement
		}
		if o.Spec.UpdateStrategy.Type == appsv1.RollingUpdateStatefulSetStrategyType && o.Spec.UpdateStrategy.RollingUpdate == nil {
			partition := int32(0)
			o.Spec.UpdateStrategy.RollingUpdate = &appsv1.RollingUpdateStatefulSetStrategy{Partition: &partition}
		}
	case *batchv1.Job:
		if o.Spec.Parallelism == nil {
			o.Spec.Parallelism = ptr.To(int32(1))
		}
		if o.Spec.BackoffLimit == nil {
			o.Spec.BackoffLimit = ptr.To(int32(6))
		}
		if o.Spec.CompletionMode == nil {
			o.Spec.CompletionMode = ptr.To(batchv1.NonIndexedCompletion)
		}
		if o.Spec.Suspend == nil {
			o.Spec.Suspend = ptr.To(false)
		}
		if o.Spec.ManualSelector == nil {
			o.Spec.ManualSelector = ptr.To(false)
		}
		if o.Spec.PodReplacementPolicy == nil {
			o.Spec.PodReplacementPolicy = ptr.To(batchv1.TerminatingOrFailed)
		}
	case *batchv1.CronJob:
		if o.Spec.ConcurrencyPolicy == "" {
			o.Spec.ConcurrencyPolicy = batchv1.AllowConcurrent
		}
		if o.Spec.Suspend == nil {
			o.Spec.Suspend = ptr.To(false)
		}
		if o.Spec.SuccessfulJobsHistoryLimit == nil {
			o.Spec.SuccessfulJobsHistoryLimit = ptr.To(int32(3))
		}
		if o.Spec.FailedJobsHistoryLimit == nil {
			o.Spec.FailedJobsHistoryLimit = ptr.To(int32(1))
		}
	}
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
