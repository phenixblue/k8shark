package server

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// buildTestArchive creates a minimal k8shark tar.gz archive with known data
// and returns its path.
func buildTestArchive(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test.tar.gz")

	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	now := time.Now().UTC()
	rec := []*capture.Record{
		{
			ID:           "rec-001",
			CapturedAt:   now,
			APIPath:      "/api/v1/namespaces/default/pods",
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(podList),
		},
	}
	meta := &capture.CaptureMetadata{
		CaptureID:         "e2e-test-capture",
		KubernetesVersion: "v1.29.0",
		CapturedAt:        now.Add(-time.Minute),
		CapturedUntil:     now,
		RecordCount:       1,
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/pods": {
			APIPath:   "/api/v1/namespaces/default/pods",
			RecordIDs: []string{"rec-001"},
			Times:     []time.Time{now},
		},
	}

	if err := archive.Write(outPath, meta, rec, idx); err != nil {
		t.Fatalf("archive.Write: %v", err)
	}
	return outPath
}

func TestServer_Open_EndToEnd(t *testing.T) {
	archivePath := buildTestArchive(t)

	kubeconfigOut := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	srv, err := Open(OpenOptions{
		ArchivePath:   archivePath,
		KubeconfigOut: kubeconfigOut,
		Verbose:       false,
	})
	if err != nil {
		t.Fatalf("server.Open: %v", err)
	}
	defer srv.Shutdown()

	// Kubeconfig must exist.
	if _, err := os.Stat(kubeconfigOut); err != nil {
		t.Fatalf("kubeconfig not written: %v", err)
	}
	// Address must be set.
	if srv.Address() == "" {
		t.Fatal("server address is empty")
	}

	// Query the mock server (accept self-signed TLS cert in tests).
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 — test only
		},
	}

	for _, tc := range []struct {
		name     string
		path     string
		wantKind string
	}{
		{"api-versions", "/api", "APIVersions"},
		{"api-groups", "/apis", "APIGroupList"},
		{"core-resource-list", "/api/v1", "APIResourceList"},
		{"pod-list", "/api/v1/namespaces/default/pods", "PodList"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := fmt.Sprintf("%s%s", srv.Address(), tc.path)
			resp, err := client.Get(url)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", resp.StatusCode)
			}
			var result map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if result["kind"] != tc.wantKind {
				t.Errorf("expected kind=%q, got %v", tc.wantKind, result["kind"])
			}
		})
	}

	// Verify pod data round-trips correctly.
	url := fmt.Sprintf("%s/api/v1/namespaces/default/pods", srv.Address())
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET pods: %v", err)
	}
	defer resp.Body.Close()
	var pods map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&pods); err != nil {
		t.Fatal(err)
	}
	items, ok := pods["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatal("expected at least one pod in items")
	}
	item := items[0].(map[string]any)
	meta := item["metadata"].(map[string]any)
	if meta["name"] != "nginx" {
		t.Errorf("expected pod name=nginx, got %v", meta["name"])
	}
}
