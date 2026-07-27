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
	cmd.Flags().Bool("encrypt", false, "encrypt the output archive with a passphrase (age); from --encrypt-passphrase-file, $"+encryptPassphraseEnv+", or an interactive prompt")
	cmd.Flags().String("encrypt-passphrase-file", "", "read the encryption passphrase from this file (first line) instead of prompting")
	cmd.Flags().StringArray("encrypt-recipient", nil, "age recipient public key (age1...) to encrypt to (repeatable); mutually exclusive with passphrase encryption")
	cmd.Flags().String("encrypt-recipients-file", "", "file of age recipient public keys (one per line) to encrypt to")
	_ = cmd.MarkFlagFilename("encrypt-passphrase-file")
	_ = cmd.MarkFlagFilename("encrypt-recipients-file")
}

// recipientsRequested reports whether the user asked for public-key (recipient)
// encryption, without parsing the keys. Used to detect the passphrase/recipient
// conflict before any interactive prompt runs.
func recipientsRequested(cmd *cobra.Command) bool {
	inline, _ := cmd.Flags().GetStringArray("encrypt-recipient")
	file, _ := cmd.Flags().GetString("encrypt-recipients-file")
	return len(inline) > 0 || file != ""
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

// encryptConfig is the resolved write-side encryption for a command.
type encryptConfig struct {
	enabled    bool
	mode       string // "passphrase" or "recipients" (for display); empty when disabled
	recipients []age.Recipient
}

// resolveEncryption determines how (if at all) a command should encrypt its
// output. Passphrase mode is enabled only by --encrypt or
// --encrypt-passphrase-file; once enabled, the passphrase itself comes from the
// file, $KSHRK_ENCRYPT_PASSPHRASE, or an interactive prompt (none of those
// sources enable encryption on their own). Recipient mode is enabled by
// --encrypt-recipient / --encrypt-recipients-file. The two modes are mutually
// exclusive: age requires a passphrase (scrypt) recipient to be the file's only
// recipient. The conflict is reported before any interactive passphrase prompt
// so the user isn't asked for a secret that can't be used.
func resolveEncryption(cmd *cobra.Command) (encryptConfig, error) {
	passphraseMode := encryptRequested(cmd)
	recipientMode := recipientsRequested(cmd)

	if passphraseMode && recipientMode {
		return encryptConfig{}, fmt.Errorf("passphrase encryption (--encrypt / --encrypt-passphrase-file) cannot be combined with recipient encryption (--encrypt-recipient / --encrypt-recipients-file): age requires a passphrase to be the only recipient")
	}

	switch {
	case recipientMode:
		recips, err := recipientsFromFlags(cmd)
		if err != nil {
			return encryptConfig{}, err
		}
		if len(recips) == 0 {
			return encryptConfig{}, fmt.Errorf("no age recipients found in --encrypt-recipient / --encrypt-recipients-file")
		}
		return encryptConfig{enabled: true, mode: "recipients", recipients: recips}, nil
	case passphraseMode:
		passphrase, _, err := resolveEncryptPassphrase(cmd)
		if err != nil {
			return encryptConfig{}, err
		}
		recips, err := archive.RecipientsFromPassphrase(passphrase)
		if err != nil {
			return encryptConfig{}, err
		}
		return encryptConfig{enabled: true, mode: "passphrase", recipients: recips}, nil
	default:
		return encryptConfig{}, nil
	}
}

// recipientsFromFlags parses all age recipients from --encrypt-recipient
// (inline age1... keys) and --encrypt-recipients-file (a recipients file).
func recipientsFromFlags(cmd *cobra.Command) ([]age.Recipient, error) {
	var recips []age.Recipient

	inline, _ := cmd.Flags().GetStringArray("encrypt-recipient")
	for _, s := range inline {
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return nil, fmt.Errorf("invalid --encrypt-recipient %q: %w", s, err)
		}
		recips = append(recips, r)
	}

	if file, _ := cmd.Flags().GetString("encrypt-recipients-file"); file != "" {
		f, err := os.Open(file)
		if err != nil {
			return nil, fmt.Errorf("opening recipients file %q: %w", file, err)
		}
		defer f.Close()
		fileRecips, err := age.ParseRecipients(f)
		if err != nil {
			return nil, fmt.Errorf("parsing recipients file %q: %w", file, err)
		}
		// age.ParseRecipients also accepts post-quantum "age1pq1..." (Hybrid)
		// keys, but --encrypt-recipient parses X25519 (age1...) only. Keep the
		// two flag variants consistent — and interoperable with age-keygen — by
		// rejecting non-X25519 recipients here too.
		for _, r := range fileRecips {
			if _, ok := r.(*age.X25519Recipient); !ok {
				return nil, fmt.Errorf("recipients file %q contains a non-X25519 recipient; only age1... public keys are supported", file)
			}
		}
		recips = append(recips, fileRecips...)
	}

	return recips, nil
}
