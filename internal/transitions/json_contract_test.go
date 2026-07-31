package transitions

import (
	"encoding/json"
	"testing"
)

// TestReport_IsObjectNotArray guards the shape change made before v1.0:
// `transitions -o json` used to emit a bare top-level array, which had nowhere
// to grow — no schema_version, no summary, no paging — without a breaking
// change. The envelope is the extension point, so it must stay an object.
func TestReport_IsObjectNotArray(t *testing.T) {
	b, err := json.Marshal(&Report{SchemaVersion: SchemaVersion, Transitions: make([]Transition, 0)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe any
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := probe.(map[string]any); !ok {
		t.Fatalf("top level is %T, want a JSON object; got %s", probe, b)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"schema_version", "transitions"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing frozen top-level key %q", k)
		}
	}
	// Empty must be [] so `jq '.transitions[]'` works.
	var arr struct {
		Transitions *[]Transition `json:"transitions"`
	}
	if err := json.Unmarshal(b, &arr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if arr.Transitions == nil {
		t.Errorf("transitions marshaled as null, want []; got %s", b)
	}
}
