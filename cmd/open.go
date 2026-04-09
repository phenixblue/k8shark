package cmd

import (
	"fmt"

	"github.com/phenixblue/k8shark/internal/server"
	"github.com/spf13/cobra"
)

var openCmd = &cobra.Command{
	Use:   "open <capture.tar.gz>",
	Short: "Open a capture file and start a mock Kubernetes API server",
	Long: `Extracts a k8shark capture archive, starts a local mock Kubernetes
API server, and writes a kubeconfig so kubectl can connect immediately.`,
	Args: cobra.ExactArgs(1),
	RunE: runOpen,
}

func init() {
	rootCmd.AddCommand(openCmd)
	openCmd.Flags().String("port", "0", "port for the mock API server (0 = random available port)")
	openCmd.Flags().String("kubeconfig-out", "", "where to write the generated kubeconfig (default: ~/.kube/k8shark-<id>)")
	openCmd.Flags().String("at", "", "pin replay to a specific timestamp (RFC3339, e.g. 2026-04-09T10:00:00Z)")
}

func runOpen(cmd *cobra.Command, args []string) error {
	archivePath := args[0]

	port, _ := cmd.Flags().GetString("port")
	kubeconfigOut, _ := cmd.Flags().GetString("kubeconfig-out")
	at, _ := cmd.Flags().GetString("at")
	verbose, _ := cmd.Root().PersistentFlags().GetBool("verbose")

	opts := server.OpenOptions{
		ArchivePath:   archivePath,
		Port:          port,
		KubeconfigOut: kubeconfigOut,
		At:            at,
		Verbose:       verbose,
	}

	srv, err := server.Open(opts)
	if err != nil {
		return fmt.Errorf("opening capture: %w", err)
	}

	fmt.Printf("k8shark mock server running\n")
	fmt.Printf("  Address:    %s\n", srv.Address())
	fmt.Printf("  Kubeconfig: %s\n", srv.KubeconfigPath())
	fmt.Printf("\nRun: export KUBECONFIG=%s\n", srv.KubeconfigPath())
	fmt.Printf("Then use kubectl normally against the capture.\n")
	fmt.Printf("\nPress Ctrl+C to stop.\n")

	return srv.Wait()
}
