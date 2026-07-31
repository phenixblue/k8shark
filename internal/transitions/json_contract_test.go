package transitions

import (
	"encoding/json"
	"testing"
)

// TestLoadReport_IsObjectNotArray guards the shape change made before v1.0:
// `transitions -o json` used to emit a bare top-level array, which had nowhere
// to grow — no schema_version, no summary, no paging — without a breaking
// change. The envelope is the extension point, so it must stay an object.
//
// This marshals what LoadReport actually returns, not a hand-built Report: a
// literal with Transitions pre-initialized would keep passing even if
// LoadReport regressed to a nil slice, reintroducing the `jq '.transitions[]'`
// null footgun the contract exists to prevent.
func TestLoadReport_IsObjectNotArray(t *testing.T) {
	// Two identical snapshots — no state change, so the event list is
	// legitimately empty and the nil-vs-[] distinction is what's under test.
	body := `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"a","uid":"u1"}}]}`
	path := buildPollArchive(t, "/api/v1/pods", []string{body, body})

	rep, err := LoadReport(path, FilterOpts{}, nil)
	if err != nil {
		t.Fatalf("LoadReport: %v", err)
	}
	if len(rep.Transitions) != 0 {
		t.Fatalf("expected no transitions between identical snapshots, got %d", len(rep.Transitions))
	}
	if rep.Transitions == nil {
		t.Error("Transitions is nil; it must be an empty slice so JSON is [] not null")
	}

	b, err := json.Marshal(rep)
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
	for _, k := range []string{"schema_version", "capture_id", "transitions"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing frozen top-level key %q; got %s", k, b)
		}
	}

	var fields struct {
		SchemaVersion int           `json:"schema_version"`
		CaptureID     string        `json:"capture_id"`
		Transitions   *[]Transition `json:"transitions"`
	}
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if fields.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", fields.SchemaVersion, SchemaVersion)
	}
	if fields.CaptureID == "" {
		t.Error("capture_id is empty; LoadReport must populate it from archive metadata")
	}
	// Empty must be [] so `jq '.transitions[]'` works.
	if fields.Transitions == nil {
		t.Errorf("transitions marshaled as null, want []; got %s", b)
	}
}

// TestLoadTransitions_EmptyIsNonNil pins the same guarantee on the
// envelope-free entry point, since it returns rep.Transitions directly.
func TestLoadTransitions_EmptyIsNonNil(t *testing.T) {
	body := `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"a","uid":"u1"}}]}`
	path := buildPollArchive(t, "/api/v1/pods", []string{body, body})

	ts, err := LoadTransitions(path, FilterOpts{}, nil)
	if err != nil {
		t.Fatalf("LoadTransitions: %v", err)
	}
	if len(ts) != 0 {
		t.Fatalf("expected no transitions, got %d", len(ts))
	}
	if ts == nil {
		t.Error("LoadTransitions returned nil; callers marshaling it directly would emit null")
	}
}
