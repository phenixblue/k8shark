package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// varyPodList builds a realistic, *varied* PodList body — not the near-identical
// stubs the other benchmarks use. Size scales with pods.
func varyPodList(ns string, snap, pods int) json.RawMessage {
	items := make([]map[string]any, 0, pods)
	for p := 0; p < pods; p++ {
		items = append(items, map[string]any{
			"metadata": map[string]any{
				"name":      fmt.Sprintf("%s-workload-%d-%d-%x", ns, p, snap, p*7+snap),
				"namespace": ns, "uid": fmt.Sprintf("uid-%s-%04d-%04d", ns, p, snap),
				"resourceVersion":   strconv.Itoa(1_000_000 + snap*101 + p),
				"creationTimestamp": "2026-04-10T10:00:00Z",
				"labels": map[string]any{
					"app": fmt.Sprintf("svc-%d", p%9), "pod-template-hash": fmt.Sprintf("%x", p*13+snap),
					"tier": []string{"frontend", "backend", "cache"}[p%3],
				},
			},
			"spec": map[string]any{
				"nodeName": fmt.Sprintf("node-%d", p%12),
				"containers": []map[string]any{{
					"name": "main", "image": fmt.Sprintf("registry.example.com/%s/app:v1.%d.%d", ns, snap%9, p%5),
					"resources": map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi"}},
				}},
			},
			"status": map[string]any{
				"phase": []string{"Running", "Pending", "Succeeded"}[(p+snap)%3],
				"podIP": fmt.Sprintf("10.%d.%d.%d", snap%255, p%255, (p+snap)%255),
				"containerStatuses": []map[string]any{{
					"name": "main", "ready": p%4 != 0, "restartCount": (p + snap) % 6,
				}},
			},
		})
	}
	b, _ := json.Marshal(map[string]any{"kind": "PodList", "apiVersion": "v1", "items": items})
	return b
}

// writeLargeArchive generates a varied multi-snapshot capture across many
// namespaces and resource types. Returns the record count and total
// uncompressed record-body bytes.
func writeLargeArchive(tb testing.TB, path string, namespaces, snapshots, pods int) (int, int64) {
	tb.Helper()
	sw, err := archive.NewStreamWriter(path)
	if err != nil {
		tb.Fatal(err)
	}
	resources := []string{"pods", "deployments", "replicasets", "services", "configmaps"}
	base := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	index := make(capture.Index)
	var rawBytes int64
	recCount := 0
	for nsi := 0; nsi < namespaces; nsi++ {
		ns := fmt.Sprintf("ns-%03d", nsi)
		for _, res := range resources {
			group := "api/v1"
			if res != "pods" && res != "services" && res != "configmaps" {
				group = "apis/apps/v1"
			}
			apiPath := fmt.Sprintf("/%s/namespaces/%s/%s", group, ns, res)
			entry := &capture.IndexEntry{APIPath: apiPath}
			for s := 0; s < snapshots; s++ {
				// pods get the big varied body; other types a smaller list.
				n := pods
				if res != "pods" {
					n = pods / 4
				}
				body := varyPodList(ns, s, n)
				rawBytes += int64(len(body))
				ts := base.Add(time.Duration(s) * 30 * time.Second)
				rec := capture.Record{
					ID: fmt.Sprintf("%s-%s-%d", ns, res, s), CapturedAt: ts,
					APIPath: apiPath, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: body,
				}
				if err := sw.WriteRecord(&rec); err != nil {
					tb.Fatal(err)
				}
				entry.Seqs = append(entry.Seqs, s)
				entry.Times = append(entry.Times, ts)
				recCount++
			}
			index[apiPath] = entry
		}
	}
	meta := capture.CaptureMetadata{
		FormatVersion: capture.CurrentFormatVersion, CaptureID: "large-bench",
		CapturedAt: base, CapturedUntil: base.Add(time.Duration(snapshots) * 30 * time.Second),
		RecordCount: recCount,
	}
	if err := sw.Finish(&meta, index, nil); err != nil {
		tb.Fatal(err)
	}
	return recCount, rawBytes
}

// sweep simulates open/ui usage: reconstruct every list path at several points
// in time, which populates the record + response caches.
func sweep(store *CaptureStore, at time.Time) {
	for path := range store.Index {
		_, _, _ = store.ReconstructAt(path, at)
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// BenchmarkServeLargeCapture is the hot-path regression benchmark: a full
// reconstruct sweep over a moderately large, realistic capture.
func BenchmarkServeLargeCapture(b *testing.B) {
	path := filepath.Join(b.TempDir(), "large.kshrk")
	writeLargeArchive(b, path, 30, 10, 30) // ~30 ns × 5 res × 10 snaps = 1500 records
	ar, err := archive.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { ar.Close() })
	store, err := LoadStore(ar)
	if err != nil {
		b.Fatal(err)
	}
	end := store.Metadata.CapturedUntil
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sweep(store, end)
	}
}

// TestLargeCaptureMemory documents peak memory for a large, realistic capture.
// Sizes default to a moderate capture and can be scaled via env vars
// (KSHRK_MEM_NS / _SNAPS / _PODS) to reproduce the README numbers. Skipped in -short.
func TestLargeCaptureMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping memory measurement in -short")
	}
	ns := envInt("KSHRK_MEM_NS", 50)
	snaps := envInt("KSHRK_MEM_SNAPS", 20)
	pods := envInt("KSHRK_MEM_PODS", 40)

	path := filepath.Join(t.TempDir(), "large.kshrk")
	recs, raw := writeLargeArchive(t, path, ns, snaps, pods)
	fi, _ := os.Stat(path)

	ar, err := archive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	store, err := LoadStore(ar)
	if err != nil {
		t.Fatal(err)
	}

	// Warm caches the way open/ui would: sweep every path at a few timestamps.
	for _, frac := range []float64{0.25, 0.5, 0.75, 1.0} {
		span := store.Metadata.CapturedUntil.Sub(store.Metadata.CapturedAt)
		at := store.Metadata.CapturedAt.Add(time.Duration(float64(span) * frac))
		sweep(store, at)
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mib := func(b uint64) float64 { return float64(b) / (1 << 20) }
	t.Logf("LARGE CAPTURE MEMORY")
	t.Logf("  namespaces=%d snapshots=%d podsPerList=%d", ns, snaps, pods)
	t.Logf("  records=%d  raw record bytes=%.1f MiB  archive on disk=%.1f MiB",
		recs, float64(raw)/(1<<20), float64(fi.Size())/(1<<20))
	t.Logf("  record cache cap=%.0f MiB  response cache cap=%.0f MiB",
		float64(recordCacheMaxBytes)/(1<<20), float64(responseCacheMaxBytes)/(1<<20))
	t.Logf("  HeapInuse=%.1f MiB  HeapAlloc=%.1f MiB  Sys=%.1f MiB",
		mib(m.HeapInuse), mib(m.HeapAlloc), mib(m.Sys))
}
