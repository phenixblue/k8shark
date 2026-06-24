package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/phenixblue/k8shark/internal/inspect"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var inspectCmd = &cobra.Command{
	Use:   "inspect <capture.khsrk>",
	Short: "Display a summary of a capture archive's contents",
	Long: `Reads a k8shark capture archive and prints capture metadata and a
table of captured resource types without starting a server.`,
	Args: cobra.ExactArgs(1),
	RunE: runInspect,
}

func init() {
	rootCmd.AddCommand(inspectCmd)
	inspectCmd.Flags().StringP("output", "o", "table", "Output format: table, json, or yaml")
	inspectCmd.Flags().BoolP("wide", "w", false, "Show full namespace list in table output")
}

func runInspect(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")

	report, err := inspect.Run(args[0])
	if err != nil {
		return err
	}

	switch strings.ToLower(output) {
	case "json":
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	case "yaml":
		return yaml.NewEncoder(cmd.OutOrStdout()).Encode(report)
	default:
		wide, _ := cmd.Flags().GetBool("wide")
		printInspectTable(cmd, report, wide)
		return nil
	}
}

func printInspectTable(cmd *cobra.Command, r *inspect.Report, wide bool) {
	out := cmd.OutOrStdout()
	duration := r.CapturedUntil.Sub(r.CapturedAt).Truncate(time.Second)

	fmt.Fprintf(out, "Capture ID:   %s\n", r.CaptureID)
	fmt.Fprintf(out, "Captured:     %s → %s  (%s)\n",
		r.CapturedAt.UTC().Format(time.RFC3339),
		r.CapturedUntil.UTC().Format(time.RFC3339),
		duration)
	fmt.Fprintf(out, "Kubernetes:   %s\n", r.KubernetesVersion)
	fmt.Fprintf(out, "Server:       %s\n", r.ServerAddress)
	fmt.Fprintf(out, "Archive:      %s (%s)\n", r.ArchivePath, formatBytes(r.ArchiveSize))
	fmt.Fprintf(out, "Records:      %d\n\n", r.RecordCount)

	if len(r.Resources) == 0 {
		fmt.Fprintln(out, "No resources captured.")
		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if wide {
		fmt.Fprintln(tw, "RESOURCE\tGROUP\tVERSION\tITEMS\tNAMESPACES\tRECORDS")
	} else {
		fmt.Fprintln(tw, "RESOURCE\tGROUP\tVERSION\tITEMS\tNAMESPACES\tRECORDS")
	}
	for _, rs := range r.Resources {
		var nsCol string
		if !rs.Namespaced {
			nsCol = "-"
		} else if wide {
			nsCol = strings.Join(rs.Namespaces, ", ")
		} else {
			n := len(rs.Namespaces)
			switch n {
			case 0:
				nsCol = "0 namespaces"
			case 1:
				nsCol = rs.Namespaces[0]
			case 2:
				nsCol = strings.Join(rs.Namespaces, ", ")
			default:
				nsCol = fmt.Sprintf("%d namespaces", n)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d\n",
			rs.Resource, rs.Group, rs.Version, rs.Items, nsCol, rs.Records)
	}
	_ = tw.Flush()

	if !wide {
		fmt.Fprintf(out, "\nUse --wide / -w to show the full namespace list.\n")
	}
}
