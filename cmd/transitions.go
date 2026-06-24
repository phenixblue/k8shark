package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

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

Narrow the output with --resource / --namespace / --name and the --since/--until
time window, add --diff to show field-level changes for MODIFIED events, and use
-o json for machine-readable output.`,
	Example: `  # List all state changes in a capture
  kshrk transitions capture.kshrk

  # Only Deployment changes in the "prod" namespace
  kshrk transitions capture.kshrk --resource deployments --namespace prod

  # Show field diffs for MODIFIED events within a time window
  kshrk transitions capture.kshrk --diff --since 2026-04-09T10:00:00Z --until 2026-04-09T10:05:00Z

  # Machine-readable output
  kshrk transitions capture.kshrk -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runTransitions,
}

func init() {
	rootCmd.AddCommand(transitionsCmd)
	transitionsCmd.Flags().String("resource", "", "filter by resource name fragment (e.g. pods, deployments)")
	transitionsCmd.Flags().String("namespace", "", "filter by exact namespace")
	transitionsCmd.Flags().String("name", "", "filter by exact object name")
	transitionsCmd.Flags().String("since", "", "start of time window (RFC3339)")
	transitionsCmd.Flags().String("until", "", "end of time window (RFC3339)")
	transitionsCmd.Flags().Bool("diff", false, "show field diffs for MODIFIED events")
	transitionsCmd.Flags().StringP("output", "o", "table", "output format: table or json")
}

func runTransitions(cmd *cobra.Command, args []string) error {
	resource, _ := cmd.Flags().GetString("resource")
	namespace, _ := cmd.Flags().GetString("namespace")
	name, _ := cmd.Flags().GetString("name")
	sinceStr, _ := cmd.Flags().GetString("since")
	untilStr, _ := cmd.Flags().GetString("until")
	showDiff, _ := cmd.Flags().GetBool("diff")
	output, _ := cmd.Flags().GetString("output")

	opts := transitions.FilterOpts{
		Resource:  resource,
		Namespace: namespace,
		Name:      name,
	}
	if sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return fmt.Errorf("invalid --since %q: must be RFC3339", sinceStr)
		}
		opts.Since = t
	}
	if untilStr != "" {
		t, err := time.Parse(time.RFC3339, untilStr)
		if err != nil {
			return fmt.Errorf("invalid --until %q: must be RFC3339", untilStr)
		}
		opts.Until = t
	}

	ts, err := transitions.LoadTransitions(args[0], opts)
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
