package inspect

import (
	"path/filepath"
	"testing"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
)

// TestRun_Encrypted verifies inspect.Run threads identities through to the
// archive open: an encrypted archive is refused without a key and summarized
// with one.
func TestRun_Encrypted(t *testing.T) {
	const pass = "inspect-encrypt-test"
	r, err := age.NewScryptRecipient(pass)
	if err != nil {
		t.Fatalf("NewScryptRecipient: %v", err)
	}
	r.SetWorkFactor(10)

	path := filepath.Join(t.TempDir(), "enc.kshrk")
	sw, err := archive.NewEncryptedStreamWriter(path, []age.Recipient{r})
	if err != nil {
		t.Fatalf("NewEncryptedStreamWriter: %v", err)
	}
	rec := map[string]any{"id": "r1", "api_path": "/api/v1/nodes", "response_code": 200,
		"response_body": map[string]any{"apiVersion": "v1", "kind": "NodeList", "items": []any{}}}
	if _, err := sw.WriteRecord(rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	meta := map[string]any{"format_version": 1, "capture_id": "enc", "record_count": 1}
	idx := map[string]any{"/api/v1/nodes": map[string]any{"api_path": "/api/v1/nodes", "seqs": []int{0}}}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// Without a key, Run must fail (not silently misread the ciphertext).
	if _, err := Run(path, nil); err == nil {
		t.Fatal("Run on encrypted archive without identities succeeded, want error")
	}

	ids, err := archive.IdentitiesFromPassphrase(pass)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	report, err := Run(path, ids)
	if err != nil {
		t.Fatalf("Run with identities: %v", err)
	}
	if report.CaptureID != "enc" {
		t.Errorf("CaptureID = %q, want enc", report.CaptureID)
	}
}
