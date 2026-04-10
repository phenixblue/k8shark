package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestParseAPIPath(t *testing.T) {
	type tc struct {
		path      string
		group     string
		version   string
		resource  string
		namespace string
	}
	cases := []tc{
		{"/api/v1/pods", "", "v1", "pods", ""},
		{"/api/v1/namespaces/default/pods", "", "v1", "pods", "default"},
		{"/api/v1/namespaces/kube-system/configmaps", "", "v1", "configmaps", "kube-system"},
		{"/apis/apps/v1/deployments", "apps", "v1", "deployments", ""},
		{"/apis/apps/v1/namespaces/default/deployments", "apps", "v1", "deployments", "default"},
		{"/apis/batch/v1/namespaces/ci/jobs", "batch", "v1", "jobs", "ci"},
	}
	for _, tc := range cases {
		g, v, r, ns := parseAPIPath(tc.path)
		if g != tc.group || v != tc.version || r != tc.resource || ns != tc.namespace {
			t.Errorf("parseAPIPath(%q): got (%q,%q,%q,%q), want (%q,%q,%q,%q)",
				tc.path, g, v, r, ns, tc.group, tc.version, tc.resource, tc.namespace)
		}
	}
}

func TestResourceToKind(t *testing.T) {
	cases := map[string]string{
		"pods":        "Pod",
		"deployments": "Deployment",
		"configmaps":  "ConfigMap",
		"services":    "Service",
		"widgets":     "Widget", // fallback
	}
	for resource, want := range cases {
		if got := resourceToKind(resource); got != want {
			t.Errorf("resourceToKind(%q) = %q, want %q", resource, got, want)
		}
	}
}

// buildTestStore creates a temp directory with the k8shark-capture layout and
// returns a loaded CaptureStore ready for use in tests.
func buildTestStore(t *testing.T, records map[string][]byte) *CaptureStore {
	t.Helper()
	dir := t.TempDir()
	recDir := filepath.Join(dir, "k8shark-capture", "records")
	if err := os.MkdirAll(recDir, 0o750); err != nil {
		t.Fatal(err)
	}

	index := make(capture.Index)
	for apiPath, body := range records {
		recID := "rec-" + apiPath[1:] // simple deterministic ID
		// Replace slashes in ID with dashes for filesystem safety.
		for i, c := range recID {
			if c == '/' {
				recID = recID[:i] + "-" + recID[i+1:]
			}
		}
		rec := capture.Record{
			ID:           recID,
			CapturedAt:   time.Now().UTC(),
			APIPath:      apiPath,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(body),
		}
		data, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(recDir, recID+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		index[apiPath] = &capture.IndexEntry{
			APIPath:   apiPath,
			RecordIDs: []string{recID},
			Times:     []time.Time{rec.CapturedAt},
		}
	}

	metaJSON, _ := json.Marshal(capture.CaptureMetadata{
		CaptureID:         "test-capture-id",
		KubernetesVersion: "v1.29.0",
		CapturedAt:        time.Now().UTC().Add(-time.Minute),
		CapturedUntil:     time.Now().UTC(),
		RecordCount:       len(records),
	})
	if err := os.WriteFile(filepath.Join(dir, "k8shark-capture", "metadata.json"), metaJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	idxJSON, _ := json.Marshal(index)
	if err := os.WriteFile(filepath.Join(dir, "k8shark-capture", "index.json"), idxJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := LoadStore(dir)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}

func TestStore_Latest_Found(t *testing.T) {
	podList := `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"nginx"}}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(podList),
	})
	body, code, err := store.Latest("/api/v1/namespaces/default/pods", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result["kind"] != "PodList" {
		t.Errorf("expected kind=PodList, got %v", result["kind"])
	}
}

func TestStore_Latest_NotFound(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	_, code, err := store.Latest("/api/v1/namespaces/default/services", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 404 {
		t.Errorf("expected 404, got %d", code)
	}
}
