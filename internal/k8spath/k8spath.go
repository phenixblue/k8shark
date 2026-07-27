// Package k8spath extracts (group, version, resource, namespace) from a
// captured API path such as "/api/v1/namespaces/default/pods" or
// "/apis/apps/v1/deployments". Every package that reads a capture.Index key
// (a Kubernetes REST path, possibly with a query suffix) as structured
// group/version/resource/namespace needs this; it was previously copy-pasted
// into seven packages with three subtly different behaviors (#235).
package k8spath

import "strings"

// Parse extracts group, version, resource, and namespace from a captured API
// path. Any query string — ?as=Table, ?as=TableSchema, ?container=,
// ?previous=, or any other suffix the capture engine emits — is stripped
// first, and any path segments past the resource (e.g. a pod's /log
// subresource) are ignored. This matters for index keys like
// ".../namespaces/ns/pods/name/log?container=app": a rigid segment-count
// check mis-parses that to resource="", namespace="" (the #235 bug in
// internal/diff's diff --resource pods, which silently dropped every pod-log
// change as a result). Cluster-scoped paths return an empty namespace.
func Parse(path string) (group, version, resource, namespace string) {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case len(parts) >= 3 && parts[0] == "api": // /api/v1/...
		version = parts[1]
		rest := parts[2:]
		if len(rest) >= 3 && rest[0] == "namespaces" {
			namespace = rest[1]
			resource = rest[2]
		} else {
			resource = rest[0]
		}
	case len(parts) >= 4 && parts[0] == "apis": // /apis/<group>/<version>/...
		group, version = parts[1], parts[2]
		rest := parts[3:]
		if len(rest) >= 3 && rest[0] == "namespaces" {
			namespace = rest[1]
			resource = rest[2]
		} else {
			resource = rest[0]
		}
	}
	return
}
