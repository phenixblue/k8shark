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
	cmd.Flags().Bool("writable", false, "")
	cmd.Flags().Bool("with-kwok", false, "")
	cmd.Flags().Bool("with-controller-manager", false, "")
	return cmd
}

func TestResolveUIStartPaused(t *testing.T) {
	cases := []struct {
		name               string
		replayMode         bool
		startPaused        bool
		startPausedChanged bool
		want               bool
	}{
		{"no replay mode leaves paused false", false, false, false, false},
		{"replay mode defaults to paused", true, false, false, true},
		{"explicit --start-paused=false wins in replay mode", true, false, true, false},
		{"explicit --start-paused=true still paused", true, true, true, true},
		{"no replay mode with flag set still honors flag", false, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveUIStartPaused(c.replayMode, c.startPaused, c.startPausedChanged)
			if got != c.want {
				t.Errorf("resolveUIStartPaused(%v, %v, %v) = %v, want %v",
					c.replayMode, c.startPaused, c.startPausedChanged, got, c.want)
			}
		})
	}
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

// TestRunUI_WithKwokImpliesReplayMode verifies --with-kwok forces replay mode
// (and thus --writable) the same way --with-controller-manager and --writable
// do, so it also conflicts with --at.
func TestRunUI_WithKwokImpliesReplayMode(t *testing.T) {
	arch := buildDiffArchive(t, healthyPodList)
	cmd := newTestUICommand()
	_ = cmd.Flags().Set("at", "-5m")
	_ = cmd.Flags().Set("with-kwok", "true")

	err := runUI(cmd, []string{arch})
	if err == nil {
		t.Fatal("expected error: --with-kwok implies replay mode, which conflicts with --at")
	}
	if !strings.Contains(err.Error(), "--at cannot be combined") {
		t.Errorf("error = %v, want it to explain the --at/replay conflict", err)
	}
}

// TestRunUI_WithControllerManagerImpliesReplayMode is the symmetric case for
// --with-controller-manager: it implies replay mode (and thus --writable)
// exactly like --with-kwok does, so it must conflict with --at the same way.
func TestRunUI_WithControllerManagerImpliesReplayMode(t *testing.T) {
	arch := buildDiffArchive(t, healthyPodList)
	cmd := newTestUICommand()
	_ = cmd.Flags().Set("at", "-5m")
	_ = cmd.Flags().Set("with-controller-manager", "true")

	err := runUI(cmd, []string{arch})
	if err == nil {
		t.Fatal("expected error: --with-controller-manager implies replay mode, which conflicts with --at")
	}
	if !strings.Contains(err.Error(), "--at cannot be combined") {
		t.Errorf("error = %v, want it to explain the --at/replay conflict", err)
	}
}
