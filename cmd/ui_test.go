package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newTestUICommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("port", "0", "")
	cmd.Flags().String("api-port", "0", "")
	cmd.Flags().String("kubeconfig-out", "", "")
	cmd.Flags().String("at", "", "")
	cmd.Flags().String("speed", "", "")
	cmd.Flags().String("from", "", "")
	cmd.Flags().String("to", "", "")
	cmd.Flags().Bool("loop", false, "")
	cmd.Flags().Bool("start-paused", false, "")
	return cmd
}

func TestRunUI_RejectsAtWithReplayFlags(t *testing.T) {
	arch := buildDiffArchive(t, healthyPodList)
	cmd := newTestUICommand()
	_ = cmd.Flags().Set("at", "-5m")
	_ = cmd.Flags().Set("speed", "2x")

	err := runUI(cmd, []string{arch})
	if err == nil {
		t.Fatal("expected error combining --at with replay flags")
	}
	if !strings.Contains(err.Error(), "--at cannot be combined") {
		t.Errorf("error = %v, want it to explain the --at/replay conflict", err)
	}
}
