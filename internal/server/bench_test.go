package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	capture "github.com/phenixblue/k8shark/internal/capture"
)

// buildBenchStore builds a CaptureStore with n pod list records for
// benchmarking. It bypasses buildTestStore because that helper only accepts
// *testing.T.
func buildBenchStore(b *testing.B, n int) *CaptureStore {
	b.Helper()
	dir := b.TempDir()
	recDir := filepath.Join(dir, "k8shark-capture", "records")
	if err := os.MkdirAll(recDir, 0o750); err != nil {
		b.Fatal(err)
	}

	index := make(capture.Index)
	for i := 0; i < n; i++ {
		apiPath := fmt.Sprintf("/api/v1/namespaces/ns%d/pods", i)
		body := json.RawMessage(fmt.Sprintf(
			`{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"pod%d","namespace":"ns%d"}}]}`,
			i, i))
		recID := fmt.Sprintf("bench-rec-%04d", i)
		rec := capture.Record{
			ID:           recID,
			CapturedAt:   time.Now().UTC(),
			APIPath:      apiPath,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: body,
		}
		data, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(recDir, recID+".json"), data, 0o644); err != nil {
			b.Fatal(err)
		}
		index[apiPath] = &capture.IndexEntry{
			APIPath:   apiPath,
			RecordIDs: []string{recID},
			Times:     []time.Time{rec.CapturedAt},
		}
	}

	metaJSON, _ := json.Marshal(capture.CaptureMetadata{
		CaptureID:         "bench-capture",
		KubernetesVersion: "v1.29.0",
		CapturedAt:        time.Now().UTC().Add(-time.Minute),
		CapturedUntil:     time.Now().UTC(),
		RecordCount:       n,
	})
	if err := os.WriteFile(filepath.Join(dir, "k8shark-capture", "metadata.json"), metaJSON, 0o644); err != nil {
		b.Fatal(err)
	}
	idxJSON, _ := json.Marshal(index)
	if err := os.WriteFile(filepath.Join(dir, "k8shark-capture", "index.json"), idxJSON, 0o644); err != nil {
		b.Fatal(err)
	}

	store, err := LoadStore(dir)
	if err != nil {
		b.Fatalf("LoadStore: %v", err)
	}
	return store
}

// BenchmarkHandler_GetList measures handler latency for a list request
// served from the in-memory store.
func BenchmarkHandler_GetList(b *testing.B) {
	store := buildBenchStore(b, 1)
	h := newHandler(store, time.Time{}, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/ns0/pods", nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			b.Fatalf("unexpected status %d", rw.Code)
		}
	}
}

// BenchmarkHandler_GetList_LargeStore measures list request latency when the
// store holds many distinct paths (simulates a large capture).
func BenchmarkHandler_GetList_LargeStore(b *testing.B) {
	for _, size := range []int{10, 100, 500} {
		size := size
		b.Run(fmt.Sprintf("store_%d_paths", size), func(b *testing.B) {
			store := buildBenchStore(b, size)
			h := newHandler(store, time.Time{}, false)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/ns0/pods", nil)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rw := httptest.NewRecorder()
				h.ServeHTTP(rw, req)
			}
		})
	}
}

// BenchmarkHandler_GetVersion measures the fast-path /version handler.
func BenchmarkHandler_GetVersion(b *testing.B) {
	store := buildBenchStore(b, 1)
	h := newHandler(store, time.Time{}, false)
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
	}
}

// BenchmarkHandler_NotFound measures handler latency when the path is absent
// from the store — exercises the full routing path without a store hit.
func BenchmarkHandler_NotFound(b *testing.B) {
	store := buildBenchStore(b, 1)
	h := newHandler(store, time.Time{}, false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/missing/widgets", nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
	}
}
