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
	Use:   "inspect <capture.tar.gz>",
	Short: "Display a summary of a capture archive's contents",
	Long: `Reads a k8shark capture archive and prints capture metadata and a
table of captured resource types without starting a server.`,
	Args: cobra.ExactArgs(1),
	RunE: runInspect,
}

func init() {
	rootCmd.AddCommand(inspectCmd)
	inspectCmd.Flags().StringP("output", "o", "table", "Output format: table, json, or yaml")
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
		printInspectTable(cmd, report)
		return nil
	}
}

func printInspectTable(cmd *cobra.Command, r *inspect.Report) {
	out := cmd.OutOrStdout()
	duration := r.CapturedUntil.Sub(r.CapturedAt).Truncate(time.Second)

	fmt.Fprintf(out, "Capture ID:   %s\n", r.CaptureID)
	fmt.Fprintf(out, "Captured:     %s → %s  (%s)\n",
		r.CapturedAt.UTC().Format(time.RFC3339),
		r.CapturedUntil.UTC().Format(time.RFC3339),
		duration)
	fmt.Fprintf(out, "Kubernetes:   %s\n", r.KubernetesVersion)
	fmt.Fprintf(out, "Server:       %s\n", r.ServerAddress)
	fmt.Fprintf(out, "Archive:      %s (%d bytes)\n", r.ArchivePath, r.ArchiveSize)
	fmt.Fprintf(out, "Records:      %d\n\n", r.RecordCount)

	if len(r.Resources) == 0 {
		fmt.Fprintln(out, "No resources captured.")
		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RESOURCE\tGROUP\tVERSION\tNAMESPACED\tNAMESPACES\tRECORDS")
	for _, rs := range r.Resources {
		ns := strings.Join(rs.Namespaces, ",")
		if !rs.Namespaced {
			ns = "-"
		}
		namespaced := "no"
		if rs.Namespaced {
			namespaced = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n",
			rs.Resource, rs.Group, rs.Version, namespaced, ns, rs.Records)
	}
	_ = tw.Flush()
}
