package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/config"
	"github.com/phenixblue/k8shark/internal/redact"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var captureCmd = &cobra.Command{
	Use:   "capture",
	Short: "Capture Kubernetes cluster state to a .kshrk archive",
	Long: `Runs a series of Kubernetes API read operations at defined intervals
for a configured duration. All responses are recorded and packaged into a
single .kshrk capture file for later replay.

Resources, namespaces, and intervals come from the --config file. Use
--auto-discover to capture every available API resource without listing them,
--output - to stream records as NDJSON to stdout, --redact-secrets to scrub
Secret values from the archive after capture, and --encrypt (passphrase) or
--encrypt-recipient (age public keys) to write the archive as an encrypted
(age) envelope.`,
	Example: `  # Capture using a config file
  kshrk capture --config k8shark.yaml

  # Auto-discover and capture all resources for 5 minutes
  kshrk capture --auto-discover --duration 5m

  # Stream records as NDJSON to stdout instead of writing an archive
  kshrk capture --config k8shark.yaml --output -

  # Capture, then redact Secret values from the archive
  kshrk capture --config k8shark.yaml --redact-secrets

  # Capture and encrypt the archive, prompting for a passphrase
  kshrk capture --config k8shark.yaml --encrypt

  # Encrypt using a passphrase read from a file (no prompt)
  kshrk capture --config k8shark.yaml --encrypt-passphrase-file ./pass.txt

  # Encrypt to one or more age recipient public keys (shareable)
  kshrk capture --config k8shark.yaml --encrypt-recipient age1abc... --encrypt-recipient age1def...`,
	RunE: runCapture,
}

func init() {
	rootCmd.AddCommand(captureCmd)
	captureCmd.Flags().StringP("output", "o", "", "output file path (default: ./k8shark-<timestamp>.kshrk)")
	captureCmd.Flags().String("kubeconfig", "", "path to kubeconfig (defaults to KUBECONFIG env, then ~/.kube/config)")
	captureCmd.Flags().String("duration", "", "capture duration, overrides config file value (e.g. 10m, 1h)")
	captureCmd.Flags().Bool("auto-discover", false, "auto-discover and capture all available API resources")
	captureCmd.Flags().Bool("redact-secrets", false, "redact Secret data and stringData values from the archive after capture")
	captureCmd.Flags().StringArray("allow-secret", nil, "namespace/name of secret to preserve when --redact-secrets is set (repeatable)")
	captureCmd.Flags().StringArray("redact-field", nil, "field redaction rule applied after capture: <fieldPath>:<Kind>:<replacement>[:<valueType>] (repeatable)")
	addEncryptFlags(captureCmd)
	_ = viper.BindPFlag("output", captureCmd.Flags().Lookup("output"))
	_ = viper.BindPFlag("kubeconfig", captureCmd.Flags().Lookup("kubeconfig"))
	_ = viper.BindPFlag("duration", captureCmd.Flags().Lookup("duration"))
	_ = viper.BindPFlag("auto_discover", captureCmd.Flags().Lookup("auto-discover"))
	// --output writes a *.kshrk archive, but also accepts "-" to stream NDJSON
	// to stdout. Scope file completion to *.kshrk while still offering "-".
	_ = captureCmd.RegisterFlagCompletionFunc("output", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if strings.HasPrefix(toComplete, "-") {
			return []string{"-\tstream NDJSON to stdout"}, cobra.ShellCompDirectiveNoFileComp
		}
		return []string{captureExt}, cobra.ShellCompDirectiveFilterFileExt
	})
}

