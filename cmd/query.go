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
	Use:   "query <capture.kshrk> <expression>",
	Short: "Search or run a JSONPath query across every captured object",
	Long: `Evaluates an expression against every object captured in the archive — across
all resource types and namespaces — at a chosen snapshot, and prints what
matched.

By default the expression is a kubectl-style JSONPath template. With --text
or --regex, it's instead a plain substring or regular expression searched
across every object body and captured pod log (current and --previous).

Limit the scope with --resource and --namespace, and pin the snapshot in time
with --at.`,
	Example: `  # Every container image in the capture (JSONPath)
  kshrk query capture.kshrk '{.spec.containers[*].image}'

  # Just Deployments' replica counts, as JSON
  kshrk query capture.kshrk '{.spec.replicas}' --resource deployments -o json

  # Where does this error string appear, across objects and pod logs?
  kshrk query capture.kshrk 'connection refused' --text

  # Same, with a regular expression
  kshrk query capture.kshrk 'connection (refused|reset)' --regex`,
	Args: cobra.ExactArgs(2),
	RunE: runQuery,
}

func init() {
	rootCmd.AddCommand(queryCmd)
	queryCmd.Flags().StringP("output", "o", "table", "output format: table or json")
	queryCmd.Flags().String("at", "", "query state at a timestamp (RFC3339 or relative duration like -5m); default latest")
	queryCmd.Flags().String("resource", "", "limit the query to one resource type, e.g. pods")
	queryCmd.Flags().String("namespace", "", "limit the query to one namespace")
	queryCmd.Flags().Bool("text", false, "treat the expression as a plain substring, searched across object bodies and pod logs instead of JSONPath")
	queryCmd.Flags().Bool("regex", false, "treat the expression as a regular expression, searched across object bodies and pod logs instead of JSONPath")
	_ = queryCmd.RegisterFlagCompletionFunc("output",
		cobra.FixedCompletions([]string{"table", "json"}, cobra.ShellCompDirectiveNoFileComp))
}

func runQuery(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")
	atRaw, _ := cmd.Flags().GetString("at")
	resource, _ := cmd.Flags().GetString("resource")
	namespace, _ := cmd.Flags().GetString("namespace")
	text, _ := cmd.Flags().GetBool("text")
	useRegex, _ := cmd.Flags().GetBool("regex")
	if text && useRegex {
		return fmt.Errorf("--text and --regex are mutually exclusive")
	}

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

	if text || useRegex {
		result, err := query.SearchText(store, query.TextOptions{
			Pattern:   args[1],
			Regex:     useRegex,
			At:        at,
			Resource:  resource,
			Namespace: namespace,
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
			printTextTable(cmd, result)
		}
		return nil
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

func printTextTable(cmd *cobra.Command, r *query.TextResult) {
	out := cmd.OutOrStdout()
	if len(r.Matches) == 0 {
		fmt.Fprintln(out, "No matches.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RESOURCE\tNAMESPACE\tNAME\tLOCATION\tSNIPPET")
	for _, m := range r.Matches {
		loc := m.Field
		if m.Log {
			loc = "log"
			if m.Container != "" {
				loc += ":" + m.Container
			}
			if m.Previous {
				loc += " (previous)"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.Resource, m.Namespace, m.Name, tableSafe(loc), tableSafe(m.Snippet))
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "\n%d match(es)\n", len(r.Matches))
}

// tableSafe replaces tab and newline characters so a matched value (e.g. a
// multi-line ConfigMap entry or log line) can't split a tabwriter row/column
// when printed as a table cell.
func tableSafe(s string) string {
	s = strings.ReplaceAll(s, "\r\n", `\n`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}
