package cmd

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTestAnonymizeCommand builds a command with just the anonymize flags so
// resolveAnonymizeSalt can be exercised in isolation, mirroring
// newTestEncryptCommand in encrypt_flags_test.go.
func newTestAnonymizeCommand() *cobra.Command {
	cmd := &cobra.Command{}
	addAnonymizeFlags(cmd)
	return cmd
}

func TestResolveAnonymizeSalt_File(t *testing.T) {
	dir := t.TempDir()
	saltFile := filepath.Join(dir, "salt.txt")
	wantHex := strings.Repeat("ab", 32)
	if err := os.WriteFile(saltFile, []byte(wantHex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newTestAnonymizeCommand()
	_ = cmd.Flags().Set("anonymize-salt-file", saltFile)

	got, err := resolveAnonymizeSalt(cmd)
	if err != nil {
		t.Fatalf("resolveAnonymizeSalt: %v", err)
	}
	want, _ := hex.DecodeString(wantHex)
	if string(got) != string(want) {
		t.Errorf("salt = %x, want %x", got, want)
	}
}

func TestResolveAnonymizeSalt_FileTakesPrecedenceOverEnv(t *testing.T) {
	dir := t.TempDir()
	saltFile := filepath.Join(dir, "salt.txt")
	fileHex := strings.Repeat("11", 32)
	if err := os.WriteFile(saltFile, []byte(fileHex), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(anonymizeSaltEnv, strings.Repeat("22", 32))

	cmd := newTestAnonymizeCommand()
	_ = cmd.Flags().Set("anonymize-salt-file", saltFile)

	got, err := resolveAnonymizeSalt(cmd)
	if err != nil {
		t.Fatalf("resolveAnonymizeSalt: %v", err)
	}
	want, _ := hex.DecodeString(fileHex)
	if string(got) != string(want) {
		t.Errorf("salt = %x, want the file's value %x, not the env var's", got, want)
	}
}

func TestResolveAnonymizeSalt_Env(t *testing.T) {
	wantHex := strings.Repeat("33", 32)
	t.Setenv(anonymizeSaltEnv, wantHex)

	cmd := newTestAnonymizeCommand()
	got, err := resolveAnonymizeSalt(cmd)
	if err != nil {
		t.Fatalf("resolveAnonymizeSalt: %v", err)
	}
	want, _ := hex.DecodeString(wantHex)
	if string(got) != string(want) {
		t.Errorf("salt = %x, want %x", got, want)
	}
}

// With neither a file nor the env var set, resolveAnonymizeSalt must
// generate a usable salt on its own rather than erroring — an anonymize run
// without an explicit salt is a legitimate first use, not a mistake — and it
// must warn the caller (via stderr) that the value needs to be saved to
// reproduce this run, since without that warning a fresh random salt every
// invocation would look identical to a real re-run failing to reproduce.
func TestResolveAnonymizeSalt_GeneratesAndWarnsWhenUnset(t *testing.T) {
	cmd := newTestAnonymizeCommand()
	var stderr strings.Builder
	cmd.SetErr(&stderr)

	got, err := resolveAnonymizeSalt(cmd)
	if err != nil {
		t.Fatalf("resolveAnonymizeSalt: %v", err)
	}
	if len(got) != 32 {
		t.Errorf("generated salt is %d bytes, want 32", len(got))
	}

	printed := stderr.String()
	if !strings.Contains(printed, "save this") {
		t.Errorf("stderr does not warn to save the salt: %q", printed)
	}
	// The printed hex must decode back to exactly the salt that was returned
	// — otherwise a user who dutifully saves what's printed would not
	// actually reproduce this run.
	printedHex := hex.EncodeToString(got)
	if !strings.Contains(printed, printedHex) {
		t.Errorf("stderr does not contain the hex encoding of the actual returned salt %x: %q", got, printed)
	}
}

func TestDecodeSaltHex(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		got, err := decodeSaltHex("  " + strings.Repeat("ab", 16) + "\n")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 16 {
			t.Errorf("len = %d, want 16", len(got))
		}
	})
	t.Run("not hex", func(t *testing.T) {
		if _, err := decodeSaltHex("not-hex-at-all"); err == nil {
			t.Error("want an error for a non-hex string")
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := decodeSaltHex(""); err == nil {
			t.Error("want an error for an empty salt")
		}
	})
	t.Run("whitespace only", func(t *testing.T) {
		if _, err := decodeSaltHex("   \n"); err == nil {
			t.Error("want an error for a whitespace-only salt")
		}
	})

	// The salt is treated as a secret everywhere else in this file (no
	// inline --anonymize-salt flag, precisely to keep it out of shell
	// history) — an error that echoes the malformed value right back would
	// undo that the moment someone pastes the error into a log or a support
	// ticket. This asserts the invariant directly against a value that would
	// be very easy to recognize if it leaked, rather than just trusting the
	// implementation not to interpolate it.
	t.Run("does not echo the malformed value into the error", func(t *testing.T) {
		secret := "this-looks-like-a-leaked-secret-not-hex-zzz"
		_, err := decodeSaltHex(secret)
		if err == nil {
			t.Fatal("want an error")
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q leaks the malformed input value", err.Error())
		}
	})
}
