package archive

import (
	"bytes"
	"fmt"
	"testing"
)

// sampleRecord is a minimal JSON object with an id field — the minimum
// WriteRecord requires.
func sampleRecord(i int) any {
	return map[string]any{
		"id":            fmt.Sprintf("record-%04d", i),
		"captured_at":   "2026-04-10T10:00:00Z",
		"api_path":      "/api/v1/namespaces/default/pods",
		"http_method":   "GET",
		"response_code": 200,
		"response_body": map[string]any{
			"kind":       "PodList",
			"apiVersion": "v1",
			"items":      []any{},
		},
	}
}

// BenchmarkStreamWriter_WriteRecord measures throughput of writing individual
// records to a streaming tar.gz archive.
func BenchmarkStreamWriter_WriteRecord(b *testing.B) {
	dir := b.TempDir()
	w, err := NewStreamWriter(dir + "/capture.tar.gz")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := w.WriteRecord(sampleRecord(i)); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	_ = w.Finish(nil, nil)
}

// BenchmarkStreamWriter_RoundTrip measures the full write→finish→open cycle
// for archives of increasing record counts.
func BenchmarkStreamWriter_RoundTrip(b *testing.B) {
	for _, n := range []int{10, 100, 500} {
		n := n
		b.Run(fmt.Sprintf("%d_records", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				dir := b.TempDir()
				path := dir + "/capture.tar.gz"
				w, err := NewStreamWriter(path)
				if err != nil {
					b.Fatal(err)
				}
				for j := 0; j < n; j++ {
					if err := w.WriteRecord(sampleRecord(j)); err != nil {
						b.Fatal(err)
					}
				}
				if err := w.Finish(map[string]any{"capture_id": "bench"}, map[string]any{}); err != nil {
					b.Fatal(err)
				}
				if err := Open(path, b.TempDir()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkNDJSONWriter_WriteRecord measures throughput of NDJSON output.
func BenchmarkNDJSONWriter_WriteRecord(b *testing.B) {
	w := NewNDJSONWriter(&bytes.Buffer{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := w.WriteRecord(sampleRecord(i)); err != nil {
			b.Fatal(err)
		}
	}
}
