package server

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

func TestLoadStore_FormatVersionGuard(t *testing.T) {
	t.Run("legacy (no version) loads", func(t *testing.T) {
		if _, err := loadStoreWithVersion(t, 0); err != nil {
			t.Errorf("pre-versioning archive should load: %v", err)
		}
	})
	t.Run("current loads", func(t *testing.T) {
		if _, err := loadStoreWithVersion(t, capture.CurrentFormatVersion); err != nil {
			t.Errorf("current-version archive should load: %v", err)
		}
	})
	t.Run("newer is rejected", func(t *testing.T) {
		if _, err := loadStoreWithVersion(t, capture.CurrentFormatVersion+1); err == nil {
			t.Error("expected LoadStore to reject a newer format version")
		}
	})
}

func loadStoreWithVersion(t *testing.T, version int) (*CaptureStore, error) {
	t.Helper()
	now := time.Date(2026, 4, 9, 8, 0, 0, 0, time.UTC)
	body := []byte(`{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"p","namespace":"default"}}]}`)
	r := &capture.Record{ID: "r1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: body}
	idx := capture.Index{
		r.APIPath: {APIPath: r.APIPath, Seqs: []int{0}, Times: []time.Time{now}},
	}
	meta := &capture.CaptureMetadata{FormatVersion: version, CaptureID: "ver-test", CapturedAt: now, CapturedUntil: now, RecordCount: 1}

	out := filepath.Join(t.TempDir(), "capture.kshrk")
	sw, err := archive.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	if err := sw.WriteRecord(r); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ar, err := archive.Open(out)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })
	return LoadStore(ar)
}
