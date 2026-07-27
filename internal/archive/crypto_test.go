package archive

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
)

// testPassphrase is a fixed, documented test-only passphrase. It must never
// change without regenerating testdata/golden-v1-passphrase.kshrk.
const testPassphrase = "correct-horse-battery-staple-test-only" //nolint:gosec // test fixture, not a real secret

// sampleEncryptedArchive mirrors sampleArchive but encrypts the output to
// recipients using the production StreamWriter path.
func sampleEncryptedArchive(t *testing.T, path string, recipients []age.Recipient) {
	t.Helper()
	sw, err := NewEncryptedStreamWriter(path, recipients)
	if err != nil {
		t.Fatalf("NewEncryptedStreamWriter: %v", err)
	}
	rec := map[string]any{
		"id": "rec-1", "api_path": "/api/v1/namespaces/default/pods",
		"http_method": "GET", "response_code": 200,
		"response_body": map[string]any{"apiVersion": "v1", "kind": "PodList", "items": []any{}},
	}
	if err := sw.WriteRecord(rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	meta := map[string]any{"format_version": 1, "capture_id": "encrypted-v1", "record_count": 1}
	idx := map[string]any{
		"/api/v1/namespaces/default/pods": map[string]any{
			"api_path": "/api/v1/namespaces/default/pods", "seqs": []int{0},
		},
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

func TestEncryptedArchiveRoundTrip_Passphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encrypted.kshrk")
	recipients, err := RecipientsFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("RecipientsFromPassphrase: %v", err)
	}
	sampleEncryptedArchive(t, path, recipients)

	// A plain Open must fail with a clear "encrypted" message rather than
	// misreading the ciphertext as a corrupt zip.
	_, err = Open(path)
	if err == nil {
		t.Fatal("Open on encrypted archive succeeded without a key, want error")
	}
	if !strings.Contains(err.Error(), "is encrypted") {
		t.Errorf("Open error = %q, want it to mention the archive is encrypted", err)
	}

	identities, err := IdentitiesFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	ar, err := OpenWithIdentities(path, identities)
	if err != nil {
		t.Fatalf("OpenWithIdentities: %v", err)
	}
	defer ar.Close()

	var meta map[string]any
	if err := ar.ReadMetadata(&meta); err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta["capture_id"] != "encrypted-v1" {
		t.Errorf("metadata.capture_id = %v", meta["capture_id"])
	}
	data, err := ar.ReadRecord("/api/v1/namespaces/default/pods", 0)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if len(data) == 0 {
		t.Error("ReadRecord returned empty data")
	}
}

func TestEncryptedArchiveRoundTrip_X25519(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encrypted-x25519.kshrk")
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	sampleEncryptedArchive(t, path, []age.Recipient{id.Recipient()})

	ar, err := OpenWithIdentities(path, []age.Identity{id})
	if err != nil {
		t.Fatalf("OpenWithIdentities: %v", err)
	}
	defer ar.Close()

	var meta map[string]any
	if err := ar.ReadMetadata(&meta); err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta["capture_id"] != "encrypted-v1" {
		t.Errorf("metadata.capture_id = %v", meta["capture_id"])
	}
}

func TestEncryptedArchiveWrongPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encrypted.kshrk")
	recipients, err := RecipientsFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("RecipientsFromPassphrase: %v", err)
	}
	sampleEncryptedArchive(t, path, recipients)

	identities, err := IdentitiesFromPassphrase("wrong-passphrase")
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	_, err = OpenWithIdentities(path, identities)
	if err == nil {
		t.Fatal("OpenWithIdentities with wrong passphrase succeeded, want error")
	}
	// Assert the stable user-facing message so a regression back to a raw age
	// error (e.g. "no identity matched any of the recipients") is caught.
	if !strings.Contains(err.Error(), "incorrect passphrase or key") {
		t.Errorf("wrong-passphrase error = %q, want a clean 'incorrect passphrase or key' message", err)
	}
}

