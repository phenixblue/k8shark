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
	Short: "Capture Kubernetes cluster state to a .tar.gz archive",
	Long: `Runs a series of Kubernetes API read operations at defined intervals
for a configured duration. All responses are recorded and packaged into a
single .tar.gz capture file for later replay.`,
	RunE: runCapture,
}

func init() {
	rootCmd.AddCommand(captureCmd)
	captureCmd.Flags().StringP("output", "o", "", "output file path (default: ./k8shark-<timestamp>.tar.gz)")
	captureCmd.Flags().String("kubeconfig", "", "path to kubeconfig (defaults to KUBECONFIG env, then ~/.kube/config)")
	captureCmd.Flags().String("duration", "", "capture duration, overrides config file value (e.g. 10m, 1h)")
	captureCmd.Flags().Bool("redact-secrets", false, "redact Secret data and stringData values from the archive after capture")
	captureCmd.Flags().StringArray("allow-secret", nil, "namespace/name of secret to preserve when --redact-secrets is set (repeatable)")
	_ = viper.BindPFlag("output", captureCmd.Flags().Lookup("output"))
	_ = viper.BindPFlag("kubeconfig", captureCmd.Flags().Lookup("kubeconfig"))
	_ = viper.BindPFlag("duration", captureCmd.Flags().Lookup("duration"))
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

	// Optional in-place redaction of Secret values.
	if doRedact, _ := cmd.Flags().GetBool("redact-secrets"); doRedact {
		allows, _ := cmd.Flags().GetStringArray("allow-secret")
		allowList := make(map[string]bool, len(allows))
		for _, a := range allows {
			allowList[a] = true
		}

		tmpPath := sum.OutputPath + ".redacting"
		n, err := redact.Archive(sum.OutputPath, tmpPath, allowList)
		if err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("redacting secrets: %w", err)
		}
		if err := os.Rename(tmpPath, sum.OutputPath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("replacing archive with redacted version: %w", err)
		}
		fmt.Fprintf(os.Stdout, "  Redacted:  %d secret(s) scrubbed from archive\n", n)
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

// formatBytes returns a human-readable byte size string.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
