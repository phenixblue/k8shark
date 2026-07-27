package cmd

import (
	"fmt"
	"os"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/spf13/cobra"
)

var decryptCmd = &cobra.Command{
	Use:   "decrypt <capture.kshrk>",
	Short: "Decrypt an encrypted capture archive back to plaintext",
	Long: `Writes a plaintext copy of an age-encrypted .kshrk archive. The
original encrypted archive is not modified; the output defaults to
<in>-decrypted.kshrk.

Every other kshrk command (inspect, open, ui, replay, diff, query,
transitions, diagnose, redact) already reads an encrypted archive
transparently given a key — this command is for producing a standalone
plaintext copy, e.g. to hand off to a tool that can't decrypt, or before
re-encrypting to a different key/recipient set.`,
	Example: `  # Decrypt using a passphrase read from a file
  kshrk decrypt capture.kshrk --decrypt-passphrase-file ./pass.txt

  # Decrypt using an age identity (private key) file
  kshrk decrypt capture.kshrk --decrypt-identity-file ./key.txt

  # Choose the output path explicitly
  kshrk decrypt capture.kshrk --output plain.kshrk --decrypt-passphrase-file ./pass.txt`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeArchiveArg,
	RunE:              runDecrypt,
}

func init() {
	rootCmd.AddCommand(decryptCmd)
	decryptCmd.Flags().StringP("output", "o", "", "output archive path (default: <in>-decrypted.kshrk)")
	_ = decryptCmd.MarkFlagFilename("output", captureExt)
}

func runDecrypt(cmd *cobra.Command, args []string) error {
	in := args[0]
	out, _ := cmd.Flags().GetString("output")
	if out == "" {
		out = defaultCryptOutput(in, "decrypted")
	}

	if err := rejectSamePath(in, out); err != nil {
		return err
	}

	identities, err := resolveDecryptIdentities(cmd, in)
	if err != nil {
		return err
	}

	if err := archive.DecryptFile(in, out, identities); err != nil {
		return err
	}

	fi, _ := os.Stat(out)
	var size int64
	if fi != nil {
		size = fi.Size()
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Decrypted -> %s (%s)\n", out, formatBytes(size))
	return nil
}
