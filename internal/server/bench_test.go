package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	capture "github.com/phenixblue/k8shark/internal/capture"
)

// buildBenchStore builds a CaptureStore with n pod list records for
// benchmarking. It bypasses buildTestStore because that helper only accepts
// *testing.T.
func buildBenchStore(b *testing.B, n int) *CaptureStore {
	b.Helper()
	dir := b.TempDir()
	outPath := filepath.Join(dir, "bench.kshrk")

	sw, err := archive.NewStreamWriter(outPath)
	if err != nil {
		b.Fatal(err)
	}

	index := make(capture.Index)
	for i := 0; i < n; i++ {
		apiPath := fmt.Sprintf("/api/v1/namespaces/ns%d/pods", i)
		body := json.RawMessage(fmt.Sprintf(
			`{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"pod%d","namespace":"ns%d"}}]}`,
			i, i))
		rec := capture.Record{
			ID:           fmt.Sprintf("bench-rec-%04d", i),
			CapturedAt:   time.Now().UTC(),
			APIPath:      apiPath,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: body,
		}
		if err := sw.WriteRecord(&rec); err != nil {
			b.Fatal(err)
		}
		index[apiPath] = &capture.IndexEntry{
			APIPath: apiPath,
			Seqs:    []int{0},
			Times:   []time.Time{rec.CapturedAt},
		}
	}

	meta := capture.CaptureMetadata{
		CaptureID:         "bench-capture",
		KubernetesVersion: "v1.29.0",
		CapturedAt:        time.Now().UTC().Add(-time.Minute),
		CapturedUntil:     time.Now().UTC(),
		RecordCount:       n,
	}
	if err := sw.Finish(&meta, index, nil); err != nil {
		b.Fatal(err)
	}

	ar, err := archive.Open(outPath)
	if err != nil {
		b.Fatalf("archive.Open: %v", err)
	}
	b.Cleanup(func() { ar.Close() })

	store, err := LoadStore(ar)
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
