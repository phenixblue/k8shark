package server_test

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/redact"
	"github.com/phenixblue/k8shark/internal/server"
	"github.com/phenixblue/k8shark/internal/transitions"
)

// buildMalformedWatchIndexArchive writes a structurally valid ZIP archive
// with valid metadata.json and index.json.zst entries, but whose
// watch-index.json.zst entry decompresses to invalid JSON. Mirrors
// internal/archive's buildZipMalformedWatchIndex fixture, which established
// that ReadWatchIndex reports found=true with a non-nil error in this case
// specifically so callers can tell a corrupt watch index apart from an
// archive that simply predates the feature.
func buildMalformedWatchIndexArchive(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	// Safety net for any t.Fatalf below: on the happy path these are already
	// closed by the explicit, error-checked calls at the end of this
	// function, so this second Close just returns (and discards) an
	// "already closed" error rather than doing anything harmful — see
	// internal/archive/corrupt_test.go's buildZipMissingMetadata.
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	zstdCompress := func(data []byte) []byte {
		t.Helper()
		var buf strings.Builder
		enc, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatalf("zstd.NewWriter: %v", err)
		}
		if _, err := enc.Write(data); err != nil {
			t.Fatalf("zstd Write: %v", err)
		}
		if err := enc.Close(); err != nil {
			t.Fatalf("zstd Close: %v", err)
		}
		return []byte(buf.String())
	}

	writeEntry := func(name string, data []byte) {
		t.Helper()
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatalf("CreateHeader(%s): %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write(%s): %v", name, err)
		}
	}

	writeEntry("k8shark-capture/metadata.json", []byte(`{"format_version":1,"capture_id":"malformed-watch-index"}`))
	writeEntry("k8shark-capture/index.json.zst", zstdCompress([]byte(`{}`)))
	writeEntry("k8shark-capture/watch-index.json.zst", zstdCompress([]byte("{")))

	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file Close: %v", err)
	}
}

// TestAllReaders_RejectMalformedWatchIndex is a regression test: a
// watch-index.json.zst entry that is present but fails to parse means the
// archive is corrupt, not that it predates the watch-index feature. Every
// reader that consults ReadWatchIndex must surface that error instead of
// silently degrading to poll-based inference (transitions), dropping watch
// records from redacted output (redact), or running without watch
// transitions (server) — the same class of bug #250 fixed for a
// too-new format version.
func TestAllReaders_RejectMalformedWatchIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "malformed-watch-index.kshrk")
	buildMalformedWatchIndexArchive(t, path)

	assertWatchIndexError := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error for a malformed watch index, got nil")
		}
		if !strings.Contains(err.Error(), "watch index") {
			t.Errorf("error = %v, want it to mention the watch index", err)
		}
	}

	t.Run("server.LoadStore", func(t *testing.T) {
		ar, err := archive.Open(path)
		if err != nil {
			t.Fatalf("archive.Open: %v", err)
		}
		defer ar.Close()
		_, err = server.LoadStore(ar)
		assertWatchIndexError(t, err)
	})

	t.Run("redact.Archive", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "out.kshrk")
		_, err := redact.Archive(path, dst, redact.Options{})
		assertWatchIndexError(t, err)
	})

	t.Run("transitions.LoadTransitions", func(t *testing.T) {
		_, err := transitions.LoadTransitions(path, transitions.FilterOpts{}, nil)
		assertWatchIndexError(t, err)
	})
}