func TestEncryptedArchiveNoIdentities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encrypted.kshrk")
	recipients, err := RecipientsFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("RecipientsFromPassphrase: %v", err)
	}
	sampleEncryptedArchive(t, path, recipients)

	_, err = OpenWithIdentities(path, nil)
	if err == nil {
		t.Fatal("OpenWithIdentities with no identities succeeded, want error")
	}
	if !strings.Contains(err.Error(), "is encrypted") {
		t.Errorf("no-identities error = %q, want it to mention the archive is encrypted", err)
	}
}

// TestEncryptedArchiveTamperDetection flips a byte in the ciphertext and
// confirms age's AEAD authentication rejects it instead of silently
// returning corrupted plaintext.
func TestEncryptedArchiveTamperDetection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encrypted.kshrk")
	recipients, err := RecipientsFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("RecipientsFromPassphrase: %v", err)
	}
	sampleEncryptedArchive(t, path, recipients)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Flip a byte well past the age header/recipient stanzas, inside the
	// STREAM ciphertext, so this exercises payload authentication rather
	// than header parsing.
	tamperIdx := len(data) - 10
	data[tamperIdx] ^= 0xFF
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	identities, err := IdentitiesFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	ar, err := OpenWithIdentities(path, identities)
	if err != nil {
		// Failing at Open time (during the final-chunk authentication
		// DecryptReaderAt performs eagerly) is an acceptable place to catch this.
		return
	}
	defer ar.Close()
	if _, err := ar.ReadRecord("/api/v1/namespaces/default/pods", 0); err == nil {
		t.Fatal("ReadRecord succeeded against tampered ciphertext, want error")
	}
}

// TestEncryptedArchiveConcurrentReads pins age.DecryptReaderAt's documented
// promise that its ReaderAt is safe for concurrent ReadAt calls, since
// internal/server and internal/ui hold a single *Archive across goroutines.
func TestEncryptedArchiveConcurrentReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encrypted.kshrk")
	recipients, err := RecipientsFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("RecipientsFromPassphrase: %v", err)
	}
	sw, err := NewEncryptedStreamWriter(path, recipients)
	if err != nil {
		t.Fatalf("NewEncryptedStreamWriter: %v", err)
	}
	const n = 20
	idx := map[string]any{}
	seqs := make([]int, 0, n)
	for i := 0; i < n; i++ {
		rec := map[string]any{"id": "rec", "api_path": "/api/v1/pods", "n": i}
		if err := sw.WriteRecord(rec); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
		seqs = append(seqs, i)
	}
	idx["/api/v1/pods"] = map[string]any{"api_path": "/api/v1/pods", "seqs": seqs}
	if err := sw.Finish(map[string]any{"capture_id": "concurrent"}, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	identities, err := IdentitiesFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	ar, err := OpenWithIdentities(path, identities)
	if err != nil {
		t.Fatalf("OpenWithIdentities: %v", err)
	}
	defer ar.Close()

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			if _, err := ar.ReadRecord("/api/v1/pods", seq); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent ReadRecord failed: %v", err)
	}
}

// TestGoldenV1Passphrase opens a checked-in passphrase-encrypted fixture to
// catch any future age-library upgrade or kshrk change that breaks reading
// already-written encrypted archives. Regenerate with:
// go test ./internal/archive -run TestGoldenV1Passphrase -update
func TestGoldenV1Passphrase(t *testing.T) {
	golden := filepath.Join("testdata", "golden-v1-passphrase.kshrk")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		recipients, err := RecipientsFromPassphrase(testPassphrase)
		if err != nil {
			t.Fatalf("RecipientsFromPassphrase: %v", err)
		}
		sampleEncryptedArchive(t, golden, recipients)
		t.Logf("regenerated %s", golden)
	}
	if _, err := os.Stat(golden); err != nil {
		t.Skipf("golden fixture missing (run with -update): %v", err)
	}

	identities, err := IdentitiesFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	ar, err := OpenWithIdentities(golden, identities)
	if err != nil {
		t.Fatalf("OpenWithIdentities(golden): %v", err)
	}
	defer ar.Close()
	var meta struct {
		CaptureID string `json:"capture_id"`
	}
	if err := ar.ReadMetadata(&meta); err != nil {
		t.Fatalf("ReadMetadata(golden): %v", err)
	}
	if meta.CaptureID != "encrypted-v1" {
		t.Errorf("golden capture_id = %q, want encrypted-v1", meta.CaptureID)
	}
	if _, err := ar.ReadRecord("/api/v1/namespaces/default/pods", 0); err != nil {
		t.Fatalf("ReadRecord(golden): %v", err)
	}
}