func runCapture(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(viper.ConfigFileUsed())
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if v, _ := cmd.Flags().GetString("output"); v != "" {
		cfg.Output = v
	}
	if v, _ := cmd.Flags().GetString("kubeconfig"); v != "" {
		cfg.Kubeconfig = v
	}
	if v, _ := cmd.Flags().GetString("duration"); v != "" {
		cfg.DurationRaw = v
	}
	if cmd.Flags().Changed("auto-discover") {
		v, _ := cmd.Flags().GetBool("auto-discover")
		cfg.AutoDiscover = v
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// When streaming NDJSON to stdout, stdout must carry only records, so all
	// human-oriented output (spinner, status, summary) goes to stderr instead.
	streamingStdout := cfg.Output == "-"
	msgOut := os.Stdout
	if streamingStdout {
		msgOut = os.Stderr
	}

	// Reject incompatible flag combinations before any interactive passphrase
	// prompt so users aren't asked for a secret only to be told it can't be
	// used. These helpers check the flags without prompting.
	if (encryptRequested(cmd) || recipientsRequested(cmd)) && streamingStdout {
		return fmt.Errorf("archive encryption cannot be combined with --output - (NDJSON streaming to stdout is not encrypted)")
	}

	// Resolve redaction inputs up front: whether the capture will be redacted
	// determines how the archive is encrypted (see below).
	doRedactSecrets, allowList, fieldRules, err := resolveRedaction(cmd, cfg)
	if err != nil {
		return err
	}
	willRedact := doRedactSecrets || len(fieldRules) > 0

	// Resolve encryption before the (potentially long) capture starts so a bad
	// or missing passphrase fails fast rather than after minutes of polling.
	enc, err := resolveEncryption(cmd)
	if err != nil {
		return err
	}

	// Decide what the engine encrypts the on-disk archive to. When the capture
	// will be redacted afterwards, the engine encrypts the intermediate to an
	// ephemeral key so the redact pass can decrypt it in memory and re-encrypt
	// to the real recipients — without ever writing plaintext to disk and
	// without needing the recipients' private keys (which we don't have in
	// recipient mode). Otherwise the engine encrypts directly to the final
	// recipients.
	var engineRecipients, redactRecipients []age.Recipient
	var redactIdentities []age.Identity
	if enc.enabled {
		redactRecipients = enc.recipients
		if willRedact {
			eph, gerr := age.GenerateX25519Identity()
			if gerr != nil {
				return fmt.Errorf("generating ephemeral encryption key: %w", gerr)
			}
			engineRecipients = []age.Recipient{eph.Recipient()}
			redactIdentities = []age.Identity{eph}
		} else {
			engineRecipients = enc.recipients
		}
	}

	verbose, _ := cmd.Root().PersistentFlags().GetBool("verbose")

	engine, err := capture.NewEngine(cfg, verbose)
	if err != nil {
		return fmt.Errorf("initializing capture engine: %w", err)
	}
	engine.SetEncryption(engineRecipients)

	fmt.Fprintf(msgOut, "Starting capture -> %s\n", cfg.Output)

	// Spinner runs until capture finishes.
	stopSpinner := startSpinner(msgOut)
	sum, err := engine.Run()
	stopSpinner()

	if err != nil {
		return fmt.Errorf("capture failed: %w", err)
	}

	fmt.Fprintf(msgOut, "\nCapture complete\n")
	fmt.Fprintf(msgOut, "  Output:    %s (%s)\n", sum.OutputPath, formatBytes(sum.OutputSize))
	if enc.enabled {
		fmt.Fprintf(msgOut, "  Encrypted: yes (age %s)\n", enc.mode)
	}
	fmt.Fprintf(msgOut, "  Records:   %d across %d resource path(s)\n", sum.RecordCount, sum.ResourceCount)
	fmt.Fprintf(msgOut, "  Duration:  %s\n", sum.Duration)
	if sum.PodLogs.Attempted > 0 {
		fmt.Fprintf(msgOut, "  Pod logs:  %d/%d captured", sum.PodLogs.Captured, sum.PodLogs.Attempted)
		if sum.PodLogs.Skipped > 0 {
			fmt.Fprintf(msgOut, " (%d skipped)", sum.PodLogs.Skipped)
		}
		if sum.PodLogs.CapturedPrevious > 0 {
			fmt.Fprintf(msgOut, ", %d previous", sum.PodLogs.CapturedPrevious)
		}
		fmt.Fprintln(msgOut)
		if len(sum.PodLogs.Failures) > 0 {
			fmt.Fprintln(msgOut, "  Skipped (sample):")
			for _, f := range sum.PodLogs.Failures {
				fmt.Fprintf(msgOut, "    - %s/%s [container=%s]: %s\n",
					f.Namespace, f.Pod, f.Container, f.Reason)
			}
			if sum.PodLogs.Skipped > len(sum.PodLogs.Failures) {
				fmt.Fprintf(msgOut, "    ... and %d more (run with --verbose for full list)\n",
					sum.PodLogs.Skipped-len(sum.PodLogs.Failures))
			}
		}
	}

	// Post-capture redaction (inputs resolved before the capture started).
	if willRedact {
		tmpPath := sum.OutputPath + ".redacting"
		result, err := redact.Archive(sum.OutputPath, tmpPath, redact.Options{
			RedactSecrets: doRedactSecrets,
			AllowList:     allowList,
			Rules:         fieldRules,
			// Decrypt the (ephemerally-encrypted) intermediate and re-encrypt to
			// the real recipients so the redacted archive stays encrypted end to
			// end; both are nil for a plaintext capture.
			Identities: redactIdentities,
			Recipients: redactRecipients,
		})
		if err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("redacting archive: %w", err)
		}
		if err := os.Rename(tmpPath, sum.OutputPath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("replacing archive with redacted version: %w", err)
		}
		if result.SecretsRedacted > 0 || result.FieldsRedacted > 0 {
			fmt.Fprintf(msgOut, "  Redacted:  %d secret(s), %d record(s) with field rules applied\n",
				result.SecretsRedacted, result.FieldsRedacted)
		}
	}

	return nil
}

// resolveRedaction merges the --redact-secrets / --allow-secret / --redact-field
// CLI flags with any redaction block in the config file into the effective
// redaction inputs for a capture.
func resolveRedaction(cmd *cobra.Command, cfg *config.Config) (doRedactSecrets bool, allowList map[string]bool, fieldRules []config.RedactionRule, err error) {
	doRedactSecrets, _ = cmd.Flags().GetBool("redact-secrets")
	if cfg.Redaction.RedactSecrets {
		doRedactSecrets = true
	}

	allows, _ := cmd.Flags().GetStringArray("allow-secret")
	allowList = make(map[string]bool, len(allows))
	for _, a := range allows {
		allowList[a] = true
	}
	for _, a := range cfg.Redaction.AllowSecrets {
		allowList[a] = true
	}

	fieldRules = append(fieldRules, cfg.Redaction.Rules...)
	redactFields, _ := cmd.Flags().GetStringArray("redact-field")
	for _, rf := range redactFields {
		rule, perr := parseRedactField(rf)
		if perr != nil {
			return false, nil, nil, perr
		}
		fieldRules = append(fieldRules, rule)
	}
	return doRedactSecrets, allowList, fieldRules, nil
}

// startSpinner prints a rotating spinner on w until the returned stop function
// is called. stop blocks until the spinner goroutine has exited.
func startSpinner(w *os.File) func() {
	frames := []string{"|", "/", "-", "\\"}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				fmt.Fprint(w, "\r")
				return
			case <-time.After(100 * time.Millisecond):
				fmt.Fprintf(w, "\r  capturing %s", frames[i%len(frames)])
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}
