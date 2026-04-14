package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/phenixblue/k8shark/internal/server"
	"github.com/phenixblue/k8shark/internal/ui"
	"github.com/spf13/cobra"
)

var uiCmd = &cobra.Command{
	Use:   "ui <capture.tar.gz>",
	Short: "Open an interactive web explorer for a capture archive",
	Long: `Starts a local web UI for browsing a k8shark capture and also runs
the mock Kubernetes API server with generated kubeconfig output.`,
	Args: cobra.ExactArgs(1),
	RunE: runUI,
}

func init() {
	rootCmd.AddCommand(uiCmd)
	uiCmd.Flags().String("port", "0", "port for the local UI server (0 = random available port)")
	uiCmd.Flags().String("api-port", "0", "port for the mock API server (0 = random available port)")
	uiCmd.Flags().String("kubeconfig-out", "", "where to write the generated kubeconfig (default: ~/.kube/k8shark-<id>)")
	uiCmd.Flags().String("at", "", "pin UI data to a specific timestamp (RFC3339 or relative duration like -5m)")
}

func runUI(cmd *cobra.Command, args []string) error {
	archivePath := args[0]
	uiPort, _ := cmd.Flags().GetString("port")
	apiPort, _ := cmd.Flags().GetString("api-port")
	kubeconfigOut, _ := cmd.Flags().GetString("kubeconfig-out")
	at, _ := cmd.Flags().GetString("at")
	verbose, _ := cmd.Root().PersistentFlags().GetBool("verbose")

	mockSrv, err := server.Open(server.OpenOptions{
		ArchivePath:   archivePath,
		Port:          apiPort,
		KubeconfigOut: kubeconfigOut,
		At:            at,
		Verbose:       verbose,
	})
	if err != nil {
		return fmt.Errorf("opening mock API: %w", err)
	}

	uiSrv, err := ui.Open(ui.OpenOptions{
		ArchivePath: archivePath,
		Port:        uiPort,
		At:          at,
		Verbose:     verbose,
	})
	if err != nil {
		mockSrv.Shutdown()
		return fmt.Errorf("opening UI: %w", err)
	}

	fmt.Printf("k8shark mock server running\n")
	fmt.Printf("  Address:    %s\n", mockSrv.Address())
	fmt.Printf("  Kubeconfig: %s\n", mockSrv.KubeconfigPath())
	fmt.Printf("\nRun: export KUBECONFIG=%s\n", mockSrv.KubeconfigPath())
	fmt.Printf("Then use kubectl normally against the capture.\n\n")

	fmt.Printf("k8shark UI running\n")
	fmt.Printf("  Address: %s\n", uiSrv.Address())
	fmt.Printf("\nOpen this URL in your browser. Press Ctrl+C to stop.\n")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	uiSrv.Shutdown()
	mockSrv.Shutdown()

	return nil
}
