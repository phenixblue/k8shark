package cmd

import (
	"fmt"
	"os"

	"github.com/phenixblue/k8shark/internal/config"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(validateCmd)
}

var validateCmd = &cobra.Command{
	Use:          "validate",
	Short:        "Validate a capture config file without connecting to a cluster",
	Long:         `Parse and validate a k8shark capture config file, reporting any errors or warnings without connecting to a cluster or making any API calls.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if cfgFile == "" {
			return fmt.Errorf("--config is required")
		}

		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}

		// Count distinct non-empty namespaces across all resources.
		nsSet := make(map[string]struct{})
		for _, r := range cfg.Resources {
			for _, ns := range r.Namespaces {
				if ns != "" {
					nsSet[ns] = struct{}{}
				}
			}
		}

		// Print warnings to stderr.
		for _, w := range config.Warnings(cfg) {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}

		fmt.Printf("✓ Config valid (%d resources, %d namespaces, duration %s)\n",
			len(cfg.Resources), len(nsSet), cfg.Duration)
		return nil
	},
}
