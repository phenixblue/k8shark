package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/phenixblue/k8shark/internal/config"
	"github.com/phenixblue/k8shark/internal/server"
	"github.com/phenixblue/k8shark/internal/ui"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var uiCmd = &cobra.Command{
	Use:   "ui <capture.kshrk>",
	Short: "Open an interactive web explorer for a capture archive",
	Long: `Starts a local web UI for browsing a k8shark capture — namespaces,
workloads, pods, object YAML/JSON, relationships, and a watch-event timeline —
and also runs the mock Kubernetes API server with generated kubeconfig output.
Ports default to random; pin them with --port / --api-port (or a ui: block in
the config file).`,
	Example: `  # Browse a capture in the web UI
  kshrk ui capture.kshrk

  # Pin the UI and mock API server ports
  kshrk ui capture.kshrk --port 8080 --api-port 8081

  # Open the UI pinned to a point in time
  kshrk ui capture.kshrk --at -5m

  # Watch a closed-loop simulation in the dashboard (see docs/kwok.md)
  kshrk ui capture.kshrk --with-kwok --with-controller-manager`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeArchiveArg,
	RunE:              runUI,
}

func init() {
	rootCmd.AddCommand(uiCmd)
	uiCmd.Flags().String("port", "0", "port for the local UI server (0 = random available port)")
	uiCmd.Flags().String("api-port", "0", "port for the mock API server (0 = random available port)")
	uiCmd.Flags().String("kubeconfig-out", "", "where to write the generated kubeconfig (default: ~/.kube/k8shark-<id>)")
	uiCmd.Flags().String("at", "", "pin UI data to a specific timestamp (RFC3339 or relative duration like -5m)")
	// Replay mode: when any of these is set, the dashboard becomes a VCR driven
	// by a shared replay clock (kubectl follows the same clock).
	uiCmd.Flags().String("speed", "", "replay mode: playback speed factor, e.g. 2x, 3x, 0.5x (enables replay)")
	uiCmd.Flags().String("from", "", "replay window start: RFC3339 or relative duration like -10m")
	uiCmd.Flags().String("to", "", "replay window end: RFC3339 or relative duration like -1m")
	uiCmd.Flags().Bool("loop", false, "replay mode: restart from the window start when the end is reached")
	uiCmd.Flags().Bool("start-paused", false, "replay mode: start paused (the UI defaults to this; pass --start-paused=false to auto-play)")
	uiCmd.Flags().Bool("writable", false, "replay mode: accept client writes into an in-memory overlay")
	uiCmd.Flags().Bool("with-kwok", false, "replay mode: also run a detected 'kwok' binary against the server to drive pod/node lifecycle (implies --writable)")
	uiCmd.Flags().Bool("with-controller-manager", false, controllerManagerFlagHelp)
}

