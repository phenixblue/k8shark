package inspect

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

// The `-o json` output is a stable, scriptable interface as of v1.0 (see
// docs/stability-policy.md). This marshals what Run actually returns rather
// than a zero-value Report: a zero value would emit `"resources": null` and
// `"schema_version": 0` and still pass the key-presence checks, which would
// make the test useless as a guard.
func TestRun_JSONContract(t *testing.T) {
	now := time.Date(2026, 4, 9, 8, 0, 0, 0, time.UTC)
	path := buildArchive(t, []*capture.Record{{
		ID:           "pods-0",
		CapturedAt:   now,
		APIPath:      "/api/v1/pods",
		HTTPMethod:   "GET",
		ResponseCode: 200,
		ResponseBody: json.RawMessage(`{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"a"}}]}`),
	}})

	rep, err := Run(path, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{
		"schema_version", "archive_format_version", "capture_id", "captured_at",
		"captured_until", "kubernetes_version", "server_address", "record_count",
		"archive_path", "archive_size_bytes", "resources",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing frozen top-level key %q; got %s", k, b)
		}
	}
	// format_version was renamed to archive_format_version before v1.0 to
	// disambiguate it from schema_version (the output's own version). It must
	// not come back — two keys meaning different versions is the confusion the
	// rename removed.
	if _, ok := got["format_version"]; ok {
		t.Error("format_version reappeared; it was renamed to archive_format_version")
	}

	var probe struct {
		SchemaVersion        int                `json:"schema_version"`
		ArchiveFormatVersion int                `json:"archive_format_version"`
		Resources            *[]json.RawMessage `json:"resources"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if probe.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", probe.SchemaVersion, SchemaVersion)
	}
	if probe.ArchiveFormatVersion == 0 {
		t.Error("archive_format_version = 0; pre-versioning archives are normalized to 1")
	}
	if probe.Resources == nil {
		t.Errorf("resources marshaled as null, want an array; got %s", b)
	}
}

// TestRun_NoResources_MarshalsAsArray covers the same jq '.resources[]' footgun
// guarded elsewhere: an archive whose only records aren't resource lists yields
// zero resource summaries, and that must still be [] rather than null.
func TestRun_NoResources_MarshalsAsArray(t *testing.T) {
	now := time.Date(2026, 4, 9, 8, 0, 0, 0, time.UTC)
	// A discovery path — summarizeResources skips anything that doesn't parse
	// as group/version/resource, so this produces an empty summary list.
	path := buildArchive(t, []*capture.Record{{
		ID:           "disco-0",
		CapturedAt:   now,
		APIPath:      "/api",
		HTTPMethod:   "GET",
		ResponseCode: 200,
		ResponseBody: json.RawMessage(`{"kind":"APIVersions","versions":["v1"]}`),
	}})

	rep, err := Run(path, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Resources) != 0 {
		t.Fatalf("expected no resource summaries, got %d", len(rep.Resources))
	}
	if rep.Resources == nil {
		t.Error("Resources is nil; it must be an empty slice so JSON is [] not null")
	}

	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe struct {
		Resources *[]ResourceSummary `json:"resources"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if probe.Resources == nil {
		t.Errorf("resources marshaled as null, want []; got %s", b)
	}
}
