package server

import "testing"

// supportedPatchContentTypes are the Content-Types applyPatch is ever
// actually called with in production — every call site gates on
// supportedPatchType first (see writes.go's overlayPatch/overlayScaleWrite),
// so a request with any other Content-Type never reaches applyPatch at all.
var supportedPatchContentTypes = []string{
	"application/json-patch+json",
	"application/strategic-merge-patch+json",
	"application/apply-patch+yaml",
	"application/merge-patch+json",
}

// FuzzApplyPatch fuzzes the write path's patch application — JSON Patch
// (RFC 6902), strategic-merge, apply-patch+yaml, and plain JSON merge — all
// four of which decode an untrusted client-supplied body. applyPatch must
// only ever return an error on malformed input, never panic; a panic here
// would take down the mock server on a single bad request.
//
// contentType is constrained to supportedPatchContentTypes (optionally with
// a "; charset=..." parameter, which patchMediaType strips) rather than
// fuzzed as a free-form string, so the fuzzer explores current/patch/GVR
// mutations within applyPatch's real contract instead of spending its budget
// on Content-Type values production never reaches.
func FuzzApplyPatch(f *testing.F) {
	seeds := []struct {
		current, patch           string
		ctIndex                  uint8
		ctParam                  bool
		group, version, resource string
	}{
		{`{"kind":"Pod","spec":{"containers":[{"name":"a","image":"x"}]}}`,
			`{"spec":{"containers":[{"name":"a","image":"y"}]}}`, 1, false, "", "v1", "pods"},
		{`{"a":1}`, `[{"op":"replace","path":"/a","value":2}]`, 0, false, "", "v1", "configmaps"},
		{`{"a":1}`, "a: 2\n", 2, true, "", "v1", "configmaps"},
		{`{"a":1}`, `{"a":2}`, 3, false, "", "v1", "configmaps"},
		{`{"a":1}`, `{"a":2}`, 0, true, "apps", "v1", "deployments"},
		{``, ``, 0, false, "", "v1", "pods"},
		{`not json`, `not json either`, 1, false, "", "v1", "pods"},
	}
	for _, s := range seeds {
		f.Add([]byte(s.current), []byte(s.patch), s.ctIndex, s.ctParam, s.group, s.version, s.resource)
	}

	f.Fuzz(func(t *testing.T, current, patch []byte, ctIndex uint8, ctParam bool, group, version, resource string) {
		contentType := supportedPatchContentTypes[int(ctIndex)%len(supportedPatchContentTypes)]
		if ctParam {
			contentType += "; charset=utf-8"
		}
		_, _ = applyPatch(current, patch, contentType, group, version, resource)
	})
}
