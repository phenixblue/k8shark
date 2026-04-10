package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/phenixblue/k8shark/internal/redact"
	"github.com/spf13/cobra"
)

var redactCmd = &cobra.Command{
	Use:   "redact --in <capture.tar.gz> [--out <redacted.tar.gz>]",
	Short: "Redact Secret data from a capture archive",
	Long: `Produces a new capture archive with all Kubernetes Secret data and
stringData values replaced by "REDACTED", safe for sharing with support teams.
The original archive is not modified.`,
	RunE: runRedact,
}

func init() {
	rootCmd.AddCommand(redactCmd)
	redactCmd.Flags().String("in", "", "source capture archive (required)")
	redactCmd.Flags().String("out", "", "output archive path (default: <in>-redacted.tar.gz)")
	redactCmd.Flags().StringArray("allow-secret", nil, "namespace/name of secret to preserve (repeatable)")
	_ = redactCmd.MarkFlagRequired("in")
}

func runRedact(cmd *cobra.Command, _ []string) error {
	in, _ := cmd.Flags().GetString("in")
	out, _ := cmd.Flags().GetString("out")
	allows, _ := cmd.Flags().GetStringArray("allow-secret")

	if out == "" {
		base := strings.TrimSuffix(in, ".tar.gz")
		out = base + "-redacted.tar.gz"
	}

	// Refuse to overwrite the source.
	inAbs, _ := filepath.Abs(in)
	outAbs, _ := filepath.Abs(out)
	if inAbs == outAbs {
		return fmt.Errorf("--out must differ from --in")
	}

	allowList := make(map[string]bool, len(allows))
	for _, a := range allows {
		allowList[a] = true
	}

	n, err := redact.Archive(in, out, allowList)
	if err != nil {
		return err
	}

	fi, _ := os.Stat(out)
	size := int64(0)
	if fi != nil {
		size = fi.Size()
	}

	fmt.Printf("Redacted %d secret(s) → %s (%d bytes)\n", n, out, size)
	return nil
}
