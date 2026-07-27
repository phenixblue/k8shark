package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTestEncryptCommand builds a command with just the encryption flags so
// resolveEncryptPassphrase can be exercised in isolation.
func newTestEncryptCommand() *cobra.Command {
	cmd := &cobra.Command{}
	addEncryptFlags(cmd)
	return cmd
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
// encryption requested, no file and no env, and stdin is not a terminal (as
// in `go test`), so it must return an error rather than block on a prompt.
func TestResolveEncryptPassphrase_NonTTYNoSource(t *testing.T) {
	// Ensure the env var is unset for this test regardless of the outer env.
	t.Setenv(encryptPassphraseEnv, "")
	cmd := newTestEncryptCommand()
	_ = cmd.Flags().Set("encrypt", "true")

	_, _, err := resolveEncryptPassphrase(cmd)
	if err == nil {
		t.Fatal("expected an error when no passphrase source and no TTY, got nil")
	}
	if !strings.Contains(err.Error(), "no passphrase provided") {
		t.Errorf("error = %q, want it to mention no passphrase provided", err)
	}
}
