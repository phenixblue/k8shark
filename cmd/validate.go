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

		// Summarize resources, noting auto-discovery entries.
		var allCount, explicitCount int
		for _, r := range cfg.Resources {
			if r.All {
				allCount++
			} else {
				explicitCount++
			}
		}
		var resourceSummary string
		switch {
		case allCount > 0 && explicitCount > 0:
			resourceSummary = fmt.Sprintf("%d explicit + %d auto-discovered", explicitCount, allCount)
		case allCount > 0:
			resourceSummary = fmt.Sprintf("%d auto-discovered", allCount)
		default:
			resourceSummary = fmt.Sprintf("%d", explicitCount)
		}

		// Count distinct non-empty namespaces; detect wildcard.
		nsSet := make(map[string]struct{})
		wildcardNS := false
		for _, r := range cfg.Resources {
			for _, ns := range r.Namespaces {
				if ns == "*" {
					wildcardNS = true
				} else if ns != "" {
					nsSet[ns] = struct{}{}
				}
			}
		}
		var namespaceSummary string
		if wildcardNS {
			namespaceSummary = "all namespaces"
		} else {
			namespaceSummary = fmt.Sprintf("%d namespace(s)", len(nsSet))
		}

		// Print warnings to stderr.
		for _, w := range config.Warnings(cfg) {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}

		fmt.Printf("✓ Config valid (%s resource(s), %s, duration %s)\n",
			resourceSummary, namespaceSummary, cfg.Duration)
		return nil
	},
}
