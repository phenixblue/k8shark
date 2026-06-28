package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/diagnose"
	"github.com/spf13/cobra"
)

func newTestDiagnoseCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().StringP("output", "o", "table", "")
	cmd.Flags().String("at", "", "")
	cmd.Flags().String("severity", "info", "")
	cmd.Flags().String("category", "", "")
	cmd.Flags().String("fail-on", "", "")
	return cmd
}

const crashloopPodList = `{"apiVersion":"v1","kind":"PodList","items":[` +
	`{"metadata":{"name":"c","namespace":"default"},"status":{"phase":"Running","containerStatuses":[` +
	`{"name":"app","ready":false,"restartCount":4,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}]}`

const healthyPodList = `{"apiVersion":"v1","kind":"PodList","items":[` +
	`{"metadata":{"name":"h","namespace":"default"},"status":{"phase":"Running","containerStatuses":[` +
	`{"name":"app","ready":true,"restartCount":0,"state":{"running":{}}}]}}]}`

func TestRunDiagnose_JSON(t *testing.T) {
	arch := buildDiffArchive(t, crashloopPodList) // writes /api/v1/namespaces/default/pods
	cmd := newTestDiagnoseCommand()
	_ = cmd.Flags().Set("output", "json")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	if err := runDiagnose(cmd, []string{arch}); err != nil {
		t.Fatalf("runDiagnose: %v", err)
	}
	var rep diagnose.Report
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if rep.SchemaVersion != diagnose.SchemaVersion {
		t.Errorf("schema_version = %d", rep.SchemaVersion)
	}
	if !strings.Contains(buf.String(), "pod.crashloopbackoff") {
		t.Errorf("expected crashloop finding:\n%s", buf.String())
	}
	if rep.Summary.Critical < 1 {
		t.Errorf("expected a critical finding, got summary %+v", rep.Summary)
	}
}

func TestRunDiagnose_FailOn(t *testing.T) {
	arch := buildDiffArchive(t, crashloopPodList)
	cmd := newTestDiagnoseCommand()
	_ = cmd.Flags().Set("output", "json")
	_ = cmd.Flags().Set("fail-on", "critical")
	cmd.SetOut(&bytes.Buffer{})

	err := runDiagnose(cmd, []string{arch})
	ee, ok := err.(exitError)
	if !ok || ee.ExitCode() != 1 {
		t.Fatalf("expected exitError code 1 for critical findings, got %v (%T)", err, err)
	}
}

func TestRunDiagnose_FailOn_Clean(t *testing.T) {
	arch := buildDiffArchive(t, healthyPodList)
	cmd := newTestDiagnoseCommand()
	_ = cmd.Flags().Set("fail-on", "critical")
	cmd.SetOut(&bytes.Buffer{})

	if err := runDiagnose(cmd, []string{arch}); err != nil {
		t.Errorf("clean capture should not fail, got %v", err)
	}
}

func TestRunDiagnose_BadSeverity(t *testing.T) {
	arch := buildDiffArchive(t, healthyPodList)
	cmd := newTestDiagnoseCommand()
	_ = cmd.Flags().Set("severity", "bogus")
	cmd.SetOut(&bytes.Buffer{})
	if err := runDiagnose(cmd, []string{arch}); err == nil {
		t.Error("expected error for invalid --severity")
	}
}

func TestParseDiagnoseAt_Window(t *testing.T) {
	start := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 10, 10, 10, 0, 0, time.UTC)
	if _, err := parseDiagnoseAt("2026-04-10T10:05:00Z", start, end); err != nil {
		t.Errorf("in-window time rejected: %v", err)
	}
	if _, err := parseDiagnoseAt("-1h", start, end); err == nil {
		t.Error("expected out-of-window (before start) to error")
	}
	if _, err := parseDiagnoseAt("2026-04-10T11:00:00Z", start, end); err == nil {
		t.Error("expected out-of-window (after end) to error")
	}
}