// TestPlaintextArchiveStillOpens is a regression guard: adding encryption
// support must not change how existing plaintext archives are opened.
func TestPlaintextArchiveStillOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.kshrk")
	sampleArchive(t, path)

	ar, err := Open(path)
	if err != nil {
		t.Fatalf("Open(plaintext): %v", err)
	}
	defer ar.Close()
	var meta map[string]any
	if err := ar.ReadMetadata(&meta); err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta["capture_id"] != "golden-v1" {
		t.Errorf("metadata.capture_id = %v", meta["capture_id"])
	}

	// OpenWithIdentities must also work transparently on a plaintext archive.
	ar2, err := OpenWithIdentities(path, nil)
	if err != nil {
		t.Fatalf("OpenWithIdentities(plaintext, nil): %v", err)
	}
	defer ar2.Close()
}

// TestEncryptFile_DecryptFile_RoundTrip_Passphrase encrypts an existing
// plaintext archive after the fact, confirms the result is an age-encrypted
// file readable by OpenWithIdentities, then decrypts it back and confirms the
// bytes are byte-for-byte identical to the original plaintext archive.
func TestEncryptFile_DecryptFile_RoundTrip_Passphrase(t *testing.T) {
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "plain.kshrk")
	sampleArchive(t, plainPath)
	original, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatalf("ReadFile(plain): %v", err)
	}

	recipients, err := RecipientsFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("RecipientsFromPassphrase: %v", err)
	}
	encPath := filepath.Join(dir, "enc.kshrk")
	if err := EncryptFile(plainPath, encPath, recipients); err != nil {
		t.Fatalf("EncryptFile: %v", err)
	}

	if enc, err := IsEncrypted(encPath); err != nil || !enc {
		t.Fatalf("IsEncrypted(encrypted output) = %v, %v; want true, nil", enc, err)
	}
	// The source file must be untouched.
	unchanged, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatalf("ReadFile(plain) after EncryptFile: %v", err)
	}
	if string(unchanged) != string(original) {
		t.Fatal("EncryptFile modified its source file")
	}

	identities, err := IdentitiesFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	ar, err := OpenWithIdentities(encPath, identities)
	if err != nil {
		t.Fatalf("OpenWithIdentities(encrypted output): %v", err)
	}
	_ = ar.Close()

	decPath := filepath.Join(dir, "dec.kshrk")
	if err := DecryptFile(encPath, decPath, identities); err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	decrypted, err := os.ReadFile(decPath)
	if err != nil {
		t.Fatalf("ReadFile(decrypted): %v", err)
	}
	if string(decrypted) != string(original) {
		t.Error("DecryptFile output does not match the original plaintext archive byte-for-byte")
	}
}

// TestEncryptFile_DecryptFile_PreservesSourceMode confirms EncryptFile and
// DecryptFile create their output with the source file's permission bits,
// not os.Create's fixed 0666&umask — a restrictively-permissioned source
// (e.g. 0600) must not silently become more permissive in the copy, which
// matters most for DecryptFile since its output is plaintext.
func TestEncryptFile_DecryptFile_PreservesSourceMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits don't apply on Windows")
	}
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "plain.kshrk")
	sampleArchive(t, plainPath)
	if err := os.Chmod(plainPath, 0o600); err != nil {
		t.Fatalf("Chmod(plain, 0600): %v", err)
	}

	recipients, err := RecipientsFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("RecipientsFromPassphrase: %v", err)
	}
	encPath := filepath.Join(dir, "enc.kshrk")
	if err := EncryptFile(plainPath, encPath, recipients); err != nil {
		t.Fatalf("EncryptFile: %v", err)
	}
	if mode := statMode(t, encPath); mode != 0o600 {
		t.Errorf("encrypted output mode = %o, want 0600 (matching the source)", mode)
	}

	// Re-permission the encrypted file differently to confirm DecryptFile
	// independently preserves *its* source's mode, not a hardcoded default.
	if err := os.Chmod(encPath, 0o640); err != nil {
		t.Fatalf("Chmod(enc, 0640): %v", err)
	}
	identities, err := IdentitiesFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	decPath := filepath.Join(dir, "dec.kshrk")
	if err := DecryptFile(encPath, decPath, identities); err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	if mode := statMode(t, decPath); mode != 0o640 {
		t.Errorf("decrypted output mode = %o, want 0640 (matching the source)", mode)
	}
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	return fi.Mode().Perm()
}

