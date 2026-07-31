package query

import (
	"encoding/json"
	"testing"
)

// See docs/stability-policy.md: `-o json` is a frozen interface as of v1.0.
// query has two match shapes — JSONPath mode (Result) and --text/--regex mode
// (TextResult) — and both are frozen independently.
func TestResult_JSONContract(t *testing.T) {
	b, err := json.Marshal(&Result{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"schema_version", "matches"} {
		if _, ok := got[k]; !ok {
			t.Errorf("Result: missing frozen top-level key %q", k)
		}
	}
}

func TestTextResult_JSONContract(t *testing.T) {
	b, err := json.Marshal(&TextResult{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"schema_version", "matches"} {
		if _, ok := got[k]; !ok {
			t.Errorf("TextResult: missing frozen top-level key %q", k)
		}
	}
}

// Both modes carry group as omitempty, so it's absent for core-group resources
// and present otherwise — identical behavior, not a per-mode difference.
func TestBothModes_GroupIsOmitEmpty(t *testing.T) {
	jp, err := json.Marshal(Match{Name: "x"})
	if err != nil {
		t.Fatalf("marshal Match: %v", err)
	}
	tx, err := json.Marshal(TextMatch{Name: "x"})
	if err != nil {
		t.Fatalf("marshal TextMatch: %v", err)
	}
	for name, b := range map[string][]byte{"Match": jp, "TextMatch": tx} {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		if _, ok := m["group"]; ok {
			t.Errorf("%s: group present on an empty group; expected omitempty to drop it", name)
		}
	}
	for name, v := range map[string]any{"Match": Match{Group: "apps"}, "TextMatch": TextMatch{Group: "apps"}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		if _, ok := m["group"]; !ok {
			t.Errorf("%s: group missing when non-empty", name)
		}
	}
}
