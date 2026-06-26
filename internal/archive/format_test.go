package archive

import (
	"archive/zip"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "regenerate golden testdata fixtures")

// sampleArchive writes a minimal but representative archive (one record + index
// + metadata) to path using the production StreamWriter.
func sampleArchive(t *testing.T, path string) {
	t.Helper()
	sw, err := NewStreamWriter(path)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	rec := map[string]any{
		"id": "rec-1", "api_path": "/api/v1/namespaces/default/pods",
		"http_method": "GET", "response_code": 200,
		"response_body": map[string]any{"apiVersion": "v1", "kind": "PodList", "items": []any{}},
	}
	if err := sw.WriteRecord(rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	meta := map[string]any{"format_version": 1, "capture_id": "golden-v1", "record_count": 1}
	idx := map[string]any{
		"/api/v1/namespaces/default/pods": map[string]any{
			"api_path": "/api/v1/namespaces/default/pods", "seqs": []int{0},
		},
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

// TestArchiveFormatContract pins the on-disk layout and the Store ZIP method so
// accidental format/encoding changes fail loudly.
func TestArchiveFormatContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.kshrk")
	sampleArchive(t, path)

	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer zr.Close()

	want := map[string]bool{
		"k8shark-capture/metadata.json":  false,
		"k8shark-capture/index.json.zst": false,
		"k8shark-capture/records/" + pathDir("/api/v1/namespaces/default/pods") + "/0.json.zst": false,
	}
	for _, f := range zr.File {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
		}
		// Every entry must be Stored (payloads are already Zstd / small JSON).
		if f.Method != zip.Store {
			t.Errorf("entry %q uses ZIP method %d, want Store (%d)", f.Name, f.Method, zip.Store)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected archive entry %q not found", name)
		}
	}

	// Round-trip via the production reader.
	ar, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ar.Close()
	var meta map[string]any
	if err := ar.ReadMetadata(&meta); err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta["capture_id"] != "golden-v1" {
		t.Errorf("metadata.capture_id = %v", meta["capture_id"])
	}
	if _, err := ar.ReadRecord("/api/v1/namespaces/default/pods", 0); err != nil {
		t.Errorf("ReadRecord: %v", err)
	}
}

// TestReaderAcceptsDeflateEntries proves the switch to Store stays
// backward-compatible: an archive whose entries were written with Deflate (the
// pre-change behavior) still opens, because the reader is ZIP-method-agnostic.
func TestReaderAcceptsDeflateEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deflate.kshrk")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := zip.NewWriter(f)
	// zw.Create uses Deflate — the old behavior.
	metaW, _ := zw.Create("k8shark-capture/metadata.json")
	_, _ = metaW.Write([]byte(`{"format_version":1,"capture_id":"deflate"}`))
	idxW, _ := zw.Create("k8shark-capture/index.json.zst")
	idxZ, _ := zstdCompress([]byte(`{}`))
	_, _ = idxW.Write(idxZ)
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	f.Close()

	ar, err := Open(path)
	if err != nil {
		t.Fatalf("Open(deflate archive): %v", err)
	}
	defer ar.Close()
	var meta map[string]any
	if err := ar.ReadMetadata(&meta); err != nil {
		t.Fatalf("ReadMetadata(deflate): %v", err)
	}
	if meta["capture_id"] != "deflate" {
		t.Errorf("capture_id = %v, want deflate", meta["capture_id"])
	}
}

// TestGoldenV1 opens a checked-in v1 fixture to catch any future change that
// breaks reading existing archives. Regenerate with: go test ./internal/archive -run TestGoldenV1 -update
func TestGoldenV1(t *testing.T) {
	golden := filepath.Join("testdata", "golden-v1.kshrk")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		sampleArchive(t, golden)
		t.Logf("regenerated %s", golden)
	}
	if _, err := os.Stat(golden); err != nil {
		t.Skipf("golden fixture missing (run with -update): %v", err)
	}

	ar, err := Open(golden)
	if err != nil {
		t.Fatalf("Open(golden): %v", err)
	}
	defer ar.Close()
	var meta struct {
		FormatVersion int    `json:"format_version"`
		CaptureID     string `json:"capture_id"`
	}
	if err := ar.ReadMetadata(&meta); err != nil {
		t.Fatalf("ReadMetadata(golden): %v", err)
	}
	if meta.FormatVersion != 1 {
		t.Errorf("golden format_version = %d, want 1", meta.FormatVersion)
	}
	data, err := ar.ReadRecord("/api/v1/namespaces/default/pods", 0)
	if err != nil {
		t.Fatalf("ReadRecord(golden): %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("record JSON invalid: %v", err)
	}
}
