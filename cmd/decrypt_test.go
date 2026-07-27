package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newTestDecryptCmdCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().StringP("output", "o", "", "")
	addDecryptFlags(cmd)
	// PersistentFlags on a standalone command aren't merged into Flags() until
	// execution via cmd.Execute(); tests call runDecrypt directly, so merge
	// them explicitly (matches the pattern used elsewhere in this package).
	cmd.Flags().AddFlagSet(cmd.PersistentFlags())
	return cmd
}

func TestRunDecrypt_DefaultOutputAndRoundTrip(t *testing.T) {
	const passphrase = "decrypt-cmd-test-passphrase"
	in := buildEncryptedDiffArchive(t, healthyPodList, passphrase)

	passFile := filepath.Join(t.TempDir(), "pass.txt")
	if err := os.WriteFile(passFile, []byte(passphrase), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cmd := newTestDecryptCmdCommand()
	if err := cmd.Flags().Set("decrypt-passphrase-file", passFile); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if err := runDecrypt(cmd, []string{in}); err != nil {
		t.Fatalf("runDecrypt: %v", err)
	}

	wantOut := strings.TrimSuffix(in, ".kshrk") + "-decrypted.kshrk"
	fi, err := os.Stat(wantOut)
	if err != nil {
		t.Fatalf("default output %q not created: %v", wantOut, err)
	}
	if fi.Size() == 0 {
		t.Fatal("decrypted output is empty")
	}
}

func TestRunDecrypt_WrongPassphrase(t *testing.T) {
	in := buildEncryptedDiffArchive(t, healthyPodList, "correct-passphrase")

	passFile := filepath.Join(t.TempDir(), "wrong.txt")
	if err := os.WriteFile(passFile, []byte("wrong-passphrase"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := newTestDecryptCmdCommand()
	if err := cmd.Flags().Set("decrypt-passphrase-file", passFile); err != nil {
		t.Fatalf("set flag: %v", err)
	}

	err := runDecrypt(cmd, []string{in})
	if err == nil {
		t.Fatal("expected an error for a wrong passphrase")
	}
	if !strings.Contains(err.Error(), "incorrect passphrase") {
		t.Errorf("error = %q, want it to mention an incorrect passphrase", err)
	}
}

func TestRunDecrypt_RejectsNotEncrypted(t *testing.T) {
	in := buildDiffArchive(t, healthyPodList)
	cmd := newTestDecryptCmdCommand()

	err := runDecrypt(cmd, []string{in})
	if err == nil {
		t.Fatal("expected an error decrypting a plaintext archive")
	}
	if !strings.Contains(err.Error(), "not encrypted") {
		t.Errorf("error = %q, want it to mention the archive is not encrypted", err)
	}
}

func TestRunDecrypt_SameOutputRejected(t *testing.T) {
	const passphrase = "decrypt-cmd-test-passphrase"
	in := buildEncryptedDiffArchive(t, healthyPodList, passphrase)

	passFile := filepath.Join(t.TempDir(), "pass.txt")
	if err := os.WriteFile(passFile, []byte(passphrase), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cmd := newTestDecryptCmdCommand()
	if err := cmd.Flags().Set("output", in); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if err := cmd.Flags().Set("decrypt-passphrase-file", passFile); err != nil {
		t.Fatalf("set flag: %v", err)
	}

	if err := runDecrypt(cmd, []string{in}); err == nil {
		t.Fatal("expected an error when --output equals the input path")
	}
}
