package cmd

import (
	"fmt"
	"os"

	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/config"
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
	if err := engine.Run(); err != nil {
		return fmt.Errorf("capture failed: %w", err)
	}
	fmt.Fprintf(os.Stdout, "Capture complete -> %s\n", cfg.Output)
	return nil
}
