package anonymize

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/phenixblue/k8shark/internal/capture"
)

// containerListFields and containerStatusFields are the field names that
// hold a container's own image reference, at whatever depth they occur.
// Kept as name lists rather than per-Kind field paths (spec.template.spec
// for a Deployment, spec.jobTemplate.spec.template.spec for a CronJob, and
// so on) because every workload Kind embeds a PodTemplateSpec at a
// different nesting depth — walking the whole tree looking for these
// recognizable container-list shapes finds every one of them without
// needing a separate hardcoded path per Kind.
var containerListFields = []string{"containers", "initContainers", "ephemeralContainers"}
var containerStatusFields = []string{"containerStatuses", "initContainerStatuses", "ephemeralContainerStatuses"}

// containerFieldNames is the fixed set of the six field names above, built
// once rather than as a map literal per rewriteContainerImages call: the
// set of keys the recursive descent skips re-walking never changes call to
// call, so there is nothing to gain from reallocating it at every map node
// visited in the tree.
var containerFieldNames = func() map[string]bool {
	m := make(map[string]bool, len(containerListFields)+len(containerStatusFields))
	for _, f := range containerListFields {
		m[f] = true
	}
	for _, f := range containerStatusFields {
		m[f] = true
	}
	return m
}()

// rewriteImageRegistryInRecord decodes rec's body and rewrites the leading
// registry host of every container image reference it finds, wherever a
// recognizable containers/initContainers/ephemeralContainers (spec-side) or
// containerStatuses/initContainerStatuses/ephemeralContainerStatuses
// (status-side) list occurs in the tree — schema-aware in the sense that it
// only ever touches an "image"/"imageID" field inside one of those specific
// list shapes, never an arbitrary string anywhere in the body.
//
// V1 scope, per the design plan: only the leading registry host is
// rewritten (Docker's own heuristic for "is there an explicit registry
// here at all" — see registryHost). The repository path, tag, and digest
// are left untouched; full OCI reference grammar reconstruction is real,
// separate parsing work this milestone doesn't attempt.
func rewriteImageRegistryInRecord(rec *capture.Record, excluded excludedFunc, alias func(string) string) (bool, error) {
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
			if rewriteContainerImages(item, ik, "", excluded, alias) {
				items[i] = item
				modified = true
			}
		}
	} else if rewriteContainerImages(obj, kind, "", excluded, alias) {
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

// rewriteContainerImages recursively walks node, rewriting "image"/"imageID"
// string fields inside any container-list-shaped map it finds. kind is the
// enclosing object's own Kind (e.g. "Deployment") — fixed for the whole
// recursive descent, unlike path, which grows as the walk goes deeper
// (e.g. into a Deployment's spec.template.spec). Both are threaded through
// purely for exclusion-rule matching; nothing else in this function
// depends on kind at all, since the container-list shape it looks for is
// the same regardless of which Kind it's nested under.
func rewriteContainerImages(node interface{}, kind, path string, excluded excludedFunc, alias func(string) string) bool {
	switch v := node.(type) {
	case map[string]interface{}:
		modified := false

		for _, field := range containerListFields {
			list, ok := v[field].([]interface{})
			if !ok {
				continue
			}
			imagePath := joinFieldPath(path, field) + "[*].image"
			if excluded(CategoryImage, kind, imagePath) {
				continue
			}
			for _, raw := range list {
				c, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				if image, ok := c["image"].(string); ok && image != "" {
					if rewritten, changed := rewriteImageRegistryHost(image, alias); changed {
						c["image"] = rewritten
						modified = true
					}
				}
			}
		}
		for _, field := range containerStatusFields {
			list, ok := v[field].([]interface{})
			if !ok {
				continue
			}
			base := joinFieldPath(path, field) + "[*]"
			for _, raw := range list {
				c, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				for _, imgField := range []string{"image", "imageID"} {
					if excluded(CategoryImage, kind, base+"."+imgField) {
						continue
					}
					if image, ok := c[imgField].(string); ok && image != "" {
						if rewritten, changed := rewriteImageRegistryHost(image, alias); changed {
							c[imgField] = rewritten
							modified = true
						}
					}
				}
			}
		}

		// Recurse into everything else — this is what finds a container
		// list at whatever depth a given Kind happens to nest its
		// PodTemplateSpec (e.g. spec.template.spec for a Deployment,
		// spec.jobTemplate.spec.template.spec for a CronJob) without this
		// function needing to know any of those paths by name. Skipping
		// containerFieldNames here, rather than re-descending into them,
		// avoids redundant work: those six fields were already fully
		// handled above, whether or not they were actually present as a
		// []interface{} on this particular v.
		for k, val := range v {
			if containerFieldNames[k] {
				continue
			}
			if rewriteContainerImages(val, kind, joinFieldPath(path, k), excluded, alias) {
				modified = true
			}
		}
		return modified
	case []interface{}:
		modified := false
		childPath := path + "[*]"
		for _, val := range v {
			if rewriteContainerImages(val, kind, childPath, excluded, alias) {
				modified = true
			}
		}
		return modified
	default:
		return false
	}
}

// rewriteImageRegistryHost rewrites only the leading registry host of image,
// using Docker's own heuristic for whether one is even present: split on the
// first "/" — if there isn't one, image is a single component like
// "nginx:1.21" or "nginx@sha256:..." with no registry segment at all
// (implicit docker.io/library/...), so there's nothing to rewrite. If there
// is one, the segment before it counts as an explicit registry host only if
// it contains a "." or ":" (a domain or a host:port) or is literally
// "localhost" — otherwise it's just a Docker Hub organization/namespace
// segment (e.g. "myorg/myapp"), not a registry, and is left alone: rewriting
// an organization name isn't in scope here (the registry hostname is
// normally the sensitive part; the plan's Open Question 6 punts on full
// reference-grammar reconstruction for the same reason).
//
// The whole leading segment (including ":<port>", if present) is aliased as
// one atomic value, not split further — a registry's host and port
// together identify one logical endpoint.
//
// image may itself be prefixed with a CRI scheme, not just a bare
// reference: status.containerStatuses[*].imageID from CRI-O (rather than
// containerd/dockershim) commonly reads
// "docker-pullable://docker.io/library/nginx@sha256:...". Splitting on the
// first "/" without accounting for this would land inside the scheme
// separator's own "://" — "docker-pullable:" contains a ':' and would be
// misidentified as the registry host, corrupting the CRI scheme instead of
// rewriting the real one. Stripping a leading "<scheme>://" first, then
// applying the same heuristic to what remains, handles both a bare
// reference (no such prefix — most images, and every spec-side one) and a
// CRI-prefixed imageID uniformly.
func rewriteImageRegistryHost(image string, alias func(string) string) (string, bool) {
	prefix := ""
	rest := image
	if i := strings.Index(image, "://"); i >= 0 {
		prefix, rest = image[:i+len("://")], image[i+len("://"):]
	}

	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return image, false
	}
	first := rest[:slash]
	if !looksLikeRegistryHost(first) {
		return image, false
	}
	return prefix + alias(first) + rest[slash:], true
}

func looksLikeRegistryHost(segment string) bool {
	if segment == "localhost" {
		return true
	}
	return strings.ContainsAny(segment, ".:")
}
