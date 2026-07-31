package query

import (
	"encoding/json"
	"testing"
)

// See docs/stability-policy.md: `-o json` is a frozen interface as of v1.0.
// query has two match shapes — JSONPath mode (Result) and --text/--regex mode
// (TextResult) — and both are frozen independently.
//
// These marshal what the real constructors return rather than a hand-built
// literal, so a regression to a nil slice in Run/SearchText fails here. A
// zero-value struct would marshal `"matches": null` and pass regardless,
// which defeats the point.

func TestRun_JSONContract_NoMatches(t *testing.T) {
	cs := buildQueryStore(t, map[string]string{
		"/api/v1/pods": `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"a"}}]}`,
	})

	// A path no captured object has, so the result is legitimately empty.
	res, err := Run(cs, Options{Expression: "{.spec.thisFieldDoesNotExist}"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Matches) != 0 {
		t.Fatalf("expected no matches, got %d", len(res.Matches))
	}
	assertMatchesContract(t, res, "Result")
}

func TestSearchText_JSONContract_NoMatches(t *testing.T) {
	cs := buildQueryStore(t, map[string]string{
		"/api/v1/pods": `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"a"}}]}`,
	})

	res, err := SearchText(cs, TextOptions{Pattern: "zzz-no-such-substring-anywhere"})
	if err != nil {
		t.Fatalf("SearchText: %v", err)
	}
	if len(res.Matches) != 0 {
		t.Fatalf("expected no matches, got %d", len(res.Matches))
	}
	assertMatchesContract(t, res, "TextResult")
}

// assertMatchesContract checks the frozen top-level keys, that schema_version
// is really populated (not a zero value that happens to marshal), and that an
// empty matches list is [] rather than null so `jq '.matches[]'` works.
func assertMatchesContract(t *testing.T, v any, label string) {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%s: unmarshal: %v", label, err)
	}
	for _, k := range []string{"schema_version", "matches"} {
		if _, ok := got[k]; !ok {
			t.Errorf("%s: missing frozen top-level key %q; got %s", label, k, b)
		}
	}

	var probe struct {
		SchemaVersion int                `json:"schema_version"`
		Matches       *[]json.RawMessage `json:"matches"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("%s: unmarshal probe: %v", label, err)
	}
	if probe.SchemaVersion != SchemaVersion {
		t.Errorf("%s: schema_version = %d, want %d", label, probe.SchemaVersion, SchemaVersion)
	}
	if probe.Matches == nil {
		t.Errorf("%s: matches marshaled as null, want []; got %s", label, b)
	}
}

// Both modes carry group as omitempty, so it's absent for core-group resources
// and present otherwise — identical behavior, not a per-mode difference. (An
// earlier freeze-review pass mistook the omitempty behavior for one mode
// lacking the field; this pins that they agree.)
func TestBothModes_GroupIsOmitEmpty(t *testing.T) {
	for name, v := range map[string]any{"Match": Match{Name: "x"}, "TextMatch": TextMatch{Name: "x"}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
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
