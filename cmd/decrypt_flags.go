package cmd

import (
	"fmt"
	"os"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// decryptPassphraseEnv is the environment variable consulted for the passphrase
// to decrypt an encrypted archive when no --decrypt-passphrase-file is given.
// It is read directly via os.Getenv, never through Viper, so secret material
// can never be bound from (or leak into) the YAML config file.
const decryptPassphraseEnv = "KSHRK_DECRYPT_PASSPHRASE"

// addDecryptFlags registers the read-side decryption flags as persistent flags
// on cmd, so every archive-reading subcommand accepts them.
func addDecryptFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().String("decrypt-passphrase-file", "", "read the passphrase for an encrypted archive from this file (first line)")
	cmd.PersistentFlags().String("decrypt-identity-file", "", "age identity (key) file to decrypt an encrypted archive")
	_ = cmd.MarkPersistentFlagFilename("decrypt-passphrase-file")
	_ = cmd.MarkPersistentFlagFilename("decrypt-identity-file")
}

// resolveDecryptIdentities returns the age identities needed to open the
// archive at path, or nil when no key is required. Key material is gathered
// from --decrypt-identity-file, --decrypt-passphrase-file, and
// $KSHRK_DECRYPT_PASSPHRASE (all optional and combinable — age tries each
// identity until one succeeds). When none is supplied, it peeks whether path
// is encrypted: a plaintext archive needs no key (returns nil, so plaintext
// archives open with zero interaction), while an encrypted one triggers an
// interactive passphrase prompt on a TTY, or a clear error when stdin is not a
// terminal (rather than hanging on an unanswerable prompt).
func resolveDecryptIdentities(cmd *cobra.Command, path string) ([]age.Identity, error) {
	passphraseFile, _ := cmd.Flags().GetString("decrypt-passphrase-file")
	identityFile, _ := cmd.Flags().GetString("decrypt-identity-file")

	var identities []age.Identity

	if identityFile != "" {
		ids, err := parseIdentityFile(identityFile)
		if err != nil {
			return nil, err
		}
		identities = append(identities, ids...)
	}

	passphrase := ""
	if passphraseFile != "" {
		p, err := readPassphraseFile(passphraseFile)
		if err != nil {
			return nil, err
		}
		passphrase = p
	} else if env := os.Getenv(decryptPassphraseEnv); env != "" {
		passphrase = env
	}
	if passphrase != "" {
		ids, err := archive.IdentitiesFromPassphrase(passphrase)
		if err != nil {
			return nil, err
		}
		identities = append(identities, ids...)
	}

	if len(identities) > 0 {
		return identities, nil
	}

	// No explicit key was given: only prompt if the archive actually needs
	// one, so plaintext archives (the common case) open untouched.
	encrypted, err := archive.IsEncrypted(path)
	if err != nil {
		// A stat/read failure here (e.g. missing file) is better surfaced by
		// the subsequent open with its own clear message; don't preempt it.
		return nil, nil //nolint:nilerr // intentional: defer the error to Open
	}
	if !encrypted {
		return nil, nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, fmt.Errorf("archive %q is encrypted: provide --decrypt-passphrase-file, --decrypt-identity-file, or $%s (an interactive prompt requires a terminal)", path, decryptPassphraseEnv)
	}
	pass, err := promptDecryptPassphrase(cmd, path)
	if err != nil {
		return nil, err
	}
	return archive.IdentitiesFromPassphrase(pass)
}

// parseIdentityFile parses an age identity (key) file into identities.
func parseIdentityFile(path string) ([]age.Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening identity file %q: %w", path, err)
	}
	defer f.Close()
	ids, err := age.ParseIdentities(f)
	if err != nil {
		return nil, fmt.Errorf("parsing identity file %q: %w", path, err)
	}
	return ids, nil
}

// promptDecryptPassphrase reads a single passphrase from the terminal (no
// confirmation — the user is supplying an existing key, not setting a new
// one). The prompt goes to stderr so it never corrupts machine-readable
// stdout, and the typed characters are not echoed.
func promptDecryptPassphrase(cmd *cobra.Command, path string) (string, error) {
	out := cmd.ErrOrStderr()
	fmt.Fprintf(out, "Enter passphrase to decrypt %s: ", path)
	pass, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("reading passphrase: %w", err)
	}
	if len(pass) == 0 {
		return "", fmt.Errorf("passphrase must not be empty")
	}
	return string(pass), nil
}
