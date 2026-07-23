package cmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	archivepkg "github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/query"
	"github.com/spf13/cobra"
)

// buildMultiPathArchive writes an archive with one record per (path, body)
// pair, for tests that need more than buildDiffArchive's single fixed path.
func buildMultiPathArchive(t *testing.T, bodies map[string]string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "capture.kshrk")
	now := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	sw, err := archivepkg.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	index := capture.Index{}
	i := 0
	for path, body := range bodies {
		rec := &capture.Record{ID: path, CapturedAt: now, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body)}
		if err := sw.WriteRecord(rec); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
		index[path] = &capture.IndexEntry{APIPath: path, Seqs: []int{0}, Times: []time.Time{now}}
		i++
	}
	meta := &capture.CaptureMetadata{CaptureID: "test-capture", CapturedAt: now, CapturedUntil: now, RecordCount: i}
	if err := sw.Finish(meta, index, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return out
}

func mustJSONQueryString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

func newTestQueryCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().StringP("output", "o", "table", "")
	cmd.Flags().String("at", "", "")
	cmd.Flags().String("resource", "", "")
	cmd.Flags().String("namespace", "", "")
	cmd.Flags().Bool("text", false, "")
	cmd.Flags().Bool("regex", false, "")
	return cmd
}

const queryPodList = `{"apiVersion":"v1","kind":"PodList","items":[` +
	`{"metadata":{"name":"web-1","namespace":"default"},"spec":{"containers":[{"name":"app","image":"nginx:alpine"}]}},` +
	`{"metadata":{"name":"web-2","namespace":"default"},"spec":{"containers":[{"name":"app","image":"nginx:alpine"}]}}]}`

func TestRunQuery_JSON(t *testing.T) {
	arch := buildDiffArchive(t, queryPodList) // writes /api/v1/namespaces/default/pods
	cmd := newTestQueryCommand()
	_ = cmd.Flags().Set("output", "json")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	if err := runQuery(cmd, []string{arch, "{.spec.containers[*].image}"}); err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	var result query.Result
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if len(result.Matches) != 2 {
		t.Fatalf("expected 2 matches, got %d: %+v", len(result.Matches), result.Matches)
	}
	for _, m := range result.Matches {
		if string(m.Value) != `"nginx:alpine"` {
			t.Errorf("unexpected value %s", m.Value)
		}
	}
}

func TestRunQuery_TableOutput(t *testing.T) {
	arch := buildDiffArchive(t, queryPodList)
	cmd := newTestQueryCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	if err := runQuery(cmd, []string{arch, "{.metadata.name}"}); err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	if !strings.Contains(buf.String(), "web-1") || !strings.Contains(buf.String(), "web-2") {
		t.Errorf("expected table to list both pods:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "2 match(es)") {
		t.Errorf("expected match count summary:\n%s", buf.String())
	}
}

func TestRunQuery_ResourceFilter(t *testing.T) {
	arch := buildDiffArchive(t, queryPodList)
	cmd := newTestQueryCommand()
	_ = cmd.Flags().Set("output", "json")
	_ = cmd.Flags().Set("resource", "deployments")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	if err := runQuery(cmd, []string{arch, "{.metadata.name}"}); err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	var result query.Result
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if len(result.Matches) != 0 {
		t.Fatalf("expected no matches when filtering to a resource type not in this capture, got %d", len(result.Matches))
	}
}

func TestRunQuery_BadExpression(t *testing.T) {
	arch := buildDiffArchive(t, queryPodList)
	cmd := newTestQueryCommand()
	cmd.SetOut(&bytes.Buffer{})
	if err := runQuery(cmd, []string{arch, "{.spec.["}); err == nil {
		t.Error("expected error for invalid jsonpath expression")
	}
}

func TestRunQuery_NoMatches(t *testing.T) {
	arch := buildDiffArchive(t, queryPodList)
	cmd := newTestQueryCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	if err := runQuery(cmd, []string{arch, "{.spec.replicas}"}); err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	if !strings.Contains(buf.String(), "No matches.") {
		t.Errorf("expected 'No matches.' message:\n%s", buf.String())
	}
}

func TestRunQuery_TextMode_TableOutputEscapesNewlines(t *testing.T) {
	// A matched value containing a raw newline or tab must not split the
	// tabwriter row/column when rendered as a table.
	podWithMultilineAnnotation := `{"apiVersion":"v1","kind":"PodList","items":[` +
		`{"metadata":{"name":"web-1","namespace":"default","annotations":{"note":"line one\nline two\tindented"}}}]}`
	arch := buildDiffArchive(t, podWithMultilineAnnotation)
	cmd := newTestQueryCommand()
	_ = cmd.Flags().Set("text", "true")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	if err := runQuery(cmd, []string{arch, "line one"}); err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "line one\nline two") {
		t.Errorf("expected the embedded newline to be escaped, not printed raw:\n%s", out)
	}
	if !strings.Contains(out, `line one\nline two\tindented`) {
		t.Errorf("expected escaped snippet in output:\n%s", out)
	}
	if strings.Count(out, "web-1") != 1 {
		t.Errorf("expected exactly one match row, got:\n%s", out)
	}
}

