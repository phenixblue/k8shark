package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/spf13/cobra"
)

// newTestEncryptCommand builds a command with just the encryption flags so
// resolveEncryptPassphrase can be exercised in isolation.
func newTestEncryptCommand() *cobra.Command {
	cmd := &cobra.Command{}
	addEncryptFlags(cmd)
	return cmd
}

func TestResolveEncryption_Disabled(t *testing.T) {
	cmd := newTestEncryptCommand()
	enc, err := resolveEncryption(cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enc.enabled {
		t.Error("enabled = true with no flags, want false")
	}
}

func TestResolveEncryption_RecipientInline(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	cmd := newTestEncryptCommand()
	_ = cmd.Flags().Set("encrypt-recipient", id.Recipient().String())

	enc, err := resolveEncryption(cmd)
	if err != nil {
		t.Fatalf("resolveEncryption: %v", err)
	}
	if !enc.enabled || enc.mode != "recipients" {
		t.Errorf("enc = %+v, want enabled recipients", enc)
	}
	if len(enc.recipients) != 1 {
		t.Errorf("recipients len = %d, want 1", len(enc.recipients))
	}
}

func TestResolveEncryption_RecipientsFile(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	dir := t.TempDir()
	recipFile := filepath.Join(dir, "recipients.txt")
	if err := os.WriteFile(recipFile, []byte("# a recipient\n"+id.Recipient().String()+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := newTestEncryptCommand()
	_ = cmd.Flags().Set("encrypt-recipients-file", recipFile)

	enc, err := resolveEncryption(cmd)
	if err != nil {
		t.Fatalf("resolveEncryption: %v", err)
	}
	if !enc.enabled || len(enc.recipients) != 1 {
		t.Errorf("enc = %+v, want enabled with 1 recipient", enc)
	}
}

// TestResolveEncryption_RecipientsFileRejectsNonX25519 keeps the two recipient
// flag variants consistent: age.ParseRecipients accepts post-quantum Hybrid
// (age1pq1...) keys, but --encrypt-recipient is X25519-only, so the file
// variant must reject non-X25519 recipients too.
func TestResolveEncryption_RecipientsFileRejectsNonX25519(t *testing.T) {
	hybrid, err := age.GenerateHybridIdentity()
	if err != nil {
		t.Fatalf("GenerateHybridIdentity: %v", err)
	}
	dir := t.TempDir()
	recipFile := filepath.Join(dir, "recipients.txt")
	if err := os.WriteFile(recipFile, []byte(hybrid.Recipient().String()+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := newTestEncryptCommand()
	_ = cmd.Flags().Set("encrypt-recipients-file", recipFile)

	_, err = resolveEncryption(cmd)
	if err == nil {
		t.Fatal("expected an error for a non-X25519 (Hybrid) recipient in the file")
	}
	if !strings.Contains(err.Error(), "non-X25519") {
		t.Errorf("error = %q, want it to mention a non-X25519 recipient", err)
	}
}

func TestResolveEncryption_PassphraseAndRecipientConflict(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	cmd := newTestEncryptCommand()
	_ = cmd.Flags().Set("encrypt", "true")
	_ = cmd.Flags().Set("encrypt-recipient", id.Recipient().String())

	_, err = resolveEncryption(cmd)
	if err == nil {
		t.Fatal("expected an error combining passphrase and recipient encryption")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("error = %q, want it to mention the combination is disallowed", err)
	}
}

func TestResolveEncryption_InvalidRecipient(t *testing.T) {
	cmd := newTestEncryptCommand()
	_ = cmd.Flags().Set("encrypt-recipient", "not-a-valid-age-key")

	if _, err := resolveEncryption(cmd); err == nil {
		t.Fatal("expected an error for an invalid --encrypt-recipient")
	}
}

func TestResolveEncrypt_PassphraseMode(t *testing.T) {
	dir := t.TempDir()
	passFile := filepath.Join(dir, "pass.txt")
	if err := os.WriteFile(passFile, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := newTestEncryptCommand()
	_ = cmd.Flags().Set("encrypt-passphrase-file", passFile)

	enc, err := resolveEncryption(cmd)
	if err != nil {
		t.Fatalf("resolveEncryption: %v", err)
	}
	if !enc.enabled || enc.mode != "passphrase" || len(enc.recipients) != 1 {
		t.Errorf("enc = %+v, want enabled passphrase with 1 recipient", enc)
	}
}

func TestResolveEncryptPassphrase_Disabled(t *testing.T) {
	cmd := newTestEncryptCommand()
	pass, enabled, err := resolveEncryptPassphrase(cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enabled {
		t.Error("enabled = true with no flags, want false")
	}
	if pass != "" {
		t.Errorf("pass = %q, want empty", pass)
	}
}

func TestResolveEncryptPassphrase_FromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pass.txt")
	if err := os.WriteFile(path, []byte("hunter2\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := newTestEncryptCommand()
	_ = cmd.Flags().Set("encrypt-passphrase-file", path)

	pass, enabled, err := resolveEncryptPassphrase(cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Error("enabled = false, want true (passphrase file implies encryption)")
	}
	// The trailing newline must be trimmed.
	if pass != "hunter2" {
		t.Errorf("pass = %q, want %q", pass, "hunter2")
	}
}

func TestResolveEncryptPassphrase_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := newTestEncryptCommand()
	_ = cmd.Flags().Set("encrypt-passphrase-file", path)

	if _, _, err := resolveEncryptPassphrase(cmd); err == nil {
		t.Fatal("expected error for empty passphrase file, got nil")
	}
}

func TestResolveEncryptPassphrase_FromEnv(t *testing.T) {
	t.Setenv(encryptPassphraseEnv, "env-secret")
	cmd := newTestEncryptCommand()
	_ = cmd.Flags().Set("encrypt", "true")

	pass, enabled, err := resolveEncryptPassphrase(cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Error("enabled = false, want true")
	}
	if pass != "env-secret" {
		t.Errorf("pass = %q, want %q", pass, "env-secret")
	}
}

// TestResolveEncryptPassphrase_NonTTYNoSource verifies the loud-failure path:
// encryption requested, no file and no env, and stdin is not a terminal, so
// it must return an error rather than block on a prompt.
func TestResolveEncryptPassphrase_NonTTYNoSource(t *testing.T) {
	// Ensure the env var is unset for this test regardless of the outer env.
	t.Setenv(encryptPassphraseEnv, "")

	// Force stdin to a pipe (never a TTY) so the test is deterministic even
	// when `go test` is run from an interactive terminal — otherwise
	// term.IsTerminal could be true and the resolver would block on the
	// interactive prompt.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	cmd := newTestEncryptCommand()
	_ = cmd.Flags().Set("encrypt", "true")

	_, _, err = resolveEncryptPassphrase(cmd)
	if err == nil {
		t.Fatal("expected an error when no passphrase source and no TTY, got nil")
	}
	if !strings.Contains(err.Error(), "no passphrase provided") {
		t.Errorf("error = %q, want it to mention no passphrase provided", err)
	}
}
