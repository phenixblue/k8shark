package cmd

import (
	"fmt"
	"os"
	"strings"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// encryptPassphraseEnv is the environment variable consulted for the archive
// encryption passphrase when no --encrypt-passphrase-file is given. It is read
// directly via os.Getenv, never through Viper, so the passphrase can never be
// bound from (or leak into) the YAML config file.
const encryptPassphraseEnv = "KSHRK_ENCRYPT_PASSPHRASE"

// addEncryptFlags registers the write-side encryption flags on cmd.
func addEncryptFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("encrypt", false, "encrypt the output archive (age); passphrase from --encrypt-passphrase-file, $"+encryptPassphraseEnv+", or an interactive prompt")
	cmd.Flags().String("encrypt-passphrase-file", "", "read the encryption passphrase from this file (first line) instead of prompting")
	_ = cmd.MarkFlagFilename("encrypt-passphrase-file")
}

// resolveEncryptPassphrase determines whether the command should encrypt its
// output and, if so, resolves the passphrase. Encryption is enabled when
// --encrypt is set or a --encrypt-passphrase-file is given. The passphrase is
// resolved in priority order: the file, then $KSHRK_ENCRYPT_PASSPHRASE, then
// an interactive TTY prompt (with confirmation). When encryption is requested
// but no passphrase source is available and stdin is not a TTY, it fails
// loudly rather than hanging on a prompt that can never be answered.
//
// It returns (passphrase, enabled, error). passphrase is empty when enabled
// is false.
// encryptRequested reports whether the user asked for output encryption
// (via --encrypt or --encrypt-passphrase-file), without resolving or
// prompting for the passphrase. Callers use it to reject incompatible flag
// combinations before any interactive prompt runs.
func encryptRequested(cmd *cobra.Command) bool {
	encrypt, _ := cmd.Flags().GetBool("encrypt")
	passphraseFile, _ := cmd.Flags().GetString("encrypt-passphrase-file")
	return encrypt || passphraseFile != ""
}

func resolveEncryptPassphrase(cmd *cobra.Command) (string, bool, error) {
	if !encryptRequested(cmd) {
		return "", false, nil
	}
	passphraseFile, _ := cmd.Flags().GetString("encrypt-passphrase-file")

	if passphraseFile != "" {
		pass, err := readPassphraseFile(passphraseFile)
		if err != nil {
			return "", true, err
		}
		return pass, true, nil
	}

	if pass := os.Getenv(encryptPassphraseEnv); pass != "" {
		return pass, true, nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", true, fmt.Errorf("encryption requested but no passphrase provided: set --encrypt-passphrase-file or $%s (an interactive prompt requires a terminal)", encryptPassphraseEnv)
	}

	pass, err := promptNewPassphrase(cmd)
	if err != nil {
		return "", true, err
	}
	return pass, true, nil
}

// readPassphraseFile reads a passphrase from the first line of path, trimming
// a single trailing newline (and any \r) so a file created with a trailing
// newline works as expected. An empty passphrase is rejected.
func readPassphraseFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading passphrase file %q: %w", path, err)
	}
	pass := string(b)
	if i := strings.IndexAny(pass, "\r\n"); i >= 0 {
		pass = pass[:i]
	}
	if pass == "" {
		return "", fmt.Errorf("passphrase file %q is empty", path)
	}
	return pass, nil
}

// promptNewPassphrase interactively reads a passphrase twice from the
// terminal and confirms the two entries match. The typed characters are not
// echoed.
func promptNewPassphrase(cmd *cobra.Command) (string, error) {
	fd := int(os.Stdin.Fd())
	// Prompts go to stderr so they never corrupt machine-readable output on
	// stdout (or a stdout redirect).
	out := cmd.ErrOrStderr()

	fmt.Fprint(out, "Enter passphrase for archive encryption: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("reading passphrase: %w", err)
	}
	if len(first) == 0 {
		return "", fmt.Errorf("passphrase must not be empty")
	}

	fmt.Fprint(out, "Confirm passphrase: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("reading passphrase confirmation: %w", err)
	}

	if string(first) != string(second) {
		return "", fmt.Errorf("passphrases do not match")
	}
	return string(first), nil
}

// encryptOptionsFromPassphrase builds the age recipients and identities for a
// passphrase. Recipients encrypt the output; identities decrypt an existing
// encrypted archive (needed when redacting an encrypted capture in place).
func encryptOptionsFromPassphrase(passphrase string) (recipients []age.Recipient, identities []age.Identity, err error) {
	recipients, err = archive.RecipientsFromPassphrase(passphrase)
	if err != nil {
		return nil, nil, err
	}
	identities, err = archive.IdentitiesFromPassphrase(passphrase)
	if err != nil {
		return nil, nil, err
	}
	return recipients, identities, nil
}
