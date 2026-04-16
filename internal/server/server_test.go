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
			APIPath: "/api/v1/namespaces/default/pods",
			Seqs:    []int{0},
			Times:   []time.Time{now},
		},
	}

	sw, err := archive.NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	if err := sw.WriteRecord(rec[0]); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("archive Finish: %v", err)
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

func TestParseReplayAt(t *testing.T) {
	meta := capture.CaptureMetadata{
		CapturedAt:    time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC),
		CapturedUntil: time.Date(2026, 4, 9, 11, 0, 0, 0, time.UTC),
	}

	t.Run("rfc3339", func(t *testing.T) {
		got, err := parseReplayAt(meta, "2026-04-09T10:30:00Z")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 4, 9, 10, 30, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("relative duration", func(t *testing.T) {
		got, err := parseReplayAt(meta, "-5m")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := time.Date(2026, 4, 9, 10, 55, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Fatalf("got %s, want %s", got, want)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		_, err := parseReplayAt(meta, "not-a-time")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("before capture", func(t *testing.T) {
		_, err := parseReplayAt(meta, "2026-04-09T09:59:59Z")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if err != nil && err.Error() == "" {
			t.Fatal("expected clear error message")
		}
	})

	t.Run("after capture", func(t *testing.T) {
		_, err := parseReplayAt(meta, "1m")
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestServer_Open_RejectsOutOfRangeAt(t *testing.T) {
	archivePath := buildTestArchive(t)
	_, err := Open(OpenOptions{ArchivePath: archivePath, At: "1900-01-01T00:00:00Z"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
