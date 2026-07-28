package k8spath

import "testing"

// TestParse covers every path shape the capture engine emits as an index key
// (see internal/capture/engine.go: buildAPIPath, and the pod-log index key
// format in fetchOnePodLog), plus the query-string suffixes it appends.
func TestParse(t *testing.T) {
	cases := []struct {
		name                string
		path                string
		group, version      string
		resource, namespace string
	}{
		{"core cluster-scoped", "/api/v1/nodes", "", "v1", "nodes", ""},
		{"core namespaced", "/api/v1/namespaces/default/pods", "", "v1", "pods", "default"},
		{"core namespaced, other namespace", "/api/v1/namespaces/kube-system/configmaps", "", "v1", "configmaps", "kube-system"},
		{"group cluster-scoped", "/apis/apps/v1/deployments", "apps", "v1", "deployments", ""},
		{"group namespaced", "/apis/apps/v1/namespaces/default/deployments", "apps", "v1", "deployments", "default"},
		{"group namespaced, other group/namespace", "/apis/batch/v1/namespaces/ci/jobs", "batch", "v1", "jobs", "ci"},

		// Table / TableSchema views (?as=...) — same resource, query stripped.
		{"core namespaced, as=Table", "/api/v1/namespaces/default/pods?as=Table", "", "v1", "pods", "default"},
		{"group cluster-scoped, as=TableSchema", "/apis/apps/v1/deployments?as=TableSchema", "apps", "v1", "deployments", ""},

		// Pod-log subresource keys (fetchOnePodLog's indexKey format): a
		// subresource segment past the resource, plus a query suffix. This is
		// the #235 regression — a rigid segment-count parser mis-parses these
		// to resource="", namespace="".
		{"core pod log, container", "/api/v1/namespaces/default/pods/nginx/log?container=app", "", "v1", "pods", "default"},
		{"core pod log, container+previous", "/api/v1/namespaces/default/pods/nginx/log?container=app&previous=true", "", "v1", "pods", "default"},
		{"core pod log, no query", "/api/v1/namespaces/default/pods/nginx/log", "", "v1", "pods", "default"},

		// Single-object GET (no namespaces/ segment ambiguity for cluster-scoped).
		{"core cluster-scoped single object", "/api/v1/nodes/node-1", "", "v1", "nodes", ""},
		{"group namespaced single object", "/apis/apps/v1/namespaces/default/deployments/my-app", "apps", "v1", "deployments", "default"},

		// Degenerate inputs.
		{"empty", "", "", "", "", ""},
		{"root", "/", "", "", "", ""},
		{"unrecognized prefix", "/healthz", "", "", "", ""},
		{"api with no version", "/api", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, v, r, ns := Parse(tc.path)
			if g != tc.group || v != tc.version || r != tc.resource || ns != tc.namespace {
				t.Errorf("Parse(%q) = (%q,%q,%q,%q), want (%q,%q,%q,%q)",
					tc.path, g, v, r, ns, tc.group, tc.version, tc.resource, tc.namespace)
			}
		})
	}
}
