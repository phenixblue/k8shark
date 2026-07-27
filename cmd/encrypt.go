package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/spf13/cobra"
)

var encryptCmd = &cobra.Command{
	Use:   "encrypt <capture.kshrk>",
	Short: "Encrypt an existing capture archive after the fact",
	Long: `Writes an age-encrypted copy of an existing plaintext .kshrk archive.
The original archive is not modified; the output defaults to
<in>-encrypted.kshrk.

This is a whole-file transform, not a new capture: use it to encrypt an
archive you already have (e.g. before sharing it), as an alternative to
'kshrk capture --encrypt' at capture time.`,
	Example: `  # Encrypt with a passphrase, prompting for it
  kshrk encrypt capture.kshrk --encrypt

  # Encrypt using a passphrase read from a file (no prompt)
  kshrk encrypt capture.kshrk --encrypt-passphrase-file ./pass.txt

  # Encrypt to one or more age recipient public keys
  kshrk encrypt capture.kshrk --encrypt-recipient age1abc...

  # Choose the output path explicitly
  kshrk encrypt capture.kshrk --output shared.kshrk --encrypt-recipient age1abc...`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeArchiveArg,
	RunE:              runEncrypt,
}

func init() {
	rootCmd.AddCommand(encryptCmd)
	encryptCmd.Flags().StringP("output", "o", "", "output archive path (default: <in>-encrypted.kshrk)")
	_ = encryptCmd.MarkFlagFilename("output", captureExt)
	addEncryptFlags(encryptCmd)
}

func runEncrypt(cmd *cobra.Command, args []string) error {
	in := args[0]
	out, _ := cmd.Flags().GetString("output")
	if out == "" {
		out = defaultCryptOutput(in, "encrypted")
	}

	if err := rejectSamePath(in, out); err != nil {
		return err
	}

	enc, err := resolveEncryption(cmd)
	if err != nil {
		return err
	}
	if !enc.enabled {
		return fmt.Errorf("no encryption method given: use --encrypt, --encrypt-passphrase-file, --encrypt-recipient, or --encrypt-recipients-file")
	}

	if err := archive.EncryptFile(in, out, enc.recipients); err != nil {
		return err
	}

	fi, _ := os.Stat(out)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Encrypted (age %s) -> %s (%s)\n", enc.mode, out, formatBytes(size))
	return nil
}

// defaultCryptOutput derives the default output path for encrypt/decrypt:
// <in>-<suffix>.kshrk, trimming either the current or legacy archive
// extension so "capture.khsrk" yields "capture-encrypted.kshrk", not
// "capture.khsrk-encrypted.kshrk". Mirrors redact's default-output naming.
func defaultCryptOutput(in, suffix string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(in, ".kshrk"), ".khsrk")
	return base + "-" + suffix + ".kshrk"
}

// rejectSamePath refuses to let an output path overwrite its input,
// comparing absolute paths so relative-vs-absolute spellings of the same file
// are still caught. A failure resolving either path is itself an error,
// rather than silently comparing an unresolved path against a resolved one —
// which could miss a genuine in==out overwrite (e.g. if only one of the two
// paths is affected by an unresolvable working directory).
func rejectSamePath(in, out string) error {
	inAbs, err := filepath.Abs(in)
	if err != nil {
		return fmt.Errorf("resolving input path %q: %w", in, err)
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("resolving output path %q: %w", out, err)
	}
	if inAbs == outAbs {
		return fmt.Errorf("output path must differ from the input archive")
	}
	return nil
}
