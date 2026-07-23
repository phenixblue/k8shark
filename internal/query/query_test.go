package query

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
)

func buildQueryStore(t *testing.T, bodies map[string]string) *server.CaptureStore {
	t.Helper()
	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	out := filepath.Join(t.TempDir(), "query.kshrk")
	sw, err := archive.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	idx := capture.Index{}
	i := 0
	for path, body := range bodies {
		rec := &capture.Record{ID: path, CapturedAt: now, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body)}
		if err := sw.WriteRecord(rec); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
		idx[path] = &capture.IndexEntry{APIPath: path, Seqs: []int{0}, Times: []time.Time{now}}
		i++
	}
	meta := &capture.CaptureMetadata{
		FormatVersion: capture.CurrentFormatVersion, CaptureID: "query-test",
		KubernetesVersion: "v1.30.0", CapturedAt: now.Add(-time.Minute), CapturedUntil: now, RecordCount: i,
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ar, err := archive.Open(out)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	store, err := server.LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}

func TestRun_MatchesAcrossResourceTypes(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces/prod/pods": `{"kind":"PodList","apiVersion":"v1","items":[
		  {"metadata":{"name":"web-1","namespace":"prod"},"spec":{"containers":[{"image":"nginx:alpine"}]}}
		]}`,
		"/apis/apps/v1/namespaces/prod/deployments": `{"kind":"DeploymentList","apiVersion":"apps/v1","items":[
		  {"metadata":{"name":"web","namespace":"prod"},"spec":{"template":{"spec":{"containers":[{"image":"nginx:alpine"}]}}}}
		]}`,
	})

	result, err := Run(store, Options{Expression: "{.metadata.name}"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Matches) != 2 {
		t.Fatalf("expected 2 matches across resource types, got %d: %+v", len(result.Matches), result.Matches)
	}
	got := map[string]bool{}
	for _, m := range result.Matches {
		got[m.Resource+"/"+m.Name] = true
	}
	if !got["pods/web-1"] || !got["deployments/web"] {
		t.Errorf("missing expected matches: %+v", result.Matches)
	}
}

func TestRun_FiltersByResourceAndNamespace(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces/prod/pods": `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"a","namespace":"prod"}}]}`,
		"/api/v1/namespaces/dev/pods":  `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"b","namespace":"dev"}}]}`,
	})

	result, err := Run(store, Options{Expression: "{.metadata.name}", Namespace: "dev"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Name != "b" {
		t.Fatalf("expected only dev/b, got %+v", result.Matches)
	}
}

func TestRun_ResourceFilterExcludesOtherTypes(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces/prod/pods":     `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"a"}}]}`,
		"/api/v1/namespaces/prod/services": `{"kind":"ServiceList","apiVersion":"v1","items":[{"metadata":{"name":"svc"}}]}`,
	})

	result, err := Run(store, Options{Expression: "{.metadata.name}", Resource: "services"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Resource != "services" {
		t.Fatalf("expected only services, got %+v", result.Matches)
	}
}

func TestRun_MissingFieldYieldsNoMatchNotError(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1/namespaces/prod/services": `{"kind":"ServiceList","apiVersion":"v1","items":[{"metadata":{"name":"svc"}}]}`,
	})

	result, err := Run(store, Options{Expression: "{.spec.containers[*].image}"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Matches) != 0 {
		t.Fatalf("expected no matches for a field the resource doesn't have, got %+v", result.Matches)
	}
}

func TestRun_SkipsDiscoveryAndNonResourcePaths(t *testing.T) {
	store := buildQueryStore(t, map[string]string{
		"/api/v1":                      `{"kind":"APIResourceList"}`,
		"/openapi/v2":                  `{"swagger":"2.0"}`,
		"/api/v1/namespaces/prod/pods": `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"a"}}]}`,
	})

	result, err := Run(store, Options{Expression: "{.metadata.name}"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("expected only the pod match, got %+v", result.Matches)
	}
}

func TestRun_InvalidExpression(t *testing.T) {
	store := buildQueryStore(t, map[string]string{})
	if _, err := Run(store, Options{Expression: "{.spec.["}); err == nil {
		t.Error("expected error for invalid jsonpath expression")
	}
}
