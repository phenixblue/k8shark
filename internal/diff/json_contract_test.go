package diff

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

// See docs/stability-policy.md: `-o json` is a frozen interface as of v1.0.
//
// This marshals what Run actually returns rather than a hand-built Result. A
// literal with Changes pre-initialized would keep passing even if Run
// regressed to a nil slice or stopped populating SchemaVersion, which is
// exactly what the contract needs guarded.
func TestRun_JSONContract_NoDifferences(t *testing.T) {
	at := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	body := `{"kind":"PodList","items":[{"metadata":{"name":"nginx"}}]}`

	// Identical content on both sides, so there are legitimately no changes
	// and the nil-vs-[] distinction is what's under test.
	before := buildArchive(t, map[string][]capture.Record{
		"/api/v1/namespaces/default/pods": {
			newRecord("rec-1", "/api/v1/namespaces/default/pods", at, body),
		},
	})
	after := buildArchive(t, map[string][]capture.Record{
		"/api/v1/namespaces/default/pods": {
			newRecord("rec-2", "/api/v1/namespaces/default/pods", at.Add(5*time.Minute), body),
		},
	})

	result, err := Run(Options{BeforeArchive: before, AfterArchive: after})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Changes) != 0 {
		t.Fatalf("expected no changes between identical bodies, got %d", len(result.Changes))
	}
	if result.Changes == nil {
		t.Error("Changes is nil; it must be an empty slice so JSON is [] not null")
	}

	b, err := json.Marshal(result)
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

	var fields struct {
		SchemaVersion int       `json:"schema_version"`
		Changes       *[]Change `json:"changes"`
	}
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if fields.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", fields.SchemaVersion, SchemaVersion)
	}
	// Empty must be [] so `jq '.changes[]'` works.
	if fields.Changes == nil {
		t.Errorf("changes marshaled as null, want []; got %s", b)
	}
}
