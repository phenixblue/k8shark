package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestStore_Latest_AtTimestamp(t *testing.T) {
	dir := t.TempDir()
	recDir := filepath.Join(dir, "k8shark-capture", "records")
	if err := os.MkdirAll(recDir, 0o750); err != nil {
		t.Fatal(err)
	}

	path := "/api/v1/namespaces/default/pods"
	t1 := time.Date(2026, 4, 9, 10, 40, 0, 0, time.UTC)
	t2 := t1.Add(2 * time.Minute)
	records := []capture.Record{
		{ID: "rec-1", CapturedAt: t1, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(`{"kind":"PodList","items":[{"metadata":{"name":"before"}}]}`)},
		{ID: "rec-2", CapturedAt: t2, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(`{"kind":"PodList","items":[{"metadata":{"name":"after"}}]}`)},
	}
	for _, rec := range records {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(recDir, rec.ID+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	metaJSON, _ := json.Marshal(capture.CaptureMetadata{
		CaptureID:     "test-capture-id",
		CapturedAt:    t1,
		CapturedUntil: t2,
		RecordCount:   len(records),
	})
	if err := os.WriteFile(filepath.Join(dir, "k8shark-capture", "metadata.json"), metaJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	idxJSON, _ := json.Marshal(capture.Index{
		path: {
			APIPath:   path,
			RecordIDs: []string{"rec-1", "rec-2"},
			Times:     []time.Time{t1, t2},
		},
	})
	if err := os.WriteFile(filepath.Join(dir, "k8shark-capture", "index.json"), idxJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := LoadStore(dir)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}

	body, code, err := store.Latest(path, t1.Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if string(body) == "" || !containsString(string(body), "before") {
		t.Fatalf("expected first record body, got %s", string(body))
	}

	body, code, err = store.Latest(path, t2.Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !containsString(string(body), "after") {
		t.Fatalf("expected second record body, got %s", string(body))
	}

	_, code, err = store.Latest(path, t1.Add(-time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 404 {
		t.Fatalf("expected 404 before first record, got %d", code)
	}
}

func containsString(s, sub string) bool { return strings.Contains(s, sub) }

// buildTestStoreWithWatch creates a CaptureStore with snapshot records in
// index.json and watch event records in watch-index.json.
func buildTestStoreWithWatch(t *testing.T, snapshots map[string]watchTestRecord, events []watchTestEvent) *CaptureStore {
	t.Helper()
	dir := t.TempDir()
	recDir := filepath.Join(dir, "k8shark-capture", "records")
	if err := os.MkdirAll(recDir, 0o750); err != nil {
		t.Fatal(err)
	}

	index := make(capture.Index)
	watchIndex := make(capture.WatchIndex)

	for apiPath, s := range snapshots {
		rec := capture.Record{
			ID:           s.id,
			CapturedAt:   s.at,
			APIPath:      apiPath,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(s.body),
		}
		data, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(recDir, s.id+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		index[apiPath] = &capture.IndexEntry{
			APIPath:   apiPath,
			RecordIDs: []string{s.id},
			Times:     []time.Time{s.at},
		}
	}

	for _, ev := range events {
		rec := capture.Record{
			ID:           ev.id,
			CapturedAt:   ev.at,
			APIPath:      ev.apiPath,
			EventType:    ev.eventType,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(ev.objectBody),
		}
		data, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(recDir, ev.id+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
		wi := watchIndex[ev.apiPath]
		if wi == nil {
			wi = &capture.WatchIndexEntry{APIPath: ev.apiPath}
			watchIndex[ev.apiPath] = wi
		}
		wi.RecordIDs = append(wi.RecordIDs, ev.id)
		wi.Times = append(wi.Times, ev.at)
		wi.EventTypes = append(wi.EventTypes, ev.eventType)
	}

	metaJSON, _ := json.Marshal(capture.CaptureMetadata{
		CaptureID: "test-watch-id", KubernetesVersion: "v1.29.0",
		CapturedAt: time.Now().UTC().Add(-time.Minute), CapturedUntil: time.Now().UTC(),
	})
	if err := os.WriteFile(filepath.Join(dir, "k8shark-capture", "metadata.json"), metaJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	idxJSON, _ := json.Marshal(index)
	if err := os.WriteFile(filepath.Join(dir, "k8shark-capture", "index.json"), idxJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	wiJSON, _ := json.Marshal(watchIndex)
	if err := os.WriteFile(filepath.Join(dir, "k8shark-capture", "watch-index.json"), wiJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := LoadStore(dir)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}

type watchTestRecord struct {
	id   string
	at   time.Time
	body string
}
type watchTestEvent struct {
	id         string
	apiPath    string
	at         time.Time
	eventType  string
	objectBody string
}

func TestStore_ReconstructAt_NoWatchEvents_FallsBackToLatest(t *testing.T) {
	snapBody := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(snapBody),
	})
	body, code, err := store.ReconstructAt("/api/v1/namespaces/default/pods", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !containsString(string(body), "nginx") {
		t.Errorf("expected nginx in result, got %s", string(body))
	}
}

func TestStore_ReconstructAt_Added(t *testing.T) {
	path := "/api/v1/namespaces/default/pods"
	t0 := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)

	snap := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	newPod := `{"metadata":{"name":"redis","namespace":"default"},"spec":{}}`

	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			path: {id: "snap-1", at: t0, body: snap},
		},
		[]watchTestEvent{
			{id: "ev-1", apiPath: path, at: t1, eventType: "ADDED", objectBody: newPod},
		},
	)

	body, code, err := store.ReconstructAt(path, t1.Add(time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !containsString(string(body), "nginx") {
		t.Errorf("expected nginx still present, got %s", string(body))
	}
	if !containsString(string(body), "redis") {
		t.Errorf("expected redis added, got %s", string(body))
	}
}

func TestStore_ReconstructAt_WatchOnlyPath(t *testing.T) {
	path := "/api/v1/namespaces/default/pods"
	t0 := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)

	newPod := `{"metadata":{"name":"redis","namespace":"default"},"spec":{}}`

	store := buildTestStoreWithWatch(t, nil, []watchTestEvent{
		{id: "ev-1", apiPath: path, at: t1, eventType: "ADDED", objectBody: newPod},
	})

	body, code, err := store.ReconstructAt(path, t0.Add(5*time.Second))
	if err != nil {
		t.Fatalf("unexpected error before event: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200 before event, got %d", code)
	}
	if containsString(string(body), "redis") {
		t.Fatalf("did not expect redis before event, got %s", string(body))
	}

	body, code, err = store.ReconstructAt(path, t1.Add(time.Second))
	if err != nil {
		t.Fatalf("unexpected error after event: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200 after event, got %d", code)
	}
	if !containsString(string(body), "redis") {
		t.Fatalf("expected redis after watch-only add, got %s", string(body))
	}
}

func TestStore_ReconstructAt_Modified(t *testing.T) {
	path := "/api/v1/namespaces/default/pods"
	t0 := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)

	snap := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"},"status":{"phase":"Pending"}}]}`
	modifiedPod := `{"metadata":{"name":"nginx","namespace":"default"},"status":{"phase":"Running"}}`

	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			path: {id: "snap-1", at: t0, body: snap},
		},
		[]watchTestEvent{
			{id: "ev-1", apiPath: path, at: t1, eventType: "MODIFIED", objectBody: modifiedPod},
		},
	)

	body, code, err := store.ReconstructAt(path, t1.Add(time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if containsString(string(body), "Pending") {
		t.Errorf("Pending phase should have been replaced, got %s", string(body))
	}
	if !containsString(string(body), "Running") {
		t.Errorf("expected Running phase after MODIFIED, got %s", string(body))
	}
}

func TestStore_ReconstructAt_Deleted(t *testing.T) {
	path := "/api/v1/namespaces/default/pods"
	t0 := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)

	snap := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}},{"metadata":{"name":"redis","namespace":"default"}}]}`
	deletedPod := `{"metadata":{"name":"nginx","namespace":"default"}}`

	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			path: {id: "snap-1", at: t0, body: snap},
		},
		[]watchTestEvent{
			{id: "ev-1", apiPath: path, at: t1, eventType: "DELETED", objectBody: deletedPod},
		},
	)

	body, code, err := store.ReconstructAt(path, t1.Add(time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if containsString(string(body), "nginx") {
		t.Errorf("nginx should have been deleted, got %s", string(body))
	}
	if !containsString(string(body), "redis") {
		t.Errorf("redis should still be present, got %s", string(body))
	}
}

func TestStore_ReconstructAt_EventBeforeSnapshot_Ignored(t *testing.T) {
	path := "/api/v1/namespaces/default/pods"
	t0 := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	// Event is before the snapshot — should not be applied.
	tBefore := t0.Add(-10 * time.Second)

	snap := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	stalePod := `{"metadata":{"name":"ghost","namespace":"default"}}`

	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			path: {id: "snap-1", at: t0, body: snap},
		},
		[]watchTestEvent{
			{id: "ev-1", apiPath: path, at: tBefore, eventType: "ADDED", objectBody: stalePod},
		},
	)

	body, code, err := store.ReconstructAt(path, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if containsString(string(body), "ghost") {
		t.Errorf("event before snapshot should be ignored, got %s", string(body))
	}
}

func TestStore_ReconstructAt_OldArchiveNoWatchIndex(t *testing.T) {
	// Old archives without watch-index.json must load and serve correctly.
	snapBody := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(snapBody),
	})
	// WatchIndex should be empty (not nil).
	if store.WatchIndex == nil {
		t.Fatal("WatchIndex should be initialized to empty map, not nil")
	}
	body, code, err := store.ReconstructAt("/api/v1/namespaces/default/pods", time.Time{})
	if err != nil || code != 200 {
		t.Fatalf("old archive must serve via Latest fallback: code=%d err=%v", code, err)
	}
	if !containsString(string(body), "nginx") {
		t.Errorf("expected nginx, got %s", string(body))
	}
}
