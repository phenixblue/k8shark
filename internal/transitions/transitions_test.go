package transitions

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// ---- archive builder helpers ------------------------------------------------

// buildPollArchive creates a tar.gz with consecutive snapshot records for a
// single API path. Each body in bodies is a separate snapshot record, ordered
// by their position in the slice (times start at t0 with 1-minute intervals).
func buildPollArchive(t *testing.T, apiPath string, bodies []string) string {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test.tar.gz")

	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	idx := make(capture.Index)

	w, err := archive.NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}

	for i, body := range bodies {
		at := t0.Add(time.Duration(i) * time.Minute)
		rec := &capture.Record{
			ID:           fmt.Sprintf("snap-%d", i),
			CapturedAt:   at,
			APIPath:      apiPath,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(body),
		}
		if err := w.WriteRecord(rec); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}

		e := idx[apiPath]
		if e == nil {
			e = &capture.IndexEntry{APIPath: apiPath}
			idx[apiPath] = e
		}
		e.Seqs = append(e.Seqs, i)
		e.Times = append(e.Times, at)
	}

	meta := &capture.CaptureMetadata{
		CaptureID:     "poll-test",
		CapturedAt:    t0,
		CapturedUntil: t0.Add(time.Duration(len(bodies)) * time.Minute),
		RecordCount:   len(bodies),
	}
	if err := w.Finish(meta, idx, nil); err != nil {
		t.Fatalf("buildPollArchive Finish: %v", err)
	}
	return outPath
}

// buildWatchArchive creates a tar.gz with a single snapshot record and watch
// event records for the same API path.
func buildWatchArchive(t *testing.T, apiPath, snapBody string, events []watchEvent) string {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test.tar.gz")

	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)

	snapRec := &capture.Record{
		ID:           "snap-0",
		CapturedAt:   t0,
		APIPath:      apiPath,
		HTTPMethod:   "GET",
		ResponseCode: 200,
		ResponseBody: json.RawMessage(snapBody),
	}

	idx := capture.Index{
		apiPath: {
			APIPath: apiPath,
			Seqs:    []int{0},
			Times:   []time.Time{t0},
		},
	}

	wi := make(capture.WatchIndex)
	wiEntry := &capture.WatchIndexEntry{APIPath: apiPath}
	wi[apiPath] = wiEntry

	w, err := archive.NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	if err := w.WriteRecord(snapRec); err != nil {
		t.Fatalf("WriteRecord(snap): %v", err)
	}

	for i, ev := range events {
		id := fmt.Sprintf("watch-%d", i)
		at := t0.Add(time.Duration(i+1) * time.Second * 5)
		rec := &capture.Record{
			ID:           id,
			CapturedAt:   at,
			APIPath:      apiPath,
			EventType:    ev.eventType,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(ev.body),
		}
		if err := w.WriteRecord(rec); err != nil {
			t.Fatalf("WriteRecord(watch %d): %v", i, err)
		}
		wiEntry.Seqs = append(wiEntry.Seqs, i+1) // seq continues after snap (seq 0)
		wiEntry.Times = append(wiEntry.Times, at)
		wiEntry.EventTypes = append(wiEntry.EventTypes, ev.eventType)
	}

	meta := &capture.CaptureMetadata{
		CaptureID:     "watch-test",
		CapturedAt:    t0,
		CapturedUntil: t0.Add(time.Minute),
		RecordCount:   1 + len(events),
	}
	if err := w.Finish(meta, idx, wi); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return outPath
}

type watchEvent struct {
	eventType string
	body      string
}

// ---- tests ------------------------------------------------------------------

