package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func newTestOpenCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.PersistentFlags().BoolP("verbose", "v", false, "")
	cmd.Flags().String("port", "0", "")
	cmd.Flags().String("kubeconfig-out", "", "")
	cmd.Flags().String("at", "", "")
	return cmd
}

func TestRunOpen_MissingArchive(t *testing.T) {
	cmd := newTestOpenCommand()
	// A nonexistent archive must fail before the server starts (which would
	// otherwise block on Wait()).
	if err := runOpen(cmd, []string{"/no/such/archive.kshrk"}); err == nil {
		t.Error("expected error for missing archive")
	}
}

func TestRunOpen_BadAtFlag(t *testing.T) {
	arch := buildDiffArchive(t, `{"apiVersion":"v1","kind":"PodList","items":[]}`)
	cmd := newTestOpenCommand()
	if err := cmd.Flags().Set("at", "not-a-timestamp"); err != nil {
		t.Fatalf("set at: %v", err)
	}
	if err := runOpen(cmd, []string{arch}); err == nil {
		t.Error("expected error for invalid --at value")
	}
}
