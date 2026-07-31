package inspect

import (
	"encoding/json"
	"testing"
)

// The `-o json` output is a stable, scriptable interface as of v1.0 (see
// docs/stability-policy.md). This pins the top-level key set so an accidental
// tag rename fails here rather than silently breaking consumers.
func TestReport_JSONContract_TopLevelKeys(t *testing.T) {
	b, err := json.Marshal(Report{})
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
			t.Errorf("missing frozen top-level key %q", k)
		}
	}
	// format_version was renamed to archive_format_version before v1.0 to
	// disambiguate it from schema_version (the output's own version). It must
	// not come back — two keys meaning different versions is the confusion the
	// rename removed.
	if _, ok := got["format_version"]; ok {
		t.Error("format_version reappeared; it was renamed to archive_format_version")
	}
}
