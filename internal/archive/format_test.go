package archive

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/phenixblue/k8shark/internal/archive/format"
)

var update = flag.Bool("update", false, "regenerate golden testdata fixtures")

// goldenV1SHA256 pins the checked-in golden-v1.kshrk (plaintext) fixture's
// content hash. TestGoldenV1 is the guard that this build can still read
// plaintext archives written by earlier releases — see crypto_test.go's
// goldenV1PassphraseSHA256 for the encrypted-fixture counterpart. If the
// fixture goes missing, the test must fail loudly (not skip), and if it's
// regenerated via -update, updating this constant is a required second
// step — so running -update can't silently swap in "whatever the current
// writer produces" as the thing being tested against (#251).
const goldenV1SHA256 = "d7468f8f8b4b1d257f58fadbe4a8d839e1445869a8c6b11f3c4b0597e7ef4f83"

// goldenV2SHA256 pins the checked-in golden-v2.kshrk (plaintext) fixture's
// content hash — the version-2+ (wrapped index) counterpart of goldenV1SHA256
// above. TestGoldenV2 guards this build's ability to read its own current
// format going forward, the same way TestGoldenV1 guards the version-1
// bare-map shape it must keep reading for the life of the 1.x line (#219).
const goldenV2SHA256 = "37635d015b971f0441c6bddc330fe873a69d719eb5af0f975d96ef0989e46e0d"

// requireFixtureHash fails the test unless path's content hashes to want,
// naming both hashes so a deliberate regeneration says exactly what to paste
// into the pinned constant.
func requireFixtureHash(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("fixture %s has SHA-256 %s, want pinned %s — if this fixture was "+
			"regenerated intentionally (go test -update), update the pinned constant to match",
			path, got, want)
	}
}