func TestRunQuery_TextMode(t *testing.T) {
	arch := buildDiffArchive(t, queryPodList)
	cmd := newTestQueryCommand()
	_ = cmd.Flags().Set("output", "json")
	_ = cmd.Flags().Set("text", "true")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	if err := runQuery(cmd, []string{arch, "nginx:alpine"}); err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	var result query.TextResult
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, buf.String())
	}
	if len(result.Matches) != 2 {
		t.Fatalf("expected 2 matches, got %d: %+v", len(result.Matches), result.Matches)
	}
	for _, m := range result.Matches {
		if m.Field != "spec.containers[0].image" {
			t.Errorf("unexpected field %q", m.Field)
		}
	}
}

func TestRunQuery_RegexMode_TableOutput(t *testing.T) {
	arch := buildDiffArchive(t, queryPodList)
	cmd := newTestQueryCommand()
	_ = cmd.Flags().Set("regex", "true")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	if err := runQuery(cmd, []string{arch, `nginx:\w+`}); err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	if !strings.Contains(buf.String(), "web-1") || !strings.Contains(buf.String(), "2 match(es)") {
		t.Errorf("expected table output with both matches:\n%s", buf.String())
	}
}

func TestRunQuery_TextMode_TableShowsGenericLogLocationWithoutContainer(t *testing.T) {
	// A legacy log record with no ?container= param must still render as a
	// log row (LOCATION "log"), not a blank LOCATION.
	arch := buildMultiPathArchive(t, map[string]string{
		"/api/v1/namespaces/crash-demo/pods/flaky-1/log": mustJSONQueryString(t, "fatal: connection refused"),
	})
	cmd := newTestQueryCommand()
	_ = cmd.Flags().Set("text", "true")
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	if err := runQuery(cmd, []string{arch, "connection refused"}); err != nil {
		t.Fatalf("runQuery: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "  log  ") {
		t.Errorf("expected a generic 'log' LOCATION for a container-less log match:\n%s", out)
	}
	if strings.Contains(out, "log:") {
		t.Errorf("did not expect a container suffix when none was captured:\n%s", out)
	}
}

func TestRunQuery_TextAndRegexMutuallyExclusive(t *testing.T) {
	arch := buildDiffArchive(t, queryPodList)
	cmd := newTestQueryCommand()
	_ = cmd.Flags().Set("text", "true")
	_ = cmd.Flags().Set("regex", "true")
	cmd.SetOut(&bytes.Buffer{})

	if err := runQuery(cmd, []string{arch, "nginx"}); err == nil {
		t.Error("expected error when --text and --regex are both set")
	}
}

func TestRunQuery_InvalidRegex(t *testing.T) {
	arch := buildDiffArchive(t, queryPodList)
	cmd := newTestQueryCommand()
	_ = cmd.Flags().Set("regex", "true")
	cmd.SetOut(&bytes.Buffer{})

	if err := runQuery(cmd, []string{arch, "(unclosed"}); err == nil {
		t.Error("expected error for invalid regex")
	}
}
