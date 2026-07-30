package server

import "testing"

// FuzzApplyPatch fuzzes the write path's patch application — JSON Patch
// (RFC 6902), strategic-merge, apply-patch+yaml, and plain JSON merge — all
// four of which decode an untrusted client-supplied body. applyPatch must
// only ever return an error on malformed input, never panic; a panic here
// would take down the mock server on a single bad request.
func FuzzApplyPatch(f *testing.F) {
	seeds := []struct {
		current, patch                        string
		contentType, group, version, resource string
	}{
		{`{"kind":"Pod","spec":{"containers":[{"name":"a","image":"x"}]}}`,
			`{"spec":{"containers":[{"name":"a","image":"y"}]}}`,
			"application/strategic-merge-patch+json", "", "v1", "pods"},
		{`{"a":1}`, `[{"op":"replace","path":"/a","value":2}]`,
			"application/json-patch+json", "", "v1", "configmaps"},
		{`{"a":1}`, "a: 2\n",
			"application/apply-patch+yaml", "", "v1", "configmaps"},
		{`{"a":1}`, `{"a":2}`,
			"application/merge-patch+json", "", "v1", "configmaps"},
		{`{"a":1}`, `{"a":2}`, "", "apps", "v1", "deployments"},
		{``, ``, "application/json-patch+json", "", "v1", "pods"},
		{`not json`, `not json either`, "application/strategic-merge-patch+json", "", "v1", "pods"},
	}
	for _, s := range seeds {
		f.Add([]byte(s.current), []byte(s.patch), s.contentType, s.group, s.version, s.resource)
	}

	f.Fuzz(func(t *testing.T, current, patch []byte, contentType, group, version, resource string) {
		_, _ = applyPatch(current, patch, contentType, group, version, resource)
	})
}
