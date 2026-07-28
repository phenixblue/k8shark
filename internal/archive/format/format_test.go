package format

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestIndex_MarshalJSON_WrapsAsEntries pins the version-2+ on-disk shape:
// {"entries": {...}}, not a bare top-level map, so the index can gain
// sibling fields later without another format-version bump (#219).
func TestIndex_MarshalJSON_WrapsAsEntries(t *testing.T) {
	idx := Index{
		"/api/v1/pods": {APIPath: "/api/v1/pods", Seqs: []int{0, 1}},
	}
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal into raw map: %v", err)
	}
	if _, ok := raw["entries"]; !ok {
		t.Fatalf("marshaled Index has no top-level \"entries\" key: %s", data)
	}
	if len(raw) != 1 {
		t.Errorf("marshaled Index has %d top-level keys, want exactly 1 (\"entries\"): %s", len(raw), data)
	}
}

// TestIndex_MarshalJSON_NilIndexWritesEmptyObject confirms a nil Index
// marshals "entries" as {}, not null — the documented v2+ schema says
// "entries" is an object, and null would also round-trip back to a nil map
// instead of the empty-but-non-nil map ReadIndex's callers expect.
func TestIndex_MarshalJSON_NilIndexWritesEmptyObject(t *testing.T) {
	var idx Index
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `{"entries":{}}`; string(data) != want {
		t.Errorf("Marshal(nil Index) = %s, want %s", data, want)
	}
}

