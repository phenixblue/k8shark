package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/config"
	"github.com/phenixblue/k8shark/internal/redact"
	"github.com/spf13/cobra"
)

var redactCmd = &cobra.Command{
	Use:   "redact <capture.kshrk> [--out <redacted.kshrk>]",
	Short: "Redact Secret data and arbitrary fields from a capture archive",
	Long: `Produces a new capture archive with Kubernetes Secret data replaced by
"REDACTED" and any configured field-level redaction rules applied. The original
archive is not modified; the output defaults to <in>-redacted.kshrk.

Field rules can be supplied via --redact-field (repeatable) with the format:
  <fieldPath>:<Kind>:<replacement>[:<valueType>]

Rules may also be loaded from a config file's redaction.rules block via --config.`,
	Example: `  # Redact all Secret data and stringData values
  kshrk redact capture.kshrk --redact-secrets

  # Apply a single field-level redaction rule
  kshrk redact capture.kshrk --redact-field "data.api-key:ConfigMap:REDACTED"

  # Use redaction.rules from a config file, writing to a chosen path
  kshrk redact capture.kshrk --out safe.kshrk --config k8shark.yaml

  # Redact and encrypt the output to an age recipient (decrypt an encrypted
  # source with --decrypt-passphrase-file / --decrypt-identity-file)
  kshrk redact capture.kshrk --redact-secrets --encrypt-recipient age1abc...`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeArchiveArg,
	RunE:              runRedact,
}

func init() {
	rootCmd.AddCommand(redactCmd)
	redactCmd.Flags().String("out", "", "output archive path (default: <in>-redacted.kshrk)")
	redactCmd.Flags().Bool("redact-secrets", false, "redact all Kubernetes Secret data and stringData values")
	redactCmd.Flags().StringArray("allow-secret", nil, "namespace/name of secret to preserve (repeatable)")
	redactCmd.Flags().StringArray("redact-field", nil, "field redaction rule: <fieldPath>:<Kind>:<replacement>[:<valueType>] (repeatable)")
	redactCmd.Flags().String("config", "", "capture config file whose redaction.rules block is applied")
	_ = redactCmd.MarkFlagFilename("out", captureExt)
	_ = redactCmd.MarkFlagFilename("config", configExts...)
	// Write-side encryption for the redacted output (read-side --decrypt-* are
	// persistent flags from the root command).
	addEncryptFlags(redactCmd)
}

// parseRedactField parses a --redact-field flag value of the form:
// <fieldPath>:<Kind>:<replacement>[:<valueType>]
func parseRedactField(s string) (config.RedactionRule, error) {
	parts := strings.SplitN(s, ":", 4)
	if len(parts) < 3 {
		return config.RedactionRule{}, fmt.Errorf("--redact-field %q: expected format <fieldPath>:<Kind>:<replacement>[:<valueType>]", s)
	}
	rule := config.RedactionRule{
		FieldPath:   parts[0],
		Kind:        parts[1],
		Replacement: parts[2],
	}
	if len(parts) == 4 {
		rule.ValueType = parts[3]
	}
	return rule, nil
}

func runRedact(cmd *cobra.Command, args []string) error {
	in := args[0]
	out, _ := cmd.Flags().GetString("out")
	doRedactSecrets, _ := cmd.Flags().GetBool("redact-secrets")
	allows, _ := cmd.Flags().GetStringArray("allow-secret")
	redactFields, _ := cmd.Flags().GetStringArray("redact-field")
	cfgFile, _ := cmd.Flags().GetString("config")

	if out == "" {
		// Trim either the current or the legacy extension so redacting a
		// "*.khsrk" capture yields "<in>-redacted.kshrk", not
		// "<in>.khsrk-redacted.kshrk".
		base := strings.TrimSuffix(strings.TrimSuffix(in, ".kshrk"), ".khsrk")
		out = base + "-redacted.kshrk"
	}

	// Refuse to overwrite the source.
	if err := rejectSamePath(in, out); err != nil {
		return err
	}

	allowList := make(map[string]bool, len(allows))
	for _, a := range allows {
		allowList[a] = true
	}

	// Collect field rules: config file first, then CLI overrides appended.
	var rules []config.RedactionRule

	if cfgFile != "" {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return fmt.Errorf("loading config: %w", err)
		}
		if cfg.Redaction.RedactSecrets {
			doRedactSecrets = true
		}
		for _, a := range cfg.Redaction.AllowSecrets {
			allowList[a] = true
		}
		rules = append(rules, cfg.Redaction.Rules...)
	}

	for _, rf := range redactFields {
		rule, err := parseRedactField(rf)
		if err != nil {
			return err
		}
		rules = append(rules, rule)
	}

	// Read-side: decrypt an encrypted source archive if a key was supplied.
	identities, err := resolveDecryptIdentities(cmd, in)
	if err != nil {
		return err
	}

	// Write-side: optionally re-encrypt the redacted output (passphrase or
	// recipient keys).
	enc, err := resolveEncryption(cmd)
	if err != nil {
		return err
	}
	if !enc.enabled {
		if srcEncrypted, _ := archive.IsEncrypted(in); srcEncrypted {
			// The source is encrypted but no output encryption was requested —
			// warn that the redacted copy will be written in plaintext so an
			// encrypted capture isn't silently downgraded.
			fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: source archive is encrypted but the redacted output %q will be written in plaintext; pass a passphrase (--encrypt / --encrypt-passphrase-file) or recipient (--encrypt-recipient / --encrypt-recipients-file) flag to keep it encrypted\n", out)
		}
	}

	result, err := redact.Archive(in, out, redact.Options{
		RedactSecrets: doRedactSecrets,
		AllowList:     allowList,
		Rules:         rules,
		Identities:    identities,
		Recipients:    enc.recipients,
	})
	if err != nil {
		return err
	}

	fi, _ := os.Stat(out)
	size := int64(0)
	if fi != nil {
		size = fi.Size()
	}

	fmt.Printf("Redacted %d secret(s), %d field(s) → %s (%d bytes)\n",
		result.SecretsRedacted, result.FieldsRedacted, out, size)
	return nil
}
