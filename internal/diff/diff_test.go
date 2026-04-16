package diff

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	archivepkg "github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

func TestRun_TwoArchives(t *testing.T) {
	before := buildArchive(t, map[string][]capture.Record{
		"/api/v1/namespaces/default/pods": {
			newRecord("rec-1", "/api/v1/namespaces/default/pods", time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC), `{"kind":"PodList","items":[{"metadata":{"name":"nginx"}}]}`),
		},
	})
	after := buildArchive(t, map[string][]capture.Record{
		"/api/v1/namespaces/default/pods": {
			newRecord("rec-2", "/api/v1/namespaces/default/pods", time.Date(2026, 4, 9, 10, 5, 0, 0, time.UTC), `{"kind":"PodList","items":[{"metadata":{"name":"redis"}}]}`),
		},
	})

	result, err := Run(Options{BeforeArchive: before, AfterArchive: after})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(result.Changes))
	}
	if result.Changes[0].Path != "/api/v1/namespaces/default/pods" {
		t.Fatalf("unexpected path %q", result.Changes[0].Path)
	}
	if !strings.Contains(string(result.Changes[0].Before), "nginx") || !strings.Contains(string(result.Changes[0].After), "redis") {
		t.Fatalf("unexpected before/after bodies: before=%s after=%s", result.Changes[0].Before, result.Changes[0].After)
	}
	text, err := RenderText(result, false)
	if err != nil {
		t.Fatalf("RenderText() error: %v", err)
	}
	if !strings.Contains(text, "Path: /api/v1/namespaces/default/pods") {
		t.Fatalf("expected path header in text output, got %s", text)
	}
	if !strings.Contains(text, "--- before/api/v1/namespaces/default/pods") {
		t.Fatalf("expected unified diff header, got %s", text)
	}
	if !strings.Contains(text, "+        \"name\": \"redis\"") {
		t.Fatalf("expected added line in diff, got %s", text)
	}
}

func TestRun_SameArchiveAtTimes(t *testing.T) {
	archivePath := buildArchive(t, map[string][]capture.Record{
		"/api/v1/namespaces/default/pods": {
			newRecord("rec-1", "/api/v1/namespaces/default/pods", time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC), `{"kind":"PodList","items":[{"metadata":{"name":"before"}}]}`),
			newRecord("rec-2", "/api/v1/namespaces/default/pods", time.Date(2026, 4, 9, 10, 5, 0, 0, time.UTC), `{"kind":"PodList","items":[{"metadata":{"name":"after"}}]}`),
		},
	})

	result, err := Run(Options{Archive: archivePath, BeforeAt: "2026-04-09T10:01:00Z", AfterAt: "0s"})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(result.Changes))
	}
	if !strings.Contains(string(result.Changes[0].Before), "before") || !strings.Contains(string(result.Changes[0].After), "after") {
		t.Fatalf("unexpected before/after bodies: before=%s after=%s", result.Changes[0].Before, result.Changes[0].After)
	}
}

func TestRun_Filters(t *testing.T) {
	before := buildArchive(t, map[string][]capture.Record{
		"/api/v1/namespaces/default/pods": {
			newRecord("rec-1", "/api/v1/namespaces/default/pods", time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC), `{"kind":"PodList","items":[]}`),
		},
		"/api/v1/namespaces/kube-system/pods": {
			newRecord("rec-2", "/api/v1/namespaces/kube-system/pods", time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC), `{"kind":"PodList","items":[]}`),
		},
	})
	after := buildArchive(t, map[string][]capture.Record{
		"/api/v1/namespaces/default/pods": {
			newRecord("rec-3", "/api/v1/namespaces/default/pods", time.Date(2026, 4, 9, 10, 5, 0, 0, time.UTC), `{"kind":"PodList","items":[{"metadata":{"name":"changed"}}]}`),
		},
		"/api/v1/namespaces/kube-system/pods": {
			newRecord("rec-4", "/api/v1/namespaces/kube-system/pods", time.Date(2026, 4, 9, 10, 5, 0, 0, time.UTC), `{"kind":"PodList","items":[{"metadata":{"name":"ignored"}}]}`),
		},
	})

	result, err := Run(Options{BeforeArchive: before, AfterArchive: after, Resource: "pods", Namespace: "default"})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("expected 1 filtered change, got %d", len(result.Changes))
	}
	if result.Changes[0].Namespace != "default" {
		t.Fatalf("expected default namespace, got %q", result.Changes[0].Namespace)
	}
}

func TestRun_NoDiff(t *testing.T) {
	before := buildArchive(t, map[string][]capture.Record{
		"/api/v1/namespaces/default/pods": {
			newRecord("rec-1", "/api/v1/namespaces/default/pods", time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC), `{"kind":"PodList","items":[]}`),
		},
	})
	after := buildArchive(t, map[string][]capture.Record{
		"/api/v1/namespaces/default/pods": {
			newRecord("rec-2", "/api/v1/namespaces/default/pods", time.Date(2026, 4, 9, 10, 5, 0, 0, time.UTC), `{"items":[],"kind":"PodList"}`),
		},
	})

	result, err := Run(Options{BeforeArchive: before, AfterArchive: after})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if len(result.Changes) != 0 {
		t.Fatalf("expected no changes, got %d", len(result.Changes))
	}
}

func buildArchive(t *testing.T, entries map[string][]capture.Record) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "capture.khsrk")
	index := make(capture.Index)
	var capturedAt, capturedUntil time.Time
	totalCount := 0

	sw, err := archivepkg.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	for path, records := range entries {
		entry := &capture.IndexEntry{APIPath: path}
		for i, rec := range records {
			if capturedAt.IsZero() || rec.CapturedAt.Before(capturedAt) {
				capturedAt = rec.CapturedAt
			}
			if capturedUntil.IsZero() || rec.CapturedAt.After(capturedUntil) {
				capturedUntil = rec.CapturedAt
			}
			rcopy := rec
			if err := sw.WriteRecord(&rcopy); err != nil {
				t.Fatalf("WriteRecord: %v", err)
			}
			entry.Seqs = append(entry.Seqs, i)
			entry.Times = append(entry.Times, rec.CapturedAt)
			totalCount++
		}
		index[path] = entry
	}
	meta := capture.CaptureMetadata{CaptureID: "test-capture", CapturedAt: capturedAt, CapturedUntil: capturedUntil, RecordCount: totalCount}
	if err := sw.Finish(&meta, index, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return out
}

func newRecord(id, path string, capturedAt time.Time, body string) capture.Record {
	return capture.Record{ID: id, CapturedAt: capturedAt, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body)}
}
