package server

import "encoding/json"

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
// discovery metadata (kstore.CaptureStore.MergeResourceInfo), so /apis and
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
	if crd.Spec.Group == "" || crd.Spec.Names.Plural == "" || crd.Spec.Names.Kind == "" {
		// A missing Kind would otherwise fall through to MergeResourceInfo's
		// kstore.ResourceToKind heuristic fallback, which is a plural-depluralizing
		// guess known to be wrong for most CRDs — skipping keeps a malformed
		// CRD body from polluting discovery with a made-up Kind.
		return
	}
	// MergeResourceInfo treats namespaced as authoritative and overwrites any
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
		h.store.MergeResourceInfo(crd.Spec.Group, v.Name, crd.Spec.Names.Plural, namespaced,
			crd.Spec.Names.Kind, crd.Spec.Names.Singular, crd.Spec.Names.ShortNames)
	}
}
