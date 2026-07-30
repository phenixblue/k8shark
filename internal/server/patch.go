package server

import (
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

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
