package anonymize

import (
	"bytes"
	"encoding/json"
	"strings"
)

// A pod's log is captured as plain text stored as a JSON-encoded *string*
// (see internal/capture/podlogs.go), so its record body is `"2026-… INFO
// starting on node worker-1 …\n"` rather than a JSON object. Every matcher
// here begins by decoding a body into an object and quietly returns "nothing
// to do" when that fails — which is right for a corrupt record, and wrong for
// a log: it left pod logs, usually the most name-, IP- and hostname-dense
// text in a capture, entirely un-anonymized, and the leak check (which runs
// the same code) reported them clean.
//
// A string body is just one more leaf. The helpers below let the URL matcher
// and the sweep treat it as one.

// stringBody returns the text of a record body that is a single JSON string.
func stringBody(body json.RawMessage) (string, bool) {
	b := bytes.TrimSpace(body)
	if len(b) == 0 || b[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", false
	}
	return s, true
}

// textBodyField is the field path reported to exclude rules for a string
// body, so `fieldPath: log` (with `kind: Pod`) keeps a pod's logs untouched.
const textBodyField = "log"

// textBodyKind is the Kind reported to exclude rules for a string body: a
// pod's /log subresource belongs to a Pod; anything else has no Kind.
func textBodyKind(apiPath string) string {
	if strings.Contains(apiPath, "/pods/") {
		return "Pod"
	}
	return ""
}