// TestEncryptFile_DecryptFile_RoundTrip_X25519 is the recipient-mode
// equivalent of the passphrase round trip.
func TestEncryptFile_DecryptFile_RoundTrip_X25519(t *testing.T) {
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "plain.kshrk")
	sampleArchive(t, plainPath)

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	encPath := filepath.Join(dir, "enc.kshrk")
	if err := EncryptFile(plainPath, encPath, []age.Recipient{id.Recipient()}); err != nil {
		t.Fatalf("EncryptFile: %v", err)
	}

	decPath := filepath.Join(dir, "dec.kshrk")
	if err := DecryptFile(encPath, decPath, []age.Identity{id}); err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	original, err := os.ReadFile(plainPath)
	if err != nil {
		t.Fatalf("ReadFile(plain): %v", err)
	}
	decrypted, err := os.ReadFile(decPath)
	if err != nil {
		t.Fatalf("ReadFile(decrypted): %v", err)
	}
	if string(decrypted) != string(original) {
		t.Error("DecryptFile output does not match the original plaintext archive byte-for-byte")
	}
}

// TestEncryptFile_RejectsAlreadyEncrypted guards against silently
// double-wrapping an already-encrypted archive.
func TestEncryptFile_RejectsAlreadyEncrypted(t *testing.T) {
	dir := t.TempDir()
	recipients, err := RecipientsFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("RecipientsFromPassphrase: %v", err)
	}
	encPath := filepath.Join(dir, "enc.kshrk")
	sampleEncryptedArchive(t, encPath, recipients)

	if err := EncryptFile(encPath, filepath.Join(dir, "double.kshrk"), recipients); err == nil {
		t.Fatal("EncryptFile on an already-encrypted archive succeeded, want error")
	} else if !strings.Contains(err.Error(), "already encrypted") {
		t.Errorf("error = %q, want it to mention the archive is already encrypted", err)
	}
}

// TestDecryptFile_RejectsNotEncrypted guards against silently passing a
// plaintext archive through unchanged (or failing with a confusing
// "requires at least one identity" error) when given to DecryptFile.
func TestDecryptFile_RejectsNotEncrypted(t *testing.T) {
	dir := t.TempDir()
	plainPath := filepath.Join(dir, "plain.kshrk")
	sampleArchive(t, plainPath)

	identities, err := IdentitiesFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	err = DecryptFile(plainPath, filepath.Join(dir, "out.kshrk"), identities)
	if err == nil {
		t.Fatal("DecryptFile on a plaintext archive succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not encrypted") {
		t.Errorf("error = %q, want it to mention the archive is not encrypted", err)
	}
}

// TestDecryptFile_WrongPassphrase confirms the clean, stable error message.
func TestDecryptFile_WrongPassphrase(t *testing.T) {
	dir := t.TempDir()
	recipients, err := RecipientsFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("RecipientsFromPassphrase: %v", err)
	}
	encPath := filepath.Join(dir, "enc.kshrk")
	sampleEncryptedArchive(t, encPath, recipients)

	wrong, err := IdentitiesFromPassphrase("wrong-passphrase")
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	err = DecryptFile(encPath, filepath.Join(dir, "out.kshrk"), wrong)
	if err == nil {
		t.Fatal("DecryptFile with wrong passphrase succeeded, want error")
	}
	if !strings.Contains(err.Error(), "incorrect passphrase or key") {
		t.Errorf("error = %q, want a clean 'incorrect passphrase or key' message", err)
	}
}
