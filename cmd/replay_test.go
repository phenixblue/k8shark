package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newTestReplayCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("port", "0", "")
	cmd.Flags().String("kubeconfig-out", "", "")
	cmd.Flags().String("speed", "1x", "")
	cmd.Flags().String("from", "", "")
	cmd.Flags().String("to", "", "")
	cmd.Flags().Bool("loop", false, "")
	cmd.Flags().Bool("start-paused", false, "")
	addDecryptFlags(cmd)
	// PersistentFlags on a standalone command aren't merged into Flags() until
	// execution via cmd.Execute(); tests call runReplay directly, so merge
	// them explicitly (matches the pattern in decrypt_flags_test.go / diff_test.go).
	cmd.Flags().AddFlagSet(cmd.PersistentFlags())
	return cmd
}

func TestRunReplay_InvalidSpeed(t *testing.T) {
	arch := buildDiffArchive(t, healthyPodList)
	cmd := newTestReplayCommand()
	_ = cmd.Flags().Set("speed", "fast")

	err := runReplay(cmd, []string{arch})
	if err == nil {
		t.Fatal("expected error for invalid --speed")
	}
	if !strings.Contains(err.Error(), "speed") {
		t.Errorf("error = %v, want it to mention speed", err)
	}
}

func TestRunReplay_MissingArchive(t *testing.T) {
	cmd := newTestReplayCommand()
	if err := runReplay(cmd, []string{"/no/such/capture.kshrk"}); err == nil {
		t.Fatal("expected error for missing archive")
	}
}

func TestRunReplay_InvalidWindow(t *testing.T) {
	arch := buildDiffArchive(t, healthyPodList)
	cmd := newTestReplayCommand()
	// --to before --from is rejected.
	_ = cmd.Flags().Set("from", "-1m")
	_ = cmd.Flags().Set("to", "-5m")

	if err := runReplay(cmd, []string{arch}); err == nil {
		t.Fatal("expected error when --to precedes --from")
	}
}

// TestRunReplay_EncryptedArchiveWrongPassphrase is a regression guard for a
// gap where `replay` never resolved --decrypt-* flags: server.Replay was
// always called with nil Identities, so ANY archive.OpenWithIdentities call
// against an encrypted archive failed with the same "supply a decryption
// key" error regardless of what --decrypt-passphrase-file contained — a
// wrong passphrase and no passphrase were indistinguishable. Asserting the
// error specifically mentions "incorrect passphrase" (not "supply a
// decryption key") proves the flag is actually read and passed through to
// the decrypt attempt.
func TestRunReplay_EncryptedArchiveWrongPassphrase(t *testing.T) {
	arch := buildEncryptedDiffArchive(t, healthyPodList, "replay-encrypt-test-passphrase")

	passFile := filepath.Join(t.TempDir(), "wrong.txt")
	if err := os.WriteFile(passFile, []byte("not-the-passphrase"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cmd := newTestReplayCommand()
	if err := cmd.Flags().Set("decrypt-passphrase-file", passFile); err != nil {
		t.Fatalf("set decrypt-passphrase-file flag: %v", err)
	}

	err := runReplay(cmd, []string{arch})
	if err == nil {
		t.Fatal("expected an error for a wrong passphrase")
	}
	if !strings.Contains(err.Error(), "incorrect passphrase") {
		t.Errorf("error = %q, want it to mention an incorrect passphrase (proving the flag was actually used), not a generic \"encrypted\" error", err)
	}
}
