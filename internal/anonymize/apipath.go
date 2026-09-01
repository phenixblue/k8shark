package anonymize

import "strings"

// rewriteNamespaceInPath replaces the namespace segment of a captured API
// path with alias(original), leaving every other segment untouched. It
// deliberately does not reuse internal/store.ParseAPIPath, which is
// documented as narrowly scoped to exact 5/6-segment LIST paths — its own
// doc comment references a prior regression from loosening it, and
// capture.Record.APIPath also carries item-level GET paths (and, in
// principle, subresource paths) that function was never meant to parse.
//
// The approach here is simpler and more tolerant on purpose: find the
// literal "namespaces" segment and replace whatever comes immediately after
// it. That is always the namespace name, regardless of what (if anything)
// follows — resource type, object name, subresource — because in every
// namespaced Kubernetes API path shape, "namespaces/<ns>" appears as an
// unbroken pair:
//
//	/api/v1/namespaces/<ns>                      (GET the Namespace itself)
//	/api/v1/namespaces/<ns>/pods                 (namespaced list)
//	/api/v1/namespaces/<ns>/pods/<name>          (namespaced object GET)
//	/apis/apps/v1/namespaces/<ns>/deployments    (group/version form)
//	/api/v1/namespaces/<ns>/pods/<name>/log      (a subresource, untouched)
//
// Returns the path unchanged (ok=false) for a path with no "namespaces"
// segment (cluster-scoped, or the all-namespaces list form) and for the
// Namespace collection endpoint itself (/api/v1/namespaces, where
// "namespaces" is the last segment and there is nothing after it to alias).
func rewriteNamespaceInPath(apiPath string, alias func(string) string) (string, bool) {
	parts := strings.Split(apiPath, "/")
	for i, seg := range parts {
		if seg == "namespaces" && i+1 < len(parts) && parts[i+1] != "" {
			parts[i+1] = alias(parts[i+1])
			return strings.Join(parts, "/"), true
		}
	}
	return apiPath, false
}
