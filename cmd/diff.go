package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	diffpkg "github.com/phenixblue/k8shark/internal/diff"
	"github.com/spf13/cobra"
)

// exitError signals that a command ran successfully but found something (a
// diagnose --fail-on gate trip, a diff with differences) — exit code 1 per
// the contract documented in root.go. It always carries that exit code (not
// an arbitrary caller-supplied one) so a future call site can't accidentally
// violate the 0/1/2 contract by passing an unrelated code.
type exitError struct {
	msg string
}

func (e exitError) Error() string { return e.msg }
func (e exitError) ExitCode() int { return exitCodeFindings }

var diffCmd = &cobra.Command{
	Use:   "diff",
	Short: "Compare two capture snapshots",
	Long: `Compares resource state between two capture archives (--before/--after),
or between two points in time within a single archive (--archive with
--from/--to), and prints a diff. Limit the scope with --resource and
--namespace, and choose text or json output with -o. Exits non-zero when
differences are found.`,
	Example: `  # Diff two separate captures
  kshrk diff --before before.kshrk --after after.kshrk

  # Diff two points in time within one capture
  kshrk diff --archive capture.kshrk --from -10m --to -1m

  # Limit to a resource and namespace, as JSON
  kshrk diff --before before.kshrk --after after.kshrk --resource pods --namespace default -o json`,
	Args: cobra.NoArgs,
	RunE: runDiff,
}

func init() {
	rootCmd.AddCommand(diffCmd)
	diffCmd.Flags().String("before", "", "before archive path")
	diffCmd.Flags().String("after", "", "after archive path")
	diffCmd.Flags().String("archive", "", "single archive path for intra-archive diff")
	diffCmd.Flags().String("from", "", "time for the before snapshot, with --archive (RFC3339 or relative duration like -5m)")
	diffCmd.Flags().String("to", "", "time for the after snapshot, with --archive (RFC3339 or relative duration like -1m)")
	diffCmd.Flags().String("resource", "", "limit diff to one resource type, e.g. pods")
	diffCmd.Flags().String("namespace", "", "limit diff to one namespace")
	diffCmd.Flags().StringP("output", "o", "text", "output format: text or json")
	for _, f := range []string{"before", "after", "archive"} {
		_ = diffCmd.MarkFlagFilename(f, captureExt)
	}
	_ = diffCmd.RegisterFlagCompletionFunc("output",
		cobra.FixedCompletions([]string{"text", "json"}, cobra.ShellCompDirectiveNoFileComp))
}

func runDiff(cmd *cobra.Command, _ []string) error {
	beforeArchive, _ := cmd.Flags().GetString("before")
	afterArchive, _ := cmd.Flags().GetString("after")
	archivePath, _ := cmd.Flags().GetString("archive")
	from, _ := cmd.Flags().GetString("from")
	to, _ := cmd.Flags().GetString("to")
	resource, _ := cmd.Flags().GetString("resource")
	namespace, _ := cmd.Flags().GetString("namespace")
	output, _ := cmd.Flags().GetString("output")

	// A single key source is shared across both archives (documented v1.0
	// limitation). Pass every candidate path so an encrypted archive on either
	// side triggers the prompt, even if the other side is plaintext.
	identities, err := resolveDecryptIdentities(cmd, archivePath, beforeArchive, afterArchive)
	if err != nil {
		return err
	}

	result, err := diffpkg.Run(diffpkg.Options{
		BeforeArchive: beforeArchive,
		AfterArchive:  afterArchive,
		Archive:       archivePath,
		From:          from,
		To:            to,
		Resource:      resource,
		Namespace:     namespace,
		Identities:    identities,
	})
	if err != nil {
		return err
	}

	hasDiff := len(result.Changes) > 0
	switch strings.ToLower(output) {
	case "json":
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return err
		}
	default:
		color := isColorTerminal(cmd.OutOrStdout())
		text, err := diffpkg.RenderText(result, color)
		if err != nil {
			return err
		}
		if text != "" {
			_, _ = fmt.Fprint(cmd.OutOrStdout(), text)
		}
	}

	if hasDiff {
		return exitError{}
	}
	return nil
}

func isColorTerminal(f any) bool {
	out, ok := f.(*os.File)
	if !ok {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := out.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}
