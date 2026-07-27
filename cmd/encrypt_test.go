package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/spf13/cobra"
)

func newTestEncryptCmdCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().StringP("output", "o", "", "")
	addEncryptFlags(cmd)
	// These tests assert on disk state and returned errors, not the printed
	// summary, so discard stdout rather than letting it leak to os.Stdout.
	cmd.SetOut(io.Discard)
	return cmd
}

func TestRunEncrypt_DefaultOutputAndRoundTrip(t *testing.T) {
	in := buildDiffArchive(t, healthyPodList)
	passFile := filepath.Join(t.TempDir(), "pass.txt")
	if err := os.WriteFile(passFile, []byte("encrypt-cmd-test-passphrase"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cmd := newTestEncryptCmdCommand()
	if err := cmd.Flags().Set("encrypt-passphrase-file", passFile); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if err := runEncrypt(cmd, []string{in}); err != nil {
		t.Fatalf("runEncrypt: %v", err)
	}

	wantOut := strings.TrimSuffix(in, ".kshrk") + "-encrypted.kshrk"
	if _, err := os.Stat(wantOut); err != nil {
		t.Fatalf("default output %q not created: %v", wantOut, err)
	}
	if enc, err := archive.IsEncrypted(wantOut); err != nil || !enc {
		t.Fatalf("IsEncrypted(output) = %v, %v; want true, nil", enc, err)
	}

	identities, err := archive.IdentitiesFromPassphrase("encrypt-cmd-test-passphrase")
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	ar, err := archive.OpenWithIdentities(wantOut, identities)
	if err != nil {
		t.Fatalf("OpenWithIdentities: %v", err)
	}
	_ = ar.Close()
}

func TestRunEncrypt_ExplicitOutput(t *testing.T) {
	in := buildDiffArchive(t, healthyPodList)
	out := filepath.Join(filepath.Dir(in), "custom.kshrk")

	cmd := newTestEncryptCmdCommand()
	if err := cmd.Flags().Set("output", out); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if err := cmd.Flags().Set("encrypt-recipient", mustGenerateRecipient(t)); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if err := runEncrypt(cmd, []string{in}); err != nil {
		t.Fatalf("runEncrypt: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("explicit output %q not created: %v", out, err)
	}
}

func TestRunEncrypt_NoMethodGiven(t *testing.T) {
	in := buildDiffArchive(t, healthyPodList)
	cmd := newTestEncryptCmdCommand()

	err := runEncrypt(cmd, []string{in})
	if err == nil {
		t.Fatal("expected an error when no encryption method is given")
	}
	if !strings.Contains(err.Error(), "no encryption method given") {
		t.Errorf("error = %q, want it to mention no encryption method given", err)
	}
}

func TestRunEncrypt_RejectsAlreadyEncrypted(t *testing.T) {
	in := buildDiffArchive(t, healthyPodList)
	cmd := newTestEncryptCmdCommand()
	if err := cmd.Flags().Set("encrypt-recipient", mustGenerateRecipient(t)); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if err := runEncrypt(cmd, []string{in}); err != nil {
		t.Fatalf("runEncrypt (first pass): %v", err)
	}
	encOut := strings.TrimSuffix(in, ".kshrk") + "-encrypted.kshrk"

	// Encrypting the already-encrypted output must be rejected, not silently
	// double-wrap it.
	cmd2 := newTestEncryptCmdCommand()
	if err := cmd2.Flags().Set("encrypt-recipient", mustGenerateRecipient(t)); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	err := runEncrypt(cmd2, []string{encOut})
	if err == nil {
		t.Fatal("expected an error encrypting an already-encrypted archive")
	}
	if !strings.Contains(err.Error(), "already encrypted") {
		t.Errorf("error = %q, want it to mention already encrypted", err)
	}
}

func TestRunEncrypt_SameOutputRejected(t *testing.T) {
	in := buildDiffArchive(t, healthyPodList)
	cmd := newTestEncryptCmdCommand()
	if err := cmd.Flags().Set("output", in); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if err := cmd.Flags().Set("encrypt-recipient", mustGenerateRecipient(t)); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if err := runEncrypt(cmd, []string{in}); err == nil {
		t.Fatal("expected an error when --output equals the input path")
	}
}

// mustGenerateRecipient returns a freshly generated age1... public key string.
func mustGenerateRecipient(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("GenerateX25519Identity: %v", err)
	}
	return id.Recipient().String()
}
