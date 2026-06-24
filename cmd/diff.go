package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	diffpkg "github.com/phenixblue/k8shark/internal/diff"
	"github.com/spf13/cobra"
)

type exitError struct {
	msg  string
	code int
}

func (e exitError) Error() string { return e.msg }
func (e exitError) ExitCode() int { return e.code }

var diffCmd = &cobra.Command{
	Use:   "diff",
	Short: "Compare two capture snapshots",
	Long: `Compares resource state between two capture archives (--before/--after),
or between two points in time within a single archive (--archive with
--before-at/--after-at), and prints a diff. Limit the scope with --resource and
--namespace, and choose text or json output with -o. Exits non-zero when
differences are found.`,
	Example: `  # Diff two separate captures
  kshrk diff --before before.kshrk --after after.kshrk

  # Diff two points in time within one capture
  kshrk diff --archive capture.kshrk --before-at -10m --after-at -1m

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
	diffCmd.Flags().String("before-at", "", "time for the before snapshot (RFC3339 or relative duration like -5m)")
	diffCmd.Flags().String("after-at", "", "time for the after snapshot (RFC3339 or relative duration like -1m)")
	diffCmd.Flags().String("resource", "", "limit diff to one resource type, e.g. pods")
	diffCmd.Flags().String("namespace", "", "limit diff to one namespace")
	diffCmd.Flags().StringP("output", "o", "text", "output format: text or json")
}

func runDiff(cmd *cobra.Command, _ []string) error {
	beforeArchive, _ := cmd.Flags().GetString("before")
	afterArchive, _ := cmd.Flags().GetString("after")
	archivePath, _ := cmd.Flags().GetString("archive")
	beforeAt, _ := cmd.Flags().GetString("before-at")
	afterAt, _ := cmd.Flags().GetString("after-at")
	resource, _ := cmd.Flags().GetString("resource")
	namespace, _ := cmd.Flags().GetString("namespace")
	output, _ := cmd.Flags().GetString("output")

	result, err := diffpkg.Run(diffpkg.Options{
		BeforeArchive: beforeArchive,
		AfterArchive:  afterArchive,
		Archive:       archivePath,
		BeforeAt:      beforeAt,
		AfterAt:       afterAt,
		Resource:      resource,
		Namespace:     namespace,
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
		return exitError{code: 1}
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
