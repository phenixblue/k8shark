package redact

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// redactEncryptTestPassphrase is a fixed test-only passphrase; not a real secret.
const redactEncryptTestPassphrase = "redact-encrypt-test-passphrase" //nolint:gosec // test fixture

// buildEncryptedArchive mirrors buildTestArchive but writes the records as an
// age-encrypted archive using recipients.
func buildEncryptedArchive(t *testing.T, records []*capture.Record, recipients []age.Recipient) string {
	t.Helper()
	dir := t.TempDir()

	idx := capture.Index{}
	pathSeq := map[string]int{}
	for _, r := range records {
		seq := pathSeq[r.APIPath]
		pathSeq[r.APIPath] = seq + 1
		e := idx[r.APIPath]
		if e == nil {
			e = &capture.IndexEntry{APIPath: r.APIPath}
			idx[r.APIPath] = e
		}
		e.Seqs = append(e.Seqs, seq)
		e.Times = append(e.Times, r.CapturedAt)
	}

	meta := &capture.CaptureMetadata{
		CaptureID:     "test-id",
		CapturedAt:    time.Now().Add(-time.Minute),
		CapturedUntil: time.Now(),
		RecordCount:   len(records),
		Encrypted:     true,
	}

	outPath := filepath.Join(dir, "test.kshrk")
	sw, err := archive.NewEncryptedStreamWriter(outPath, recipients)
	if err != nil {
		t.Fatalf("buildEncryptedArchive NewEncryptedStreamWriter: %v", err)
	}
	for _, r := range records {
		if err := sw.WriteRecord(r); err != nil {
			t.Fatalf("buildEncryptedArchive WriteRecord: %v", err)
		}
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("buildEncryptedArchive Finish: %v", err)
	}
	return outPath
}

// TestRedact_EncryptedRoundTrip redacts an encrypted source archive in place:
// the source is decrypted with Identities, redacted, and re-encrypted with
// Recipients. The redacted output must itself be encrypted (a plain Open
// fails) and, once decrypted, must have the Secret value scrubbed and record
// Encrypted=true.
func TestRedact_EncryptedRoundTrip(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("my-password"))
	records := []*capture.Record{
		secretRecord("r1", "default", "db-creds", map[string]string{"password": encoded}, nil),
	}

	recipients, err := archive.RecipientsFromPassphrase(redactEncryptTestPassphrase)
	if err != nil {
		t.Fatalf("RecipientsFromPassphrase: %v", err)
	}
	identities, err := archive.IdentitiesFromPassphrase(redactEncryptTestPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}

	src := buildEncryptedArchive(t, records, recipients)
	dst := src + "-redacted.kshrk"
	t.Cleanup(func() { os.Remove(dst) })

	result, err := Archive(src, dst, Options{
		RedactSecrets: true,
		Identities:    identities,
		Recipients:    recipients,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.SecretsRedacted != 1 {
		t.Errorf("expected 1 redacted record, got %d", result.SecretsRedacted)
	}

	// Output must be encrypted: a plain Open fails.
	if _, err := archive.Open(dst); err == nil {
		t.Fatal("archive.Open on redacted encrypted archive succeeded, want error")
	}

	ar, err := archive.OpenWithIdentities(dst, identities)
	if err != nil {
		t.Fatalf("OpenWithIdentities: %v", err)
	}
	defer ar.Close()

	var meta capture.CaptureMetadata
	if err := ar.ReadMetadata(&meta); err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !meta.Encrypted {
		t.Error("redacted metadata.Encrypted = false, want true")
	}
	if !meta.Redacted {
		t.Error("redacted metadata.Redacted = false, want true")
	}

	data, err := ar.ReadRecord("/api/v1/namespaces/default/secrets", 0)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parsing record: %v", err)
	}
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(rec.ResponseBody, &obj)
	var dataMap map[string]string
	_ = json.Unmarshal(obj["data"], &dataMap)
	want := base64.StdEncoding.EncodeToString([]byte("REDACTED"))
	if dataMap["password"] != want {
		t.Errorf("data[password] = %q, want %q", dataMap["password"], want)
	}
}