func TestPollTransitions_Added(t *testing.T) {
	snap1 := `{"items":[]}`
	snap2 := `{"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	archivePath := buildPollArchive(t, "/api/v1/namespaces/default/pods", []string{snap1, snap2})

	ts, err := LoadTransitions(archivePath, FilterOpts{})
	if err != nil {
		t.Fatalf("LoadTransitions: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("expected 1 transition, got %d: %+v", len(ts), ts)
	}
	if ts[0].EventType != "ADDED" {
		t.Errorf("expected ADDED, got %s", ts[0].EventType)
	}
	if ts[0].Name != "nginx" {
		t.Errorf("expected name nginx, got %s", ts[0].Name)
	}
}

func TestPollTransitions_Modified(t *testing.T) {
	snap1 := `{"items":[{"metadata":{"name":"nginx","namespace":"default"},"status":{"phase":"Pending"}}]}`
	snap2 := `{"items":[{"metadata":{"name":"nginx","namespace":"default"},"status":{"phase":"Running"}}]}`
	archivePath := buildPollArchive(t, "/api/v1/namespaces/default/pods", []string{snap1, snap2})

	ts, err := LoadTransitions(archivePath, FilterOpts{})
	if err != nil {
		t.Fatalf("LoadTransitions: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(ts))
	}
	if ts[0].EventType != "MODIFIED" {
		t.Errorf("expected MODIFIED, got %s", ts[0].EventType)
	}
	if !strings.Contains(string(ts[0].Before), "Pending") {
		t.Errorf("expected Before to contain Pending, got %s", ts[0].Before)
	}
	if !strings.Contains(string(ts[0].After), "Running") {
		t.Errorf("expected After to contain Running, got %s", ts[0].After)
	}
}

func TestPollTransitions_Deleted(t *testing.T) {
	snap1 := `{"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	snap2 := `{"items":[]}`
	archivePath := buildPollArchive(t, "/api/v1/namespaces/default/pods", []string{snap1, snap2})

	ts, err := LoadTransitions(archivePath, FilterOpts{})
	if err != nil {
		t.Fatalf("LoadTransitions: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(ts))
	}
	if ts[0].EventType != "DELETED" {
		t.Errorf("expected DELETED, got %s", ts[0].EventType)
	}
	if ts[0].Name != "nginx" {
		t.Errorf("expected name nginx, got %s", ts[0].Name)
	}
}

func TestPollTransitions_NoChange(t *testing.T) {
	body := `{"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	archivePath := buildPollArchive(t, "/api/v1/namespaces/default/pods", []string{body, body})

	ts, err := LoadTransitions(archivePath, FilterOpts{})
	if err != nil {
		t.Fatalf("LoadTransitions: %v", err)
	}
	if len(ts) != 0 {
		t.Errorf("expected 0 transitions for identical snapshots, got %d", len(ts))
	}
}

func TestWatchTransitions_Direct(t *testing.T) {
	snap := `{"items":[]}`
	events := []watchEvent{
		{"ADDED", `{"metadata":{"name":"pod-a","namespace":"default"}}`},
		{"MODIFIED", `{"metadata":{"name":"pod-a","namespace":"default"},"status":{"phase":"Running"}}`},
		{"DELETED", `{"metadata":{"name":"pod-a","namespace":"default"}}`},
	}
	archivePath := buildWatchArchive(t, "/api/v1/namespaces/default/pods", snap, events)

	ts, err := LoadTransitions(archivePath, FilterOpts{})
	if err != nil {
		t.Fatalf("LoadTransitions: %v", err)
	}
	if len(ts) != 3 {
		t.Fatalf("expected 3 transitions, got %d: %+v", len(ts), ts)
	}
	if ts[0].EventType != "ADDED" {
		t.Errorf("[0] expected ADDED, got %s", ts[0].EventType)
	}
	if ts[1].EventType != "MODIFIED" {
		t.Errorf("[1] expected MODIFIED, got %s", ts[1].EventType)
	}
	// Before should be set to the ADDED state body.
	if !strings.Contains(string(ts[1].Before), "pod-a") {
		t.Errorf("[1].Before expected pod-a, got %s", ts[1].Before)
	}
	if ts[2].EventType != "DELETED" {
		t.Errorf("[2] expected DELETED, got %s", ts[2].EventType)
	}
}

func TestFilterByName(t *testing.T) {
	snap1 := `{"items":[]}`
	snap2 := `{"items":[{"metadata":{"name":"nginx","namespace":"default"}},{"metadata":{"name":"redis","namespace":"default"}}]}`
	archivePath := buildPollArchive(t, "/api/v1/namespaces/default/pods", []string{snap1, snap2})

	ts, err := LoadTransitions(archivePath, FilterOpts{Name: "nginx"})
	if err != nil {
		t.Fatalf("LoadTransitions: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(ts))
	}
	if ts[0].Name != "nginx" {
		t.Errorf("expected nginx, got %s", ts[0].Name)
	}
}

func TestFilterByResource(t *testing.T) {
	snap1 := `{"items":[]}`
	snap2 := `{"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	archive1 := buildPollArchive(t, "/api/v1/namespaces/default/pods", []string{snap1, snap2})

	// Only match pods, not services.
	ts, err := LoadTransitions(archive1, FilterOpts{Resource: "pods"})
	if err != nil {
		t.Fatalf("LoadTransitions: %v", err)
	}
	if len(ts) != 1 || ts[0].Resource != "pods" {
		t.Errorf("expected 1 pod transition, got %d: %+v", len(ts), ts)
	}

	ts2, err := LoadTransitions(archive1, FilterOpts{Resource: "services"})
	if err != nil {
		t.Fatalf("LoadTransitions (services): %v", err)
	}
	if len(ts2) != 0 {
		t.Errorf("expected 0 service transitions, got %d", len(ts2))
	}
}

func TestFilterByTimeWindow(t *testing.T) {
	snap1 := `{"items":[]}`
	snap2 := `{"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	snap3 := `{"items":[{"metadata":{"name":"redis","namespace":"default"}},{"metadata":{"name":"nginx","namespace":"default"}}]}`
	archivePath := buildPollArchive(t, "/api/v1/namespaces/default/pods", []string{snap1, snap2, snap3})

	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	// snap2 at t0+1m, snap3 at t0+2m
	// Use --since after snap2 timestamp → only snap3 change (redis ADDED) visible.
	since := t0.Add(90 * time.Second)

	ts, err := LoadTransitions(archivePath, FilterOpts{Since: since})
	if err != nil {
		t.Fatalf("LoadTransitions: %v", err)
	}
	if len(ts) != 1 {
		t.Fatalf("expected 1 transition after window, got %d: %+v", len(ts), ts)
	}
	if ts[0].Name != "redis" {
		t.Errorf("expected redis, got %s", ts[0].Name)
	}
}

func TestDiffJSON_Changed(t *testing.T) {
	before := json.RawMessage(`{"status":{"phase":"Pending"}}`)
	after := json.RawMessage(`{"status":{"phase":"Running"}}`)

	d, err := DiffJSON(before, after)
	if err != nil {
		t.Fatalf("DiffJSON: %v", err)
	}
	if d == "" {
		t.Fatal("expected non-empty diff")
	}
	if !strings.Contains(d, "Pending") || !strings.Contains(d, "Running") {
		t.Errorf("diff missing expected lines:\n%s", d)
	}
}

func TestDiffJSON_Identical(t *testing.T) {
	body := json.RawMessage(`{"status":{"phase":"Running"}}`)
	d, err := DiffJSON(body, body)
	if err != nil {
		t.Fatalf("DiffJSON: %v", err)
	}
	if d != "" {
		t.Errorf("expected empty diff for identical blobs, got:\n%s", d)
	}
}

// Ensure archives without watch-index.json still work (backward compat).
func TestLoadTransitions_OldArchiveNoWatchIndex(t *testing.T) {
	body := `{"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	archivePath := buildPollArchive(t, "/api/v1/namespaces/default/pods", []string{body})

	ts, err := LoadTransitions(archivePath, FilterOpts{})
	if err != nil {
		t.Fatalf("LoadTransitions: %v", err)
	}
	// Single snapshot → no pairs to diff → no transitions.
	if len(ts) != 0 {
		t.Errorf("expected 0 transitions from single snapshot, got %d", len(ts))
	}
}
