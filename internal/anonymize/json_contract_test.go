package anonymize

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// The `-o json` output is a stable, scriptable interface (see
// docs/stability-policy.md): adding a top-level key is a minor change,
// renaming or removing one is a major change. This pins the key set and
// SchemaVersion against what Archive actually returns, mirroring
// internal/diagnose/json_contract_test.go's identical pattern for the
// same reason: a hand-built Result literal would keep passing even if
// Archive regressed to a zero-value field.
func TestArchive_JSONContract(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	result, err := Archive(src, dst, Options{Categories: []Category{CategoryNamespace}, Salt: []byte("contract-salt")})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	b, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{
		"schema_version", "namespaces_renamed", "nodes_renamed", "pods_renamed",
		"workloads_renamed", "ips_renamed", "hosts_renamed", "registries_renamed",
		// output_path/output_bytes are always zero-valued here (Archive()
		// itself never sets them — see Result's own doc comment) — that's
		// exactly the case an accidental omitempty would drop, so their
		// presence here is a real check, not a formality.
		"output_path", "output_bytes",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing frozen top-level key %q (must be present even when zero-valued); got %s", k, b)
		}
	}

	var fields struct {
		SchemaVersion     int `json:"schema_version"`
		NamespacesRenamed int `json:"namespaces_renamed"`
	}
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if fields.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", fields.SchemaVersion, SchemaVersion)
	}
	if fields.NamespacesRenamed != 1 {
		t.Errorf("namespaces_renamed = %d, want 1", fields.NamespacesRenamed)
	}
}