// sampleArchive writes a minimal but representative archive (one record + index
// + metadata) to path using the production StreamWriter.
func sampleArchive(t *testing.T, path string) {
	t.Helper()
	sw, err := NewStreamWriter(path)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	rec := &format.Record{
		ID: "rec-1", APIPath: "/api/v1/namespaces/default/pods",
		HTTPMethod: "GET", ResponseCode: 200,
		ResponseBody: []byte(`{"apiVersion":"v1","kind":"PodList","items":[]}`),
	}
	if _, err := sw.WriteRecord(rec); err != nil {
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

// sampleArchiveV2 mirrors sampleArchive but builds the index/metadata using
// the actual typed format.Index/format.CaptureMetadata/format.Record values
// production code uses (internal/capture/engine.go), rather than raw
// map[string]any literals — so, unlike sampleArchive, it goes through
// format.Index's MarshalJSON and reflects the CURRENT on-disk format,
// including the version-2+ wrapped index shape (#219). sampleArchive itself
// must stay frozen to the version-1 bare-map shape it already produces: it's
// what regenerates the golden-v1 backward-compat fixture, which has to keep
// testing a true historical v1 archive, not whatever the current writer
// happens to do.
func sampleArchiveV2(t *testing.T, path string) {
	t.Helper()
	sw, err := NewStreamWriter(path)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	rec := &format.Record{
		ID: "rec-1", APIPath: "/api/v1/namespaces/default/pods",
		HTTPMethod: "GET", ResponseCode: 200,
		ResponseBody: []byte(`{"apiVersion":"v1","kind":"PodList","items":[]}`),
	}
	if _, err := sw.WriteRecord(rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	meta := &format.CaptureMetadata{
		FormatVersion: format.CurrentFormatVersion,
		CaptureID:     "golden-v2",
		RecordCount:   1,
	}
	idx := format.Index{
		"/api/v1/namespaces/default/pods": {APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}},
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

// TestArchiveFormatContract pins the on-disk layout and the Store ZIP method so
// accidental format/encoding changes fail loudly.
func TestArchiveFormatContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.kshrk")
	sampleArchiveV2(t, path)

	// Pin the pathDir derivation with a literal so a change to the hashing
	// scheme (which changes the on-disk layout) fails here independently.
	const podsPathDir = "4871feacc5b6fa6e" // SHA-256("/api/v1/namespaces/default/pods")[:8]
	if got := pathDir("/api/v1/namespaces/default/pods"); got != podsPathDir {
		t.Errorf("pathDir derivation changed: got %q, want %q — this changes the on-disk layout (bump format_version?)", got, podsPathDir)
	}

	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer zr.Close()

	want := map[string]bool{
		"k8shark-capture/metadata.json":                          false,
		"k8shark-capture/index.json.zst":                         false,
		"k8shark-capture/records/" + podsPathDir + "/0.json.zst": false,
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
	meta, err := ar.ReadMetadata()
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if meta.CaptureID != "golden-v2" {
		t.Errorf("metadata.capture_id = %v", meta.CaptureID)
	}
	if _, err := ar.ReadRecord("/api/v1/namespaces/default/pods", 0); err != nil {
		t.Errorf("ReadRecord: %v", err)
	}

	// index.json.zst must be the version-2+ wrapped shape ({"entries": {...}},
	// not a bare top-level map) so it can gain sibling fields later (#219).
	idxRaw, err := ar.readZstd("k8shark-capture/index.json.zst")
	if err != nil {
		t.Fatalf("readZstd(index): %v", err)
	}
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(idxRaw, &wrapped); err != nil {
		t.Fatalf("index.json.zst is not valid JSON: %v", err)
	}
	if _, ok := wrapped["entries"]; !ok {
		t.Errorf("index.json.zst has no top-level \"entries\" key: %s", idxRaw)
	}
}

// TestStreamWriter_WriteRecord_NilRecord verifies a nil *format.Record
// returns a clean error rather than panicking on a nil-pointer dereference —
// a regression risk introduced when WriteRecord's parameter was retyped from
// any to *format.Record (#233).
func TestStreamWriter_WriteRecord_NilRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nil-record.kshrk")
	sw, err := NewStreamWriter(path)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	defer func() { _ = sw.Abort() }()
	if _, err := sw.WriteRecord(nil); err == nil {
		t.Fatal("WriteRecord(nil) succeeded, want error")
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
	// zw.Create uses Deflate — the old behavior. Check every error so a write
	// failure can't masquerade as a passing test against a corrupt archive.
	metaW, err := zw.Create("k8shark-capture/metadata.json")
	if err != nil {
		t.Fatalf("create metadata: %v", err)
	}
	if _, err := metaW.Write([]byte(`{"format_version":1,"capture_id":"deflate"}`)); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	idxZ, err := zstdCompress([]byte(`{}`))
	if err != nil {
		t.Fatalf("zstdCompress: %v", err)
	}
	idxW, err := zw.Create("k8shark-capture/index.json.zst")
	if err != nil {
		t.Fatalf("create index: %v", err)
	}
	if _, err := idxW.Write(idxZ); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file close: %v", err)
	}

	ar, err := Open(path)
	if err != nil {
		t.Fatalf("Open(deflate archive): %v", err)
	}
	defer ar.Close()
	meta, err := ar.ReadMetadata()
	if err != nil {
		t.Fatalf("ReadMetadata(deflate): %v", err)
	}
	if meta.CaptureID != "deflate" {
		t.Errorf("capture_id = %v, want deflate", meta.CaptureID)
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
		// A missing fixture is the highest-consequence failure this test
		// guards against: it silently disables this test's check that a v1.0
		// build can still read plaintext archives written by earlier
		// releases (see TestGoldenV1Passphrase in crypto_test.go for the
		// encrypted counterpart). Fail loudly, don't skip (#251).
		t.Fatalf("golden fixture missing (run with -update to regenerate): %v", err)
	}
	if !*update {
		requireFixtureHash(t, golden, goldenV1SHA256)
	}

	ar, err := Open(golden)
	if err != nil {
		t.Fatalf("Open(golden): %v", err)
	}
	defer ar.Close()
	meta, err := ar.ReadMetadata()
	if err != nil {
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

// TestGoldenV2 is TestGoldenV1's version-2+ counterpart: it opens a
// checked-in fixture written in the current (wrapped-index) format to catch
// any future change that breaks reading it. Regenerate with:
// go test ./internal/archive -run TestGoldenV2 -update
func TestGoldenV2(t *testing.T) {
	golden := filepath.Join("testdata", "golden-v2.kshrk")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		sampleArchiveV2(t, golden)
		t.Logf("regenerated %s", golden)
	}
	if _, err := os.Stat(golden); err != nil {
		// See TestGoldenV1's identical comment: fail loudly, don't skip (#251).
		t.Fatalf("golden fixture missing (run with -update to regenerate): %v", err)
	}
	if !*update {
		requireFixtureHash(t, golden, goldenV2SHA256)
	}

	ar, err := Open(golden)
	if err != nil {
		t.Fatalf("Open(golden): %v", err)
	}
	defer ar.Close()
	meta, err := ar.ReadMetadata()
	if err != nil {
		t.Fatalf("ReadMetadata(golden): %v", err)
	}
	if meta.FormatVersion != format.CurrentFormatVersion {
		t.Errorf("golden format_version = %d, want %d", meta.FormatVersion, format.CurrentFormatVersion)
	}
	idx, err := ar.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex(golden): %v", err)
	}
	if _, ok := idx["/api/v1/namespaces/default/pods"]; !ok {
		t.Errorf("golden index missing the pods entry: %v", idx)
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
