package archive

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// bombSize is how large a "bomb" fixture decompresses to: one byte past the
// cap is all the guard needs to see to reject it, so tests target this
// instead of an arbitrarily large size like 1 GiB.
const bombSize = maxEntryBytes + 1

// buildZstdBomb streams n zero bytes through a zstd encoder in fixed-size
// chunks, without ever materializing them as one contiguous n-byte slice,
// and returns the (tiny) compressed result.
func buildZstdBomb(t *testing.T, n int) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	var buf bytes.Buffer
	enc.Reset(&buf)
	chunk := make([]byte, 1<<20) // 1 MiB of zeros, reused across writes
	for remaining := n; remaining > 0; {
		w := len(chunk)
		if remaining < w {
			w = remaining
		}
		if _, err := enc.Write(chunk[:w]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		remaining -= w
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// TestZstdDecompress_BombRejected verifies zstdDecompress itself rejects a
// zstd "bomb": a payload that compresses many zero bytes down to a few KB but
// would otherwise decompress back to the full size, unbounded (#214).
func TestZstdDecompress_BombRejected(t *testing.T) {
	compressed := buildZstdBomb(t, bombSize)
	t.Logf("%d zero bytes compressed to %d bytes", bombSize, len(compressed))

	if _, err := zstdDecompress(compressed); err == nil {
		t.Fatal("expected a size-limit error, got nil")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want it to mention the size limit", err)
	}
}

// TestArchive_ZstdBombRecord_ReturnsSizeLimitError verifies the full read
// path (Archive.ReadRecord -> readZstd -> zstdDecompress) rejects a record
// whose compressed size is tiny but whose decompressed size is enormous,
// with an error naming both the archive path and the limit (#214).
func TestArchive_ZstdBombRecord_ReturnsSizeLimitError(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "zstd-bomb.kshrk")

	sw, err := NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	bomb := map[string]any{
		"id": "bomb-1", "api_path": "/api/v1/namespaces/default/pods",
		"http_method": "GET", "response_code": 200,
		// Highly compressible: one byte past the cap once decompressed, but
		// tiny once zstd-compressed. WriteRecord's any-typed API has no
		// streaming form, so this string is necessarily materialized whole —
		// bombSize (not an arbitrarily large size) keeps that to a minimum.
		"response_body": strings.Repeat("A", bombSize),
	}
	if _, err := sw.WriteRecord(bomb); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if err := sw.Finish(map[string]any{"format_version": 1, "capture_id": "zstd-bomb"}, map[string]any{}, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	ar, err := Open(outPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ar.Close()

	_, err = ar.ReadRecord("/api/v1/namespaces/default/pods", 0)
	if err == nil {
		t.Fatal("expected a size-limit error reading a zstd-bomb record, got nil")
	}
	if !strings.Contains(err.Error(), outPath) {
		t.Errorf("error = %v, want it to name the archive path %q", err, outPath)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d", int64(maxEntryBytes))) {
		t.Errorf("error = %v, want it to name the limit %d", err, int64(maxEntryBytes))
	}
}

// TestOpen_DeflateBombEntry_ReturnsSizeLimitError verifies readRaw rejects a
// ZIP entry using ordinary Deflate (which zip.File.Open decompresses
// transparently) whose compressed size is tiny but whose decompressed size
// is enormous — the same bomb class as the zstd case, through the other read
// path (#214).
func TestOpen_DeflateBombEntry_ReturnsSizeLimitError(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "deflate-bomb.kshrk")

	f, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "k8shark-capture/metadata.json", Method: zip.Deflate})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	chunk := bytes.Repeat([]byte("A"), 1<<20) // 1 MiB, highly compressible
	chunks := bombSize/(1<<20) + 1            // one MiB past the cap, not an arbitrarily large size
	for i := 0; i < chunks; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("write chunk %d: %v", i, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file Close: %v", err)
	}

	ar, err := Open(outPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ar.Close()

	var meta map[string]any
	err = ar.ReadMetadata(&meta)
	if err == nil {
		t.Fatal("expected a size-limit error reading a deflate-bomb entry, got nil")
	}
	if !strings.Contains(err.Error(), outPath) {
		t.Errorf("error = %v, want it to name the archive path %q", err, outPath)
	}
}

// TestArchive_LargeLegitimateEntry_StillOpens verifies a large-but-legitimate
// single entry (well under maxEntryBytes) still reads normally — the cap
// must not regress ordinary large captures.
func TestArchive_LargeLegitimateEntry_StillOpens(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "large-legit.kshrk")

	sw, err := NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	// Random (not compressible), so the entry stays sizable after zstd —
	// still comfortably under the cap.
	body := make([]byte, 10<<20) // 10 MiB
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	rec := map[string]any{
		"id": "large-1", "api_path": "/api/v1/namespaces/default/pods",
		"http_method": "GET", "response_code": 200,
		"response_body": fmt.Sprintf("%x", body),
	}
	if _, err := sw.WriteRecord(rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if err := sw.Finish(map[string]any{"format_version": 1, "capture_id": "large-legit"}, map[string]any{}, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	ar, err := Open(outPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ar.Close()

	data, err := ar.ReadRecord("/api/v1/namespaces/default/pods", 0)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected a non-empty record body")
	}
}

// TestOpen_ZipSlipEntryNameIsInert pins the invariant noted in #214: openArchive
// builds an exact-name lookup (byName) and nothing in this package ever joins
// an entry's name onto a filesystem path to extract it, so a maliciously
// named entry (path traversal via "..") can't escape anywhere — unlike a
// tar/zip extractor that does. This builds an archive containing such an
// entry and asserts opening/reading it succeeds without creating any file
// outside the test's own temp directory.
func TestOpen_ZipSlipEntryNameIsInert(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "zipslip.kshrk")

	f, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	zw := zip.NewWriter(f)
	mw, err := zw.CreateHeader(&zip.FileHeader{Name: "k8shark-capture/metadata.json", Method: zip.Store})
	if err != nil {
		t.Fatalf("CreateHeader(metadata): %v", err)
	}
	if _, err := mw.Write([]byte(`{"format_version":1,"capture_id":"zipslip-test"}`)); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	markerName := "k8shark-zipslip-marker-" + t.Name()
	traversalName := "../../../../../../../../../../tmp/" + markerName
	ew, err := zw.CreateHeader(&zip.FileHeader{Name: traversalName, Method: zip.Store})
	if err != nil {
		t.Fatalf("CreateHeader(traversal): %v", err)
	}
	if _, err := ew.Write([]byte("payload")); err != nil {
		t.Fatalf("write traversal entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file Close: %v", err)
	}

	ar, err := Open(outPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ar.Close()

	var meta map[string]any
	if err := ar.ReadMetadata(&meta); err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta["capture_id"] != "zipslip-test" {
		t.Fatalf("unexpected metadata: %+v", meta)
	}

	// Only check for existence — never remove it. The name is unique per test
	// run (it embeds t.Name()), but this path is outside the test's own temp
	// directory, so a test must never delete anything there even on failure.
	markerPath := filepath.Join(os.TempDir(), markerName)
	if _, err := os.Stat(markerPath); err == nil {
		t.Fatalf("archive entry with a path-traversal name escaped to disk: %s", markerPath)
	}
}
