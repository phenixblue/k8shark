package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/query"
	"github.com/phenixblue/k8shark/internal/server"
	"github.com/spf13/cobra"
)

var queryCmd = &cobra.Command{
	Use:   "query <capture.kshrk> <jsonpath-expression>",
	Short: "Run a JSONPath query across every captured object",
	Long: `Evaluates a kubectl-style JSONPath expression against every object captured
in the archive — across all resource types and namespaces — at a chosen
snapshot, and prints the objects where it matched.

Limit the scope with --resource and --namespace, and pin the snapshot in time
with --at. Objects that don't have the queried field are skipped, not
reported as errors.`,
	Example: `  # Every container image in the capture
  kshrk query capture.kshrk '{.spec.containers[*].image}'

  # Just Deployments' replica counts, as JSON
  kshrk query capture.kshrk '{.spec.replicas}' --resource deployments -o json

  # Pod phases at a point in time
  kshrk query capture.kshrk '{.status.phase}' --resource pods --at -5m`,
	Args: cobra.ExactArgs(2),
	RunE: runQuery,
}

func init() {
	rootCmd.AddCommand(queryCmd)
	queryCmd.Flags().StringP("output", "o", "table", "output format: table or json")
	queryCmd.Flags().String("at", "", "query state at a timestamp (RFC3339 or relative duration like -5m); default latest")
	queryCmd.Flags().String("resource", "", "limit the query to one resource type, e.g. pods")
	queryCmd.Flags().String("namespace", "", "limit the query to one namespace")
	_ = queryCmd.RegisterFlagCompletionFunc("output",
		cobra.FixedCompletions([]string{"table", "json"}, cobra.ShellCompDirectiveNoFileComp))
}

func runQuery(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")
	atRaw, _ := cmd.Flags().GetString("at")
	resource, _ := cmd.Flags().GetString("resource")
	namespace, _ := cmd.Flags().GetString("namespace")

	ar, err := archive.Open(args[0])
	if err != nil {
		return fmt.Errorf("opening archive: %w", err)
	}
	defer ar.Close()
	store, err := server.LoadStore(ar)
	if err != nil {
		return fmt.Errorf("loading capture: %w", err)
	}

	at, err := parseAtFlag(atRaw, store.Metadata.CapturedAt, store.Metadata.CapturedUntil)
	if err != nil {
		return err
	}

	result, err := query.Run(store, query.Options{
		Expression: args[1],
		At:         at,
		Resource:   resource,
		Namespace:  namespace,
	})
	if err != nil {
		return err
	}

	switch strings.ToLower(output) {
	case "json":
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	default:
		printQueryTable(cmd, result)
	}
	return nil
}

func printQueryTable(cmd *cobra.Command, r *query.Result) {
	out := cmd.OutOrStdout()
	if len(r.Matches) == 0 {
		fmt.Fprintln(out, "No matches.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RESOURCE\tNAMESPACE\tNAME\tVALUE")
	for _, m := range r.Matches {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Resource, m.Namespace, m.Name, string(m.Value))
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "\n%d match(es)\n", len(r.Matches))
}
