package cmd

import (
	"bufio"
	"fmt"
	"os"
	"time"

	"github.com/phenixblue/k8shark/internal/server"
	"github.com/phenixblue/k8shark/internal/ui"
	"github.com/spf13/cobra"
)

var replayCmd = &cobra.Command{
	Use:   "replay <capture.kshrk>",
	Short: "Replay a capture forward through time at a chosen speed",
	Long: `Plays a k8shark capture forward through time, streaming captured watch
events (ADDED/MODIFIED/DELETED) to clients as a replay clock advances. Unlike
'open --at', which jumps the whole view to a single instant, replay advances a
clock and streams change over time — so controllers/operators and 'kubectl get
--watch' observe the cluster changing exactly as it did during capture.

A kubeconfig is written just like 'open'; point kubectl or a controller at it.`,
	Example: `  # Replay the whole capture at twice its original speed
  kshrk replay capture.kshrk --speed 2x

  # Replay in slow motion
  kshrk replay capture.kshrk --speed 0.5x

  # Loop the last 10 minutes of the capture
  kshrk replay capture.kshrk --from -10m --to -1m --loop

  # Start paused (press Enter to begin)
  kshrk replay capture.kshrk --start-paused`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeArchiveArg,
	RunE:              runReplay,
}

func init() {
	rootCmd.AddCommand(replayCmd)
	replayCmd.Flags().String("port", "0", "port for the mock API server (0 = random available port)")
	replayCmd.Flags().String("kubeconfig-out", "", "where to write the generated kubeconfig (default: ~/.kube/k8shark-<id>.yaml)")
	replayCmd.Flags().String("speed", "1x", "playback speed factor, e.g. 2x, 3x, 0.5x")
	replayCmd.Flags().String("from", "", "replay window start: RFC3339 or relative duration like -10m (default: capture start)")
	replayCmd.Flags().String("to", "", "replay window end: RFC3339 or relative duration like -1m (default: capture end)")
	replayCmd.Flags().Bool("loop", false, "restart the replay from the window start when it reaches the end")
	replayCmd.Flags().Bool("start-paused", false, "start paused; press Enter to begin playback")
	replayCmd.Flags().Bool("ui", false, "also start the web dashboard as a replay transport (VCR)")
	replayCmd.Flags().String("ui-port", "0", "port for the dashboard when --ui is set (0 = random)")
	replayCmd.Flags().Bool("writable", false, "accept client writes into an in-memory overlay (closed-loop controller dev)")
	replayCmd.Flags().Bool("schedule-pods", true, "bind unscheduled pods to a node on create (the scheduler replay lacks); --writable only")
	replayCmd.Flags().Bool("with-kwok", false, "also run a detected 'kwok' binary against the server to drive pod/node lifecycle (implies --writable)")
}

