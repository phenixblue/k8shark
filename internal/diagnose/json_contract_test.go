package diagnose

import (
	"encoding/json"
	"testing"
)

// The `-o json` output is a stable, scriptable interface as of v1.0 (see
// docs/stability-policy.md): adding a top-level key is a minor change,
// renaming or removing one is a major change.
//
// This pins the key set against what Run actually returns rather than a
// hand-built Report — a literal with Findings pre-initialized would keep
// passing even if Run regressed to a nil slice, which is the `jq '.findings[]'`
// footgun the contract exists to prevent. A clean capture is used so the empty
// case (the one that regressed) is the one under test.
func TestRun_JSONContract_CleanCapture(t *testing.T) {
	// One healthy, empty pod list — no rule should fire.
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

	var fields struct {
		SchemaVersion int        `json:"schema_version"`
		CaptureID     string     `json:"capture_id"`
		Findings      *[]Finding `json:"findings"`
	}
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if fields.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", fields.SchemaVersion, SchemaVersion)
	}
	if fields.CaptureID == "" {
		t.Error("capture_id is empty; Run must populate it from store metadata")
	}
	if fields.Findings == nil {
		t.Errorf("findings marshaled as null, want []; output was %s", b)
	}
}

// TestRun_At_IsOmittedWhenUnset pins the one deliberately conditional field:
// `at` appears only when Options.At was set, unlike capture_id which is always
// emitted. Documented in docs/stability-policy.md.
func TestRun_At_IsOmittedWhenUnset(t *testing.T) {
	cs := buildDiagStore(t, map[string]string{
		"/api/v1/pods": `{"kind":"PodList","apiVersion":"v1","items":[]}`,
	})
	defer cs.Close()

	b, err := json.Marshal(Run(cs, Options{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["at"]; ok {
		t.Errorf("at present without --at; it should be omitempty. got %s", b)
	}
}
