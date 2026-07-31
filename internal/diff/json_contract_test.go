package diff

import (
	"encoding/json"
	"testing"
)

// See docs/stability-policy.md: `-o json` is a frozen interface as of v1.0.
func TestResult_JSONContract(t *testing.T) {
	// Populated, not zero-value: a zero Result marshals "changes": null and
	// "schema_version": 0 and would still satisfy key-presence checks.
	b, err := json.Marshal(&Result{SchemaVersion: SchemaVersion, Changes: make([]Change, 0)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"schema_version", "changes"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing frozen top-level key %q; got %s", k, b)
		}
	}
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if probe.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", probe.SchemaVersion, SchemaVersion)
	}
}

// Run must emit `"changes": []`, never null, so `jq '.changes[]'` works on a
// no-differences result.
func TestResult_EmptyChangesMarshalsAsArray(t *testing.T) {
	b, err := json.Marshal(&Result{Changes: make([]Change, 0)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe struct {
		Changes *[]Change `json:"changes"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if probe.Changes == nil {
		t.Errorf("changes marshaled as null, want []; got %s", b)
	}
}
