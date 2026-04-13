package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	archivepkg "github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/spf13/cobra"
)

func TestRunDiff_ExitCodeOnDiff(t *testing.T) {
	before := buildDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"before"}}]}`)
	after := buildDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"after"}}]}`)

	cmd := newTestDiffCommand()
	cmd.Flags().Set("before", before)
	cmd.Flags().Set("after", after)

	err := runDiff(cmd, nil)
	if err == nil {
		t.Fatal("expected diff exit error, got nil")
	}
	exitErr, ok := err.(exitError)
	if !ok {
		t.Fatalf("expected exitError, got %T", err)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit code 1, got %d", exitErr.ExitCode())
	}
}

func TestRunDiff_JSONOutputNoDiff(t *testing.T) {
	archivePath := buildDiffArchive(t, `{"kind":"PodList","items":[]}`)
	cmd := newTestDiffCommand()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.Flags().Set("before", archivePath)
	cmd.Flags().Set("after", archivePath)
	cmd.Flags().Set("output", "json")

	err := runDiff(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var result struct {
		Changes []any `json:"changes"`
	}
	if jerr := json.Unmarshal(buf.Bytes(), &result); jerr != nil {
		t.Fatalf("invalid json output: %v", jerr)
	}
	if len(result.Changes) != 0 {
		t.Fatalf("expected no changes, got %d", len(result.Changes))
	}
}

func newTestDiffCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("before", "", "")
	cmd.Flags().String("after", "", "")
	cmd.Flags().String("archive", "", "")
	cmd.Flags().String("before-at", "", "")
	cmd.Flags().String("after-at", "", "")
	cmd.Flags().String("resource", "", "")
	cmd.Flags().String("namespace", "", "")
	cmd.Flags().StringP("output", "o", "text", "")
	return cmd
}

func buildDiffArchive(t *testing.T, body string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "capture.tar.gz")
	now := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	rec := &capture.Record{ID: "rec-1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body)}
	meta := &capture.CaptureMetadata{CaptureID: "test-capture", CapturedAt: now, CapturedUntil: now, RecordCount: 1}
	index := capture.Index{
		"/api/v1/namespaces/default/pods": {APIPath: "/api/v1/namespaces/default/pods", RecordIDs: []string{"rec-1"}, Times: []time.Time{now}},
	}
	if err := archivepkg.Write(out, meta, []*capture.Record{rec}, index); err != nil {
		t.Fatalf("archive.Write: %v", err)
	}
	return out
}