func runUI(cmd *cobra.Command, args []string) error {
	archivePath := args[0]
	uiPort, _ := cmd.Flags().GetString("port")
	apiPort, _ := cmd.Flags().GetString("api-port")
	kubeconfigOut, _ := cmd.Flags().GetString("kubeconfig-out")
	at, _ := cmd.Flags().GetString("at")
	verbose, _ := cmd.Root().PersistentFlags().GetBool("verbose")

	// Fall back to config-file ui.port / ui.api_port when the flags were left at
	// their default; an explicitly-passed flag always wins.
	if cfg, err := config.Load(viper.ConfigFileUsed()); err == nil && cfg != nil {
		if !cmd.Flags().Changed("port") && cfg.UI.Port != "" {
			uiPort = cfg.UI.Port
		}
		if !cmd.Flags().Changed("api-port") && cfg.UI.APIPort != "" {
			apiPort = cfg.UI.APIPort
		}
	}

	// Replay mode is enabled when any replay flag is set; the mock server and the
	// dashboard then share one clock (kubectl and the UI stay coherent).
	speed, _ := cmd.Flags().GetString("speed")
	from, _ := cmd.Flags().GetString("from")
	to, _ := cmd.Flags().GetString("to")
	loop, _ := cmd.Flags().GetBool("loop")
	startPaused, _ := cmd.Flags().GetBool("start-paused")
	writable, _ := cmd.Flags().GetBool("writable")
	withKwok, _ := cmd.Flags().GetBool("with-kwok")
	withControllerManager, _ := cmd.Flags().GetBool("with-controller-manager")
	// --with-kwok and --with-controller-manager both drive the overlay from a
	// live process, so either implies --writable (and replay mode).
	if withKwok || withControllerManager {
		writable = true
	}
	replayMode := cmd.Flags().Changed("speed") || cmd.Flags().Changed("from") ||
		cmd.Flags().Changed("to") || loop || startPaused || writable
	startPaused = resolveUIStartPaused(replayMode, startPaused, cmd.Flags().Changed("start-paused"))

	if err := validateKwokFlags(withKwok, true); err != nil {
		return err
	}

	// --at pins a single instant, which is meaningless once the replay clock is
	// driving time — reject the combination rather than silently ignoring --at.
	if replayMode && cmd.Flags().Changed("at") {
		return fmt.Errorf("--at cannot be combined with replay flags (--speed/--from/--to/--loop/--start-paused/--writable/--with-kwok/--with-controller-manager); use --from/--to to set the replay window")
	}

	var mockSrv *server.Server
	var err error
	if replayMode {
		mockSrv, err = server.Replay(server.ReplayOptions{
			ArchivePath: archivePath, Port: apiPort, KubeconfigOut: kubeconfigOut,
			Speed: speed, From: from, To: to, Loop: loop, StartPaused: startPaused, PauseAtWindowEnd: true,
			Writable: writable, Verbose: verbose,
		})
	} else {
		mockSrv, err = server.Open(server.OpenOptions{
			ArchivePath: archivePath, Port: apiPort, KubeconfigOut: kubeconfigOut, At: at, Verbose: verbose,
		})
	}
	if err != nil {
		return fmt.Errorf("opening mock API: %w", err)
	}

	// Optionally launch a detected kwok and/or kube-controller-manager against the
	// server, mirroring `kshrk replay`. Started after the server is up (they need
	// the kubeconfig) and torn down on shutdown. waitForNodesReady guards against
	// their informers' first LIST racing the replay clock's startup jitter (see
	// cmd/kwok.go) — needed at most once even when both are enabled.
	var kwokCleanup, controllerManagerCleanup func()
	if withKwok || withControllerManager {
		waitForNodesReady(mockSrv.Address(), mockSrv.CertPEM(), nodesReadyTimeout)
	}
	if withKwok {
		kwokCleanup, err = startKwok(mockSrv.KubeconfigPath())
		if err != nil {
			mockSrv.Shutdown()
			return err
		}
	}
	if withControllerManager {
		controllerManagerCleanup, err = startControllerManager(mockSrv.KubeconfigPath(), mockSrv.KubernetesVersion())
		if err != nil {
			if kwokCleanup != nil {
				kwokCleanup()
			}
			mockSrv.Shutdown()
			return err
		}
	}

	uiSrv, err := ui.Open(ui.OpenOptions{
		ArchivePath: archivePath,
		Port:        uiPort,
		At:          at,
		Verbose:     verbose,
		Clock:       mockSrv.Clock(), // nil unless replay mode → shared clock
	})
	if err != nil {
		if controllerManagerCleanup != nil {
			controllerManagerCleanup()
		}
		if kwokCleanup != nil {
			kwokCleanup()
		}
		mockSrv.Shutdown()
		return fmt.Errorf("opening UI: %w", err)
	}

	fmt.Printf("k8shark mock server running\n")
	fmt.Printf("  Address:    %s\n", mockSrv.Address())
	fmt.Printf("  Kubeconfig: %s\n", mockSrv.KubeconfigPath())
	if c := mockSrv.Clock(); c != nil {
		wf, wt := c.Window()
		fmt.Printf("  Replay:     %s → %s · %s\n", wf.Format("15:04:05Z07:00"), wt.Format("15:04:05Z07:00"), formatSpeed(c.Speed()))
	}
	if withKwok {
		fmt.Printf("  KWOK:       on (driving pod/node lifecycle via bundled stages)\n")
	}
	if withControllerManager {
		fmt.Printf("  Controller-manager: on (reconciling %s)\n", strings.Join(controllerManagerControllers, ", "))
	}
	fmt.Printf("\nRun: export KUBECONFIG=%s\n", mockSrv.KubeconfigPath())
	fmt.Printf("Then use kubectl normally against the capture.\n\n")

	fmt.Printf("k8shark UI running\n")
	fmt.Printf("  Address: %s\n", uiSrv.Address())
	if replayMode {
		fmt.Printf("\nOpen this URL to drive replay (play/pause/seek/speed). Press Ctrl+C to stop.\n")
	} else {
		fmt.Printf("\nOpen this URL in your browser. Press Ctrl+C to stop.\n")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	uiSrv.Shutdown()
	if controllerManagerCleanup != nil {
		controllerManagerCleanup()
	}
	if kwokCleanup != nil {
		kwokCleanup()
	}
	mockSrv.Shutdown()

	return nil
}

// resolveUIStartPaused decides whether the dashboard's replay clock should
// begin paused. The Web UI is a VCR the user drives by hand, so — unlike the
// headless `kshrk replay` command, which defaults to auto-play — it defaults
// to paused whenever replay mode is on. An explicit --start-paused (true or
// false) always wins.
func resolveUIStartPaused(replayMode, startPaused, startPausedChanged bool) bool {
	if replayMode && !startPausedChanged {
		return true
	}
	return startPaused
}
