package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/phenixblue/k8shark/internal/query"
	"github.com/spf13/cobra"
)

func newTestQueryCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().StringP("output", "o", "table", "")
	cmd.Flags().String("at", "", "")
	cmd.Flags().String("resource", "", "")
	cmd.Flags().String("namespace", "", "")
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
