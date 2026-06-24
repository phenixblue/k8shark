package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newTestInspectCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().StringP("output", "o", "table", "")
	cmd.Flags().BoolP("wide", "w", false, "")
	return cmd
}

func TestRunInspect_JSON(t *testing.T) {
	arch := buildDiffArchive(t, `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"p","namespace":"default"}}]}`)

	cmd := newTestInspectCommand()
	if err := cmd.Flags().Set("output", "json"); err != nil {
		t.Fatalf("set output: %v", err)
	}
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	if err := runInspect(cmd, []string{arch}); err != nil {
		t.Fatalf("runInspect: %v", err)
	}

	// Output must be valid JSON carrying the capture metadata.
	var report map[string]any
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("inspect output is not valid JSON: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), "test-capture") {
		t.Errorf("expected capture id in report:\n%s", buf.String())
	}
}

func TestRunInspect_Table(t *testing.T) {
	arch := buildDiffArchive(t, `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"p","namespace":"default"}}]}`)

	cmd := newTestInspectCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	if err := runInspect(cmd, []string{arch}); err != nil {
		t.Fatalf("runInspect: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "pods") {
		t.Errorf("expected pods row in table output:\n%s", out)
	}
}

func TestRunInspect_MissingArchive(t *testing.T) {
	cmd := newTestInspectCommand()
	if err := runInspect(cmd, []string{"/no/such/archive.khsrk"}); err == nil {
		t.Error("expected error for missing archive")
	}
}
