package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/timewindow"
	"github.com/phenixblue/k8shark/internal/transitions"
	"github.com/spf13/cobra"
)

var transitionsCmd = &cobra.Command{
	Use:   "transitions <capture.kshrk>",
	Short: "List resource state changes from a capture archive",
	Long: `Reads a k8shark capture archive and reports ADDED, MODIFIED, and DELETED
events for captured resources, without starting a replay server.

For watch-enabled captures, events are read directly from the watch-event index.
For poll-only captures, consecutive snapshots are diff'd to infer changes.

Narrow the output with --resource / --namespace / --name and the --from/--to
time window, add --diff to show field-level changes for MODIFIED events, and use
-o json for machine-readable output.`,
	Example: `  # List all state changes in a capture
  kshrk transitions capture.kshrk

  # Only Deployment changes in the "prod" namespace
  kshrk transitions capture.kshrk --resource deployments --namespace prod

  # Show field diffs for MODIFIED events within a time window
  kshrk transitions capture.kshrk --diff --from 2026-04-09T10:00:00Z --to 2026-04-09T10:05:00Z

  # Machine-readable output
  kshrk transitions capture.kshrk -o json`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: completeArchiveArg,
	RunE:              runTransitions,
}

func init() {
	rootCmd.AddCommand(transitionsCmd)
	transitionsCmd.Flags().String("resource", "", "filter by resource name fragment (e.g. pods, deployments)")
	transitionsCmd.Flags().String("namespace", "", "filter by exact namespace")
	transitionsCmd.Flags().String("name", "", "filter by exact object name")
	transitionsCmd.Flags().String("from", "", "start of time window (RFC3339 or relative duration like -10m)")
	transitionsCmd.Flags().String("to", "", "end of time window (RFC3339 or relative duration like -1m)")
	transitionsCmd.Flags().Bool("diff", false, "show field diffs for MODIFIED events")
	transitionsCmd.Flags().StringP("output", "o", "table", "output format: table or json")
	_ = transitionsCmd.RegisterFlagCompletionFunc("output",
		cobra.FixedCompletions([]string{"table", "json"}, cobra.ShellCompDirectiveNoFileComp))
}

func runTransitions(cmd *cobra.Command, args []string) error {
	resource, _ := cmd.Flags().GetString("resource")
	namespace, _ := cmd.Flags().GetString("namespace")
	name, _ := cmd.Flags().GetString("name")
	fromRaw, _ := cmd.Flags().GetString("from")
	toRaw, _ := cmd.Flags().GetString("to")
	showDiff, _ := cmd.Flags().GetBool("diff")
	output, _ := cmd.Flags().GetString("output")

	identities, err := resolveDecryptIdentities(cmd, args[0])
	if err != nil {
		return err
	}

	// A lightweight metadata-only open, so relative --from/--to durations can
	// be anchored to the capture end and validated against its bounds, the
	// same as every other time flag (#221). LoadTransitions below reopens the
	// archive for the actual scan.
	ar, err := archive.OpenWithIdentities(args[0], identities)
	if err != nil {
		return fmt.Errorf("opening archive: %w", err)
	}
	meta, err := ar.ReadMetadata()
	_ = ar.Close()
	if err != nil {
		return fmt.Errorf("reading metadata: %w", err)
	}

	opts := transitions.FilterOpts{
		Resource:  resource,
		Namespace: namespace,
		Name:      name,
	}
	if opts.Since, err = timewindow.ParseAt(fromRaw, meta.CapturedAt, meta.CapturedUntil, "--from"); err != nil {
		return err
	}
	if opts.Until, err = timewindow.ParseAt(toRaw, meta.CapturedAt, meta.CapturedUntil, "--to"); err != nil {
		return err
	}

	ts, err := transitions.LoadTransitions(args[0], opts, identities)
	if err != nil {
		return err
	}

	switch strings.ToLower(output) {
	case "json":
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(ts)
	default:
		return printTransitionTable(cmd, ts, showDiff)
	}
}

func printTransitionTable(cmd *cobra.Command, ts []transitions.Transition, showDiff bool) error {
	out := cmd.OutOrStdout()

	if len(ts) == 0 {
		fmt.Fprintln(out, "No transitions found.")
		return nil
	}

	if !showDiff {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "TIME\tEVENT\tRESOURCE\tNAMESPACE\tNAME")
		for _, t := range ts {
			ns := t.Namespace
			if ns == "" {
				ns = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				t.Time.UTC().Format(time.RFC3339),
				t.EventType,
				t.Resource,
				ns,
				t.Name,
			)
		}
		return tw.Flush()
	}

	// Diff mode: one block per transition, with field diff for MODIFIED.
	for _, t := range ts {
		ns := t.Namespace
		if ns == "" {
			ns = "-"
		}
		header := fmt.Sprintf("%s  %-8s  %s/%s/%s",
			t.Time.UTC().Format(time.RFC3339),
			t.EventType,
			t.Resource, ns, t.Name,
		)
		fmt.Fprintln(out, header)

		if t.EventType == "MODIFIED" {
			d, err := transitions.DiffJSON(t.Before, t.After)
			if err == nil && d != "" {
				fmt.Fprint(out, transitions.ColorizeDiff(d))
			}
		}
	}
	return nil
}
