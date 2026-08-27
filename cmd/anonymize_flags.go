package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// anonymizeSaltEnv is the environment variable consulted for the anonymize
// salt when no --anonymize-salt-file is given. Read directly via os.Getenv,
// never through Viper, for the same reason encryptPassphraseEnv is
// (encrypt_flags.go): so it can never be bound from — or leak into — the
// YAML config file. #137's design notes call this out specifically for the
// anonymize salt: #139 wants these configs distributed as shared, versioned
// artifacts across a team, and a salt sitting in a config file would
// silently turn a personal secret into a team-shared one the moment a
// profile is exported.
const anonymizeSaltEnv = "KSHRK_ANONYMIZE_SALT"

// addAnonymizeFlags registers the salt-resolution flag on cmd.
//
// Deliberately no inline --anonymize-salt=<value> flag, matching
// resolveEncryptPassphrase's precedent for the same reason: a secret passed
// as a literal command-line argument sits in shell history and is visible to
// anyone on the box via `ps`. The salt is a materially smaller secret than an
// encryption passphrase — knowing it only weakens anonymization, and only if
// an attacker also has candidate original values to test against — but the
// leak vector is exactly as cheap to hit by accident (a shared bastion host,
// a copy-pasted command in a support ticket), so the same discipline applies.
func addAnonymizeFlags(cmd *cobra.Command) {
	cmd.Flags().String("anonymize-salt-file", "", "read the anonymize salt from this file (first line, hex-encoded) instead of $"+anonymizeSaltEnv+" or generating one")
	_ = cmd.MarkFlagFilename("anonymize-salt-file")
}

// resolveAnonymizeSalt resolves the salt: --anonymize-salt-file, then
// $KSHRK_ANONYMIZE_SALT, then a freshly generated 32-byte salt. A freshly
// generated salt is printed once to stderr, hex-encoded, with a warning that
// it must be saved to reproduce these exact aliases on a re-run — otherwise
// every invocation without an explicit salt would anonymize the same capture
// differently, which is not a re-run at all, just a fresh random one that
// happens to share a namespace's *structure*.
func resolveAnonymizeSalt(cmd *cobra.Command) ([]byte, error) {
	if file, _ := cmd.Flags().GetString("anonymize-salt-file"); file != "" {
		hexStr, err := readPassphraseFile(file)
		if err != nil {
			return nil, fmt.Errorf("reading anonymize salt file: %w", err)
		}
		return decodeSaltHex(hexStr)
	}

	if hexStr := os.Getenv(anonymizeSaltEnv); hexStr != "" {
		return decodeSaltHex(hexStr)
	}

	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating anonymize salt: %w", err)
	}
	saltHex := hex.EncodeToString(salt)
	fmt.Fprintf(cmd.ErrOrStderr(),
		"generated anonymize salt (save this to reproduce these exact aliases on a re-run):\n  %s\n\n"+
			"Pass it back with --anonymize-salt-file (a file containing this value) or $%s.\n",
		saltHex, anonymizeSaltEnv)
	return salt, nil
}

// decodeSaltHex decodes a hex-encoded salt string, as read from a file or an
// environment variable. Rejected outright if it doesn't decode: a salt that
// silently truncated or got mangled in transit would still "work" (any bytes
// produce a valid HMAC key), so a decode error is the only signal available
// that something is wrong, and it must not be swallowed.
func decodeSaltHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	salt, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("anonymize salt %q is not valid hex: %w", s, err)
	}
	if len(salt) == 0 {
		return nil, fmt.Errorf("anonymize salt is empty")
	}
	return salt, nil
}