func runReplay(cmd *cobra.Command, args []string) error {
	port, _ := cmd.Flags().GetString("port")
	kubeconfigOut, _ := cmd.Flags().GetString("kubeconfig-out")
	speed, _ := cmd.Flags().GetString("speed")
	from, _ := cmd.Flags().GetString("from")
	to, _ := cmd.Flags().GetString("to")
	loop, _ := cmd.Flags().GetBool("loop")
	startPaused, _ := cmd.Flags().GetBool("start-paused")
	writable, _ := cmd.Flags().GetBool("writable")
	schedulePods, _ := cmd.Flags().GetBool("schedule-pods")
	withKwok, _ := cmd.Flags().GetBool("with-kwok")
	verbose, _ := cmd.Root().PersistentFlags().GetBool("verbose")

	// --with-kwok drives pod/node lifecycle against the overlay, so it implies
	// --writable (and needs the scheduling shim on to bind pods to nodes).
	if withKwok {
		writable = true
	}

	srv, err := server.Replay(server.ReplayOptions{
		ArchivePath:       args[0],
		Port:              port,
		KubeconfigOut:     kubeconfigOut,
		Speed:             speed,
		From:              from,
		To:                to,
		Loop:              loop,
		StartPaused:       startPaused,
		Writable:          writable,
		DisableScheduling: !schedulePods,
		Verbose:           verbose,
	})
	if err != nil {
		return fmt.Errorf("starting replay: %w", err)
	}

	// Optionally launch a detected kwok against the server to drive pod/node
	// lifecycle. Started after the server is up (it needs the kubeconfig) and torn
	// down on shutdown.
	var kwokCleanup func()
	if withKwok {
		kwokCleanup, err = startKwok(srv.KubeconfigPath())
		if err != nil {
			srv.Shutdown()
			return err
		}
		defer kwokCleanup()
	}

	clock := srv.Clock()
	winStart, winEnd := clock.Window()

	// Optionally start the dashboard as a replay transport, sharing the clock.
	uiEnabled, _ := cmd.Flags().GetBool("ui")
	uiPort, _ := cmd.Flags().GetString("ui-port")
	var uiSrv *ui.Server
	if uiEnabled {
		uiSrv, err = ui.Open(ui.OpenOptions{ArchivePath: args[0], Port: uiPort, Verbose: verbose, Clock: clock})
		if err != nil {
			srv.Shutdown()
			return fmt.Errorf("starting dashboard: %w", err)
		}
	}

	fmt.Printf("k8shark replay server running\n")
	fmt.Printf("  Address:    %s\n", srv.Address())
	fmt.Printf("  Kubeconfig: %s\n", srv.KubeconfigPath())
	fmt.Printf("  Window:     %s → %s (%s)\n", winStart.Format(time.RFC3339), winEnd.Format(time.RFC3339), winEnd.Sub(winStart).Round(time.Second))
	fmt.Printf("  Speed:      %s\n", formatSpeed(clock.Speed()))
	if loop {
		fmt.Printf("  Loop:       on\n")
	}
	if srv.Writable() {
		fmt.Printf("  Writable:   on (client writes land in an in-memory overlay)\n")
	}
	if withKwok {
		fmt.Printf("  KWOK:       on (driving pod/node lifecycle via bundled stages)\n")
	}
	fmt.Printf("  Control:    %s/_k8shark/replay\n", srv.Address())
	if uiSrv != nil {
		fmt.Printf("  Dashboard:  %s\n", uiSrv.Address())
	}
	fmt.Printf("\nRun: export KUBECONFIG=%s\n", srv.KubeconfigPath())
	fmt.Printf("Then watch it change, e.g.: kubectl get pods -A --watch\n")
	fmt.Printf("Drive playback, e.g.: curl -sk -X POST %s/_k8shark/replay/pause\n", srv.Address())

	if !srv.HasWatchEvents() {
		fmt.Printf("\nNote: this capture has no watch events; changes will be inferred by diffing\n")
		fmt.Printf("      consecutive snapshots. Re-capture with 'watch: true' for precise,\n")
		fmt.Printf("      higher-resolution ADDED/MODIFIED/DELETED events.\n")
	}

	switch {
	case startPaused && uiEnabled:
		fmt.Printf("\nStarting paused — use the dashboard to begin playback (Ctrl+C to stop).\n")
	case startPaused:
		fmt.Printf("\nPaused. Press Enter to begin playback (Ctrl+C to stop).\n")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		clock.Resume()
	default:
		fmt.Printf("\nPress Ctrl+C to stop.\n")
	}

	// Live status line until shutdown.
	stop := make(chan struct{})
	go replayStatusLoop(srv, stop)
	err = srv.Wait()
	close(stop)
	if uiSrv != nil {
		uiSrv.Shutdown()
	}
	fmt.Println()
	return err
}

// replayStatusLoop repaints a single status line showing clock position, speed,
// and events emitted, until stop is closed.
func replayStatusLoop(srv *server.Server, stop <-chan struct{}) {
	clock := srv.Clock()
	winStart, winEnd := clock.Window()
	total := winEnd.Sub(winStart)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			pos, _, ended := clock.Sample()
			state := ""
			switch {
			case clock.Paused():
				state = " · paused"
			case ended:
				state = " · ended"
			}
			fmt.Printf("\r  replaying %s (+%s / %s) · %s · %d events emitted%s   ",
				pos.Format("15:04:05Z07:00"),
				pos.Sub(winStart).Round(time.Second),
				total.Round(time.Second),
				formatSpeed(clock.Speed()),
				clock.EventsEmitted(),
				state,
			)
		}
	}
}

// formatSpeed renders a speed factor as e.g. "2x" or "0.5x".
func formatSpeed(s float64) string {
	if s == float64(int64(s)) {
		return fmt.Sprintf("%dx", int64(s))
	}
	return fmt.Sprintf("%gx", s)
}
