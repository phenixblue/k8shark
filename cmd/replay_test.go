package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newTestReplayCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("port", "0", "")
	cmd.Flags().String("kubeconfig-out", "", "")
	cmd.Flags().String("speed", "1x", "")
	cmd.Flags().String("from", "", "")
	cmd.Flags().String("to", "", "")
	cmd.Flags().Bool("loop", false, "")
	cmd.Flags().Bool("start-paused", false, "")
	return cmd
}

func TestRunReplay_InvalidSpeed(t *testing.T) {
	arch := buildDiffArchive(t, healthyPodList)
	cmd := newTestReplayCommand()
	_ = cmd.Flags().Set("speed", "fast")

	err := runReplay(cmd, []string{arch})
	if err == nil {
		t.Fatal("expected error for invalid --speed")
	}
	if !strings.Contains(err.Error(), "speed") {
		t.Errorf("error = %v, want it to mention speed", err)
	}
}

func TestRunReplay_MissingArchive(t *testing.T) {
	cmd := newTestReplayCommand()
	if err := runReplay(cmd, []string{"/no/such/capture.kshrk"}); err == nil {
		t.Fatal("expected error for missing archive")
	}
}

func TestRunReplay_InvalidWindow(t *testing.T) {
	arch := buildDiffArchive(t, healthyPodList)
	cmd := newTestReplayCommand()
	// --to before --from is rejected.
	_ = cmd.Flags().Set("from", "-1m")
	_ = cmd.Flags().Set("to", "-5m")

	if err := runReplay(cmd, []string{arch}); err == nil {
		t.Fatal("expected error when --to precedes --from")
	}
}
