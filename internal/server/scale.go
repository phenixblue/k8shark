package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// serveScale handles GET .../scale: synthesizes an autoscaling/v1 Scale
// representation of the underlying Deployment/ReplicaSet/StatefulSet/
// ReplicationController — the only built-in Kinds with a real scale
// subresource on a live apiserver. Works in read-only replay too (unlike
// scale writes, which need --writable), by resolving the object via
// objectForRead instead of currentObject.
func (h *handler) serveScale(w http.ResponseWriter, group, version, resource, namespace, name string, at time.Time) {
	if !scaleSubresourceResources[resource] {
		h.writeStatus(w, http.StatusNotFound, resource+" has no scale subresource")
		return
	}
	obj, ok := h.objectForRead(group, version, resource, namespace, name, at)
	if !ok {
		writeJSON(w, http.StatusNotFound, notFoundStatus(group, resource, name))
		return
	}
	scale, err := scaleObject(namespace, name, obj)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, statusObj(500, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, json.RawMessage(scale))
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
