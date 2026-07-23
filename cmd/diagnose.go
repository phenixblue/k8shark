package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/diagnose"
	"github.com/phenixblue/k8shark/internal/server"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var diagnoseCmd = &cobra.Command{
	Use:   "diagnose <capture.kshrk>",
	Short: "Analyze a capture and report likely problems",
	Long: `Runs a diagnostic pass over a capture and prints severity-ranked findings
(CrashLoopBackOff, OOMKilled, image-pull failures, unschedulable pods, unbound
PVCs, version skew, …) — without starting a server.

Use -o json/yaml for automation, and --fail-on to exit non-zero for CI gating.`,
	Example: `  # Ranked findings as a table
  kshrk diagnose capture.kshrk

  # Only warnings and above, scheduling category, as JSON
  kshrk diagnose capture.kshrk --severity warning --category scheduling -o json

  # Fail the build if anything critical is found
  kshrk diagnose capture.kshrk --fail-on critical`,
	Args: cobra.ExactArgs(1),
	RunE: runDiagnose,
}

func init() {
	rootCmd.AddCommand(diagnoseCmd)
	diagnoseCmd.Flags().StringP("output", "o", "table", "output format: table, json, or yaml")
	diagnoseCmd.Flags().String("at", "", "analyze state at a timestamp (RFC3339 or relative duration like -5m); default latest")
	diagnoseCmd.Flags().String("severity", "info", "minimum severity to report: info, warning, or critical")
	diagnoseCmd.Flags().String("category", "", "only report this category (workload, scheduling, storage, node, cluster)")
	diagnoseCmd.Flags().String("fail-on", "", "exit non-zero if any finding is at least this severity (info, warning, critical)")
}

func runDiagnose(cmd *cobra.Command, args []string) error {
	output, _ := cmd.Flags().GetString("output")
	atRaw, _ := cmd.Flags().GetString("at")
	severity, _ := cmd.Flags().GetString("severity")
	category, _ := cmd.Flags().GetString("category")
	failOn, _ := cmd.Flags().GetString("fail-on")

	for name, v := range map[string]string{"severity": severity, "fail-on": failOn} {
		if v != "" && v != diagnose.SeverityInfo && v != diagnose.SeverityWarning && v != diagnose.SeverityCritical {
			return fmt.Errorf("--%s must be one of info, warning, critical (got %q)", name, v)
		}
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

	report := diagnose.Run(store, diagnose.Options{At: at, MinSeverity: severity, Category: category})

	switch strings.ToLower(output) {
	case "json":
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	case "yaml":
		if err := yaml.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
			return err
		}
	default:
		printDiagnoseTable(cmd, report)
	}

	if failOn != "" {
		for _, f := range report.Findings {
			if diagnose.SeverityAtLeast(f.Severity, failOn) {
				return exitError{msg: "", code: 1}
			}
		}
	}
	return nil
}

func printDiagnoseTable(cmd *cobra.Command, r diagnose.Report) {
	out := cmd.OutOrStdout()
	if len(r.Findings) == 0 {
		fmt.Fprintln(out, "No findings. ✓")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SEVERITY\tCATEGORY\tOBJECT\tFINDING")
	for _, f := range r.Findings {
		obj := f.Object.Name
		if f.Object.Namespace != "" {
			obj = f.Object.Namespace + "/" + f.Object.Name
		}
		if f.Count > 1 {
			obj = fmt.Sprintf("%s (+%d)", obj, f.Count-1)
		}
		finding := f.Title
		if f.Evidence != "" {
			finding = f.Title + " — " + f.Evidence
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", strings.ToUpper(f.Severity), f.Category, obj, finding)
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "\n%d finding(s): %d critical, %d warning, %d info\n",
		len(r.Findings), r.Summary.Critical, r.Summary.Warning, r.Summary.Info)
}

// parseAtFlag resolves an --at flag: empty = latest; RFC3339 timestamp; or a
// relative duration like -5m against the capture end.
func parseAtFlag(raw string, start, end time.Time) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	var at time.Time
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		at = t
	} else if d, derr := time.ParseDuration(raw); derr == nil {
		at = end.Add(d)
	} else {
		return time.Time{}, fmt.Errorf("parsing --at %q: must be RFC3339 or a relative duration like -5m", raw)
	}
	// Reject times outside the capture window — otherwise reconstruction returns
	// 404s and diagnose would misleadingly report "No findings".
	if (!start.IsZero() && at.Before(start)) || (!end.IsZero() && at.After(end)) {
		return time.Time{}, fmt.Errorf("--at %q resolves to %s, outside the capture window %s..%s",
			raw, at.UTC().Format(time.RFC3339), start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	}
	return at, nil
}
