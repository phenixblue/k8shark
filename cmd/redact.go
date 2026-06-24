package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/phenixblue/k8shark/internal/config"
	"github.com/phenixblue/k8shark/internal/redact"
	"github.com/spf13/cobra"
)

var redactCmd = &cobra.Command{
	Use:   "redact --in <capture.kshrk> [--out <redacted.kshrk>]",
	Short: "Redact Secret data and arbitrary fields from a capture archive",
	Long: `Produces a new capture archive with Kubernetes Secret data replaced by
"REDACTED" and any configured field-level redaction rules applied.
The original archive is not modified.

Field rules can be supplied via --redact-field (repeatable) with the format:
  <fieldPath>:<Kind>:<replacement>[:<valueType>]

Examples:
  kshrk redact --in capture.kshrk --redact-secrets
  kshrk redact --in capture.kshrk --redact-field "data.api-key:ConfigMap:REDACTED"
  kshrk redact --in capture.kshrk --config k8shark.yaml`,
	RunE: runRedact,
}

func init() {
	rootCmd.AddCommand(redactCmd)
	redactCmd.Flags().String("in", "", "source capture archive (required)")
	redactCmd.Flags().String("out", "", "output archive path (default: <in>-redacted.kshrk)")
	redactCmd.Flags().Bool("redact-secrets", false, "redact all Kubernetes Secret data and stringData values")
	redactCmd.Flags().StringArray("allow-secret", nil, "namespace/name of secret to preserve (repeatable)")
	redactCmd.Flags().StringArray("redact-field", nil, "field redaction rule: <fieldPath>:<Kind>:<replacement>[:<valueType>] (repeatable)")
	redactCmd.Flags().String("config", "", "capture config file whose redaction.rules block is applied")
	_ = redactCmd.MarkFlagRequired("in")
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

func runRedact(cmd *cobra.Command, _ []string) error {
	in, _ := cmd.Flags().GetString("in")
	out, _ := cmd.Flags().GetString("out")
	doRedactSecrets, _ := cmd.Flags().GetBool("redact-secrets")
	allows, _ := cmd.Flags().GetStringArray("allow-secret")
	redactFields, _ := cmd.Flags().GetStringArray("redact-field")
	cfgFile, _ := cmd.Flags().GetString("config")

	if out == "" {
		base := strings.TrimSuffix(in, ".kshrk")
		out = base + "-redacted.kshrk"
	}

	// Refuse to overwrite the source.
	inAbs, _ := filepath.Abs(in)
	outAbs, _ := filepath.Abs(out)
	if inAbs == outAbs {
		return fmt.Errorf("--out must differ from --in")
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

	result, err := redact.Archive(in, out, redact.Options{
		RedactSecrets: doRedactSecrets,
		AllowList:     allowList,
		Rules:         rules,
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
