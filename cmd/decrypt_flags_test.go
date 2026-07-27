package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/spf13/cobra"
)

// newTestDecryptCommand builds a command carrying the persistent decrypt
// flags (which live on the root command in production).
func newTestDecryptCommand() *cobra.Command {
	cmd := &cobra.Command{}
	addDecryptFlags(cmd)
	// PersistentFlags on a standalone command aren't merged into Flags() until
	// execution, so expose them for GetString in these unit tests.
	cmd.Flags().AddFlagSet(cmd.PersistentFlags())
	return cmd
}

// writeEncryptedArchive writes a minimal passphrase-encrypted archive at path.
func writeEncryptedArchive(t *testing.T, path, passphrase string) {
	t.Helper()
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		t.Fatalf("NewScryptRecipient: %v", err)
	}
	r.SetWorkFactor(10) // fast KDF for tests
	sw, err := archive.NewEncryptedStreamWriter(path, []age.Recipient{r})
	if err != nil {
		t.Fatalf("NewEncryptedStreamWriter: %v", err)
	}
	rec := map[string]any{"id": "r1", "api_path": "/api/v1/nodes"}
	if _, err := sw.WriteRecord(rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if err := sw.Finish(map[string]any{"capture_id": "enc"}, map[string]any{}, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

func writePlaintextArchive(t *testing.T, path string) {
	t.Helper()
	sw, err := archive.NewStreamWriter(path)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	rec := map[string]any{"id": "r1", "api_path": "/api/v1/nodes"}
	if _, err := sw.WriteRecord(rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if err := sw.Finish(map[string]any{"capture_id": "plain"}, map[string]any{}, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

// TestResolveDecryptIdentities_PlaintextNeedsNoKey is the key regression guard:
// a plaintext archive with no decrypt flags resolves to nil identities (no
// prompt, no error) and opens normally.
func TestResolveDecryptIdentities_PlaintextNeedsNoKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.kshrk")
	writePlaintextArchive(t, path)

	cmd := newTestDecryptCommand()
	ids, err := resolveDecryptIdentities(cmd, path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ids != nil {
		t.Errorf("identities = %v, want nil for a plaintext archive", ids)
	}
	// And it must actually open with those (nil) identities.
	ar, err := archive.OpenWithIdentities(path, ids)
	if err != nil {
		t.Fatalf("OpenWithIdentities(plaintext): %v", err)
	}
	_ = ar.Close()
}

func TestResolveDecryptIdentities_PassphraseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enc.kshrk")
	const pass = "decrypt-test-passphrase"
	writeEncryptedArchive(t, path, pass)

	passFile := filepath.Join(dir, "pass.txt")
	if err := os.WriteFile(passFile, []byte(pass+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cmd := newTestDecryptCommand()
	_ = cmd.Flags().Set("decrypt-passphrase-file", passFile)

	ids, err := resolveDecryptIdentities(cmd, path)
	if err != nil {
		t.Fatalf("resolveDecryptIdentities: %v", err)
	}
	ar, err := archive.OpenWithIdentities(path, ids)
	if err != nil {
		t.Fatalf("OpenWithIdentities: %v", err)
	}
	defer ar.Close()
	var meta map[string]any
	if err := ar.ReadMetadata(&meta); err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta["capture_id"] != "enc" {
		t.Errorf("capture_id = %v, want enc", meta["capture_id"])
	}
}

func TestResolveDecryptIdentities_Env(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enc.kshrk")
	const pass = "env-decrypt-passphrase"
	writeEncryptedArchive(t, path, pass)

	t.Setenv(decryptPassphraseEnv, pass)
	cmd := newTestDecryptCommand()

	ids, err := resolveDecryptIdentities(cmd, path)
	if err != nil {
		t.Fatalf("resolveDecryptIdentities: %v", err)
	}
	ar, err := archive.OpenWithIdentities(path, ids)
	if err != nil {
		t.Fatalf("OpenWithIdentities: %v", err)
	}
	_ = ar.Close()
}

// TestResolveDecryptIdentities_IdentityFile exercises the X25519 identity-file
// path end to end, even before M4 adds CLI flags to write such archives: the
// test encrypts to an X25519 recipient directly and decrypts via the parsed
// identity file.
func TestResolveDecryptIdentities_IdentityFile(t *testing.T) {
	dir := t.TempDir()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	path := filepath.Join(dir, "enc.kshrk")
	sw, err := archive.NewEncryptedStreamWriter(path, []age.Recipient{id.Recipient()})
	if err != nil {
		t.Fatalf("NewEncryptedStreamWriter: %v", err)
	}
	if _, err := sw.WriteRecord(map[string]any{"id": "r1", "api_path": "/api/v1/nodes"}); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if err := sw.Finish(map[string]any{"capture_id": "x25519"}, map[string]any{}, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	identFile := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(identFile, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile identity: %v", err)
	}

	cmd := newTestDecryptCommand()
	_ = cmd.Flags().Set("decrypt-identity-file", identFile)

	ids, err := resolveDecryptIdentities(cmd, path)
	if err != nil {
		t.Fatalf("resolveDecryptIdentities: %v", err)
	}
	ar, err := archive.OpenWithIdentities(path, ids)
	if err != nil {
		t.Fatalf("OpenWithIdentities: %v", err)
	}
	_ = ar.Close()
}

// TestResolveDecryptIdentities_EncryptedNonTTY verifies the loud-failure path:
// an encrypted archive, no key flags/env, and a non-TTY stdin must error
// instead of blocking on a prompt.
func TestResolveDecryptIdentities_EncryptedNonTTY(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enc.kshrk")
	writeEncryptedArchive(t, path, "whatever")

	t.Setenv(decryptPassphraseEnv, "")

	// Force stdin to a pipe so the terminal check is deterministic.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	cmd := newTestDecryptCommand()
	_, err = resolveDecryptIdentities(cmd, path)
	if err == nil {
		t.Fatal("expected an error for an encrypted archive with no key and no TTY")
	}
	if !strings.Contains(err.Error(), "is encrypted") {
		t.Errorf("error = %q, want it to mention the archive is encrypted", err)
	}
}

// TestResolveDecryptIdentities_MultiPathEncryptedSecond guards the diff
// two-archive case: when the first path is plaintext but a later one is
// encrypted, the resolver must still detect the need for a key (here, error
// on a non-TTY) rather than returning nil because only the first was checked.
func TestResolveDecryptIdentities_MultiPathEncryptedSecond(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "plain.kshrk")
	writePlaintextArchive(t, plain)
	enc := filepath.Join(dir, "enc.kshrk")
	writeEncryptedArchive(t, enc, "whatever")

	t.Setenv(decryptPassphraseEnv, "")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	cmd := newTestDecryptCommand()
	_, err = resolveDecryptIdentities(cmd, plain, enc)
	if err == nil {
		t.Fatal("expected an error: second path is encrypted, no key, no TTY")
	}
	if !strings.Contains(err.Error(), "enc.kshrk") || !strings.Contains(err.Error(), "is encrypted") {
		t.Errorf("error = %q, want it to name the encrypted (second) archive", err)
	}
}

// TestResolveDecryptIdentities_WrongPassphrase confirms the resolver succeeds
// (it can't know the key is wrong) but the subsequent open fails cleanly.
func TestResolveDecryptIdentities_WrongPassphrase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enc.kshrk")
	writeEncryptedArchive(t, path, "correct-pass")

	passFile := filepath.Join(dir, "pass.txt")
	if err := os.WriteFile(passFile, []byte("wrong-pass"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := newTestDecryptCommand()
	_ = cmd.Flags().Set("decrypt-passphrase-file", passFile)

	ids, err := resolveDecryptIdentities(cmd, path)
	if err != nil {
		t.Fatalf("resolveDecryptIdentities: %v", err)
	}
	if _, err := archive.OpenWithIdentities(path, ids); err == nil {
		t.Fatal("OpenWithIdentities with wrong passphrase succeeded, want error")
	}
}