// TestIndex_UnmarshalJSON_RoundTrip confirms Marshal -> Unmarshal reproduces
// the original map, including exact Seqs/Counts contents and order — not
// just lengths, which wouldn't catch a reordering or truncation bug.
func TestIndex_UnmarshalJSON_RoundTrip(t *testing.T) {
	want := Index{
		"/api/v1/pods":              {APIPath: "/api/v1/pods", Seqs: []int{5, 2, 9}, Counts: []int{7, 0, 3}},
		"/api/v1/namespaces/x/pods": {APIPath: "/api/v1/namespaces/x/pods", Seqs: []int{4}},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Index
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for path, wantEntry := range want {
		gotEntry, ok := got[path]
		if !ok {
			t.Errorf("entry %q missing after round trip", path)
			continue
		}
		if gotEntry.APIPath != wantEntry.APIPath {
			t.Errorf("entry %q APIPath = %q, want %q", path, gotEntry.APIPath, wantEntry.APIPath)
		}
		if !reflect.DeepEqual(gotEntry.Seqs, wantEntry.Seqs) {
			t.Errorf("entry %q Seqs = %v, want %v", path, gotEntry.Seqs, wantEntry.Seqs)
		}
		if !reflect.DeepEqual(gotEntry.Counts, wantEntry.Counts) {
			t.Errorf("entry %q Counts = %v, want %v", path, gotEntry.Counts, wantEntry.Counts)
		}
	}
}

// TestIndex_UnmarshalJSON_AcceptsVersion1BareMap is the actual compatibility
// guarantee #219 exists to protect: a version-1 archive's index.json.zst was
// written as a bare top-level map (no "entries" wrapper) — this build must
// still read it correctly for the life of the 1.x line.
func TestIndex_UnmarshalJSON_AcceptsVersion1BareMap(t *testing.T) {
	bareV1 := `{
		"/api/v1/namespaces/default/pods": {
			"api_path": "/api/v1/namespaces/default/pods",
			"seqs": [0, 1, 2],
			"times": ["2026-04-09T10:00:00Z", "2026-04-09T10:00:30Z", "2026-04-09T10:01:00Z"],
			"counts": [4, 4, 5]
		}
	}`
	var idx Index
	if err := json.Unmarshal([]byte(bareV1), &idx); err != nil {
		t.Fatalf("Unmarshal(bare v1 shape): %v", err)
	}
	entry, ok := idx["/api/v1/namespaces/default/pods"]
	if !ok {
		t.Fatal("expected pods entry, not found")
	}
	if entry.APIPath != "/api/v1/namespaces/default/pods" {
		t.Errorf("APIPath = %q", entry.APIPath)
	}
	if len(entry.Seqs) != 3 || len(entry.Counts) != 3 {
		t.Errorf("Seqs/Counts = %v/%v, want length 3 each", entry.Seqs, entry.Counts)
	}
}

// TestIndex_UnmarshalJSON_EmptyBareMapAndEmptyWrapped confirms a zero-entry
// index round-trips as an empty (non-nil) map regardless of which shape
// produced it — a v1 archive with nothing captured wrote a bare "{}", while
// this build always writes the wrapped "{"entries":{}}" shape.
func TestIndex_UnmarshalJSON_EmptyBareMapAndEmptyWrapped(t *testing.T) {
	for _, tc := range []struct{ name, json string }{
		{"empty bare v1 map", `{}`},
		{"empty wrapped v2 shape", `{"entries":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var idx Index
			if err := json.Unmarshal([]byte(tc.json), &idx); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if idx == nil {
				t.Error("Index = nil, want a non-nil empty map")
			}
			if len(idx) != 0 {
				t.Errorf("Index has %d entries, want 0", len(idx))
			}
		})
	}
}

// TestWatchIndex_MarshalJSON_WrapsAsEntries and the round-trip/v1-compat
// cases mirror Index's — see those doc comments for why.
func TestWatchIndex_MarshalJSON_WrapsAsEntries(t *testing.T) {
	wi := WatchIndex{
		"/api/v1/pods": {APIPath: "/api/v1/pods", Seqs: []int{0}, EventTypes: []string{"ADDED"}},
	}
	data, err := json.Marshal(wi)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal into raw map: %v", err)
	}
	if _, ok := raw["entries"]; !ok {
		t.Fatalf("marshaled WatchIndex has no top-level \"entries\" key: %s", data)
	}
}

// TestWatchIndex_MarshalJSON_NilWatchIndexWritesEmptyObject mirrors
// TestIndex_MarshalJSON_NilIndexWritesEmptyObject — see its doc comment.
func TestWatchIndex_MarshalJSON_NilWatchIndexWritesEmptyObject(t *testing.T) {
	var wi WatchIndex
	data, err := json.Marshal(wi)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := `{"entries":{}}`; string(data) != want {
		t.Errorf("Marshal(nil WatchIndex) = %s, want %s", data, want)
	}
}

func TestWatchIndex_UnmarshalJSON_AcceptsVersion1BareMap(t *testing.T) {
	bareV1 := `{
		"/api/v1/namespaces/default/pods": {
			"api_path": "/api/v1/namespaces/default/pods",
			"seqs": [0],
			"times": ["2026-04-09T10:00:00Z"],
			"event_types": ["ADDED"]
		}
	}`
	var wi WatchIndex
	if err := json.Unmarshal([]byte(bareV1), &wi); err != nil {
		t.Fatalf("Unmarshal(bare v1 shape): %v", err)
	}
	entry, ok := wi["/api/v1/namespaces/default/pods"]
	if !ok {
		t.Fatal("expected pods entry, not found")
	}
	if len(entry.EventTypes) != 1 || entry.EventTypes[0] != "ADDED" {
		t.Errorf("EventTypes = %v", entry.EventTypes)
	}
}

// TestWatchIndex_UnmarshalJSON_NullEntry mirrors TestIndex_UnmarshalJSON_NullEntry.
func TestWatchIndex_UnmarshalJSON_NullEntry(t *testing.T) {
	var wi WatchIndex
	err := json.Unmarshal([]byte(`{"entries": {"/api/v1/pods": null}}`), &wi)
	if err == nil {
		t.Fatal("Unmarshal succeeded on a null entry value, want error")
	}
}

// TestWatchIndex_UnmarshalJSON_NullEntries mirrors TestIndex_UnmarshalJSON_NullEntries.
func TestWatchIndex_UnmarshalJSON_NullEntries(t *testing.T) {
	var wi WatchIndex
	if err := json.Unmarshal([]byte(`{"entries": null}`), &wi); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if wi == nil {
		t.Error("WatchIndex = nil, want a non-nil empty map")
	}
	if len(wi) != 0 {
		t.Errorf("WatchIndex has %d entries, want 0", len(wi))
	}
}

// TestIndex_UnmarshalJSON_MalformedEntry confirms a malformed entry produces
// a clear error rather than a zero-value entry or a panic.
func TestIndex_UnmarshalJSON_MalformedEntry(t *testing.T) {
	for _, tc := range []struct{ name, json string }{
		{"malformed wrapped entries", `{"entries": {"/api/v1/pods": "not an object"}}`},
		{"malformed bare v1 entry", `{"/api/v1/pods": "not an object"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var idx Index
			if err := json.Unmarshal([]byte(tc.json), &idx); err == nil {
				t.Fatal("Unmarshal succeeded on a malformed entry, want error")
			}
		})
	}
}

// TestIndex_UnmarshalJSON_NullEntry confirms a wrapped-shape entry whose
// value is JSON null is rejected outright rather than silently stored as a
// nil *IndexEntry — a caller dereferencing idx[path].Seqs on a nil entry
// would panic. This is specific to the wrapped shape: unmarshaling JSON null
// into a struct value (the bare v1 path's entry type) is a documented no-op,
// so a null entry there just yields a zero-value IndexEntry, never a nil one.
func TestIndex_UnmarshalJSON_NullEntry(t *testing.T) {
	var idx Index
	err := json.Unmarshal([]byte(`{"entries": {"/api/v1/pods": null}}`), &idx)
	if err == nil {
		t.Fatal("Unmarshal succeeded on a null entry value, want error")
	}
}

// TestIndex_UnmarshalJSON_NullEntries confirms a wrapped shape with "entries"
// itself set to null normalizes to an empty (non-nil) map — the same
// zero-entry-index case a genuinely empty {"entries":{}} produces — rather
// than a nil map that would panic on a later write.
func TestIndex_UnmarshalJSON_NullEntries(t *testing.T) {
	var idx Index
	if err := json.Unmarshal([]byte(`{"entries": null}`), &idx); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if idx == nil {
		t.Error("Index = nil, want a non-nil empty map")
	}
	if len(idx) != 0 {
		t.Errorf("Index has %d entries, want 0", len(idx))
	}
}
