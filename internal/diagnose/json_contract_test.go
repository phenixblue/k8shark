package diagnose

import (
	"encoding/json"
	"testing"
)

// The `-o json` output is a stable, scriptable interface as of v1.0 (see
// docs/stability-policy.md): adding a top-level key is a minor change,
// renaming or removing one is a major change. These tests pin the key set so
// an accidental tag rename fails here rather than silently breaking consumers.

func TestReport_JSONContract_TopLevelKeys(t *testing.T) {
	// Populated, not zero-value: a zero Report marshals "findings": null and
	// "schema_version": 0 and would still satisfy key-presence checks.
	b, err := json.Marshal(Report{
		SchemaVersion: SchemaVersion,
		CaptureID:     "550e8400-e29b-41d4-a716-446655440000",
		Findings:      make([]Finding, 0),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// `at` stays omitempty — it's only set when --at was passed — so it is
	// deliberately not in the always-present set.
	for _, k := range []string{"schema_version", "capture_id", "summary", "findings"} {
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

// TestRun_EmptyFindings_MarshalsAsArray guards the null-vs-[] footgun: a
// capture with nothing wrong must still emit `"findings": []`, because the
// most natural consumer pattern (`jq '.findings[]'`) errors on null.
func TestRun_EmptyFindings_MarshalsAsArray(t *testing.T) {
	// A store with one healthy pod list — no rule should fire.
	cs := buildDiagStore(t, map[string]string{
		"/api/v1/pods": `{"kind":"PodList","apiVersion":"v1","items":[]}`,
	})
	defer cs.Close()

	rep := Run(cs, Options{})
	if len(rep.Findings) != 0 {
		t.Fatalf("expected no findings, got %d", len(rep.Findings))
	}
	if rep.Findings == nil {
		t.Error("Findings is nil; it must be an empty slice so JSON is [] not null")
	}

	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe struct {
		Findings *[]Finding `json:"findings"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if probe.Findings == nil {
		t.Errorf("findings marshaled as null, want []; output was %s", b)
	}
}
