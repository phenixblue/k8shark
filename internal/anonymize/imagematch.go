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
func rewriteImageRegistryInRecord(rec *capture.Record, alias func(string) string) (bool, error) {
	var obj interface{}
	if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
		return false, nil
	}

	modified := rewriteContainerImages(obj, alias)

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
// string fields inside any container-list-shaped map it finds.
func rewriteContainerImages(node interface{}, alias func(string) string) bool {
	switch v := node.(type) {
	case map[string]interface{}:
		modified := false
		// handled tracks which top-level keys of v were already fully
		// processed by the explicit container-list logic below, so the
		// generic recursive descent at the end doesn't redundantly re-walk
		// them looking for nothing new.
		handled := make(map[string]bool)

		for _, field := range containerListFields {
			list, ok := v[field].([]interface{})
			if !ok {
				continue
			}
			handled[field] = true
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
			handled[field] = true
			for _, raw := range list {
				c, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				for _, imgField := range []string{"image", "imageID"} {
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
		// function needing to know any of those paths by name.
		for k, val := range v {
			if handled[k] {
				continue
			}
			if rewriteContainerImages(val, alias) {
				modified = true
			}
		}
		return modified
	case []interface{}:
		modified := false
		for _, val := range v {
			if rewriteContainerImages(val, alias) {
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
func rewriteImageRegistryHost(image string, alias func(string) string) (string, bool) {
	slash := strings.IndexByte(image, '/')
	if slash < 0 {
		return image, false
	}
	first := image[:slash]
	if !looksLikeRegistryHost(first) {
		return image, false
	}
	return alias(first) + image[slash:], true
}

func looksLikeRegistryHost(segment string) bool {
	if segment == "localhost" {
		return true
	}
	return strings.ContainsAny(segment, ".:")
}
