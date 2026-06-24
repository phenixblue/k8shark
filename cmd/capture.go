package cmd

import (
	"fmt"
	"os"
	"time"

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
--output - to stream records as NDJSON to stdout, and --redact-secrets to
scrub Secret values from the archive after capture.`,
	Example: `  # Capture using a config file
  kshrk capture --config k8shark.yaml

  # Auto-discover and capture all resources for 5 minutes
  kshrk capture --auto-discover --duration 5m

  # Stream records as NDJSON to stdout instead of writing an archive
  kshrk capture --config k8shark.yaml --output -

  # Capture, then redact Secret values from the archive
  kshrk capture --config k8shark.yaml --redact-secrets`,
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
	_ = viper.BindPFlag("output", captureCmd.Flags().Lookup("output"))
	_ = viper.BindPFlag("kubeconfig", captureCmd.Flags().Lookup("kubeconfig"))
	_ = viper.BindPFlag("duration", captureCmd.Flags().Lookup("duration"))
	_ = viper.BindPFlag("auto_discover", captureCmd.Flags().Lookup("auto-discover"))
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

	verbose, _ := cmd.Root().PersistentFlags().GetBool("verbose")

	engine, err := capture.NewEngine(cfg, verbose)
	if err != nil {
		return fmt.Errorf("initializing capture engine: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Starting capture -> %s\n", cfg.Output)

	// Spinner runs until capture finishes.
	stopSpinner := startSpinner(os.Stdout)
	sum, err := engine.Run()
	stopSpinner()

	if err != nil {
		return fmt.Errorf("capture failed: %w", err)
	}

	fmt.Fprintf(os.Stdout, "\nCapture complete\n")
	fmt.Fprintf(os.Stdout, "  Output:    %s (%s)\n", sum.OutputPath, formatBytes(sum.OutputSize))
	fmt.Fprintf(os.Stdout, "  Records:   %d across %d resource path(s)\n", sum.RecordCount, sum.ResourceCount)
	fmt.Fprintf(os.Stdout, "  Duration:  %s\n", sum.Duration)
	if sum.PodLogs.Attempted > 0 {
		fmt.Fprintf(os.Stdout, "  Pod logs:  %d/%d captured", sum.PodLogs.Captured, sum.PodLogs.Attempted)
		if sum.PodLogs.Skipped > 0 {
			fmt.Fprintf(os.Stdout, " (%d skipped)", sum.PodLogs.Skipped)
		}
		if sum.PodLogs.CapturedPrevious > 0 {
			fmt.Fprintf(os.Stdout, ", %d previous", sum.PodLogs.CapturedPrevious)
		}
		fmt.Fprintln(os.Stdout)
		if len(sum.PodLogs.Failures) > 0 {
			fmt.Fprintln(os.Stdout, "  Skipped (sample):")
			for _, f := range sum.PodLogs.Failures {
				fmt.Fprintf(os.Stdout, "    - %s/%s [container=%s]: %s\n",
					f.Namespace, f.Pod, f.Container, f.Reason)
			}
			if sum.PodLogs.Skipped > len(sum.PodLogs.Failures) {
				fmt.Fprintf(os.Stdout, "    ... and %d more (run with --verbose for full list)\n",
					sum.PodLogs.Skipped-len(sum.PodLogs.Failures))
			}
		}
	}

	// Post-capture redaction: merge --redact-secrets / --redact-field CLI flags
	// with any redaction.rules defined in the config file.
	doRedactSecrets, _ := cmd.Flags().GetBool("redact-secrets")
	if cfg.Redaction.RedactSecrets {
		doRedactSecrets = true
	}
	allows, _ := cmd.Flags().GetStringArray("allow-secret")
	allowList := make(map[string]bool, len(allows))
	for _, a := range allows {
		allowList[a] = true
	}
	for _, a := range cfg.Redaction.AllowSecrets {
		allowList[a] = true
	}
	redactFields, _ := cmd.Flags().GetStringArray("redact-field")
	var fieldRules []config.RedactionRule
	fieldRules = append(fieldRules, cfg.Redaction.Rules...)
	for _, rf := range redactFields {
		rule, err := parseRedactField(rf)
		if err != nil {
			return err
		}
		fieldRules = append(fieldRules, rule)
	}

	if doRedactSecrets || len(fieldRules) > 0 {
		tmpPath := sum.OutputPath + ".redacting"
		result, err := redact.Archive(sum.OutputPath, tmpPath, redact.Options{
			RedactSecrets: doRedactSecrets,
			AllowList:     allowList,
			Rules:         fieldRules,
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
			fmt.Fprintf(os.Stdout, "  Redacted:  %d secret(s), %d record(s) with field rules applied\n",
				result.SecretsRedacted, result.FieldsRedacted)
		}
	}

	return nil
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
