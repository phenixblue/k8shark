package cmd

import (
	"fmt"

	"github.com/phenixblue/k8shark/internal/ui"
	"github.com/spf13/cobra"
)

var uiCmd = &cobra.Command{
	Use:   "ui <capture.tar.gz>",
	Short: "Open an interactive web explorer for a capture archive",
	Long: `Starts a local web UI for browsing a k8shark capture without kubectl,
including hierarchy navigation, filters, and raw JSON/YAML detail views.`,
	Args: cobra.ExactArgs(1),
	RunE: runUI,
}

func init() {
	rootCmd.AddCommand(uiCmd)
	uiCmd.Flags().String("port", "0", "port for the local UI server (0 = random available port)")
	uiCmd.Flags().String("at", "", "pin UI data to a specific timestamp (RFC3339 or relative duration like -5m)")
}

func runUI(cmd *cobra.Command, args []string) error {
	archivePath := args[0]
	port, _ := cmd.Flags().GetString("port")
	at, _ := cmd.Flags().GetString("at")
	verbose, _ := cmd.Root().PersistentFlags().GetBool("verbose")

	srv, err := ui.Open(ui.OpenOptions{
		ArchivePath: archivePath,
		Port:        port,
		At:          at,
		Verbose:     verbose,
	})
	if err != nil {
		return fmt.Errorf("opening UI: %w", err)
	}

	fmt.Printf("k8shark UI running\n")
	fmt.Printf("  Address: %s\n", srv.Address())
	fmt.Printf("\nOpen this URL in your browser. Press Ctrl+C to stop.\n")

	return srv.Wait()
}
