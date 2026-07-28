package archive

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildZipMissingMetadata writes a structurally valid ZIP archive with an
// index.json.zst entry but no metadata.json — e.g. a capture interrupted
// between Finish's writes.
func buildZipMissingMetadata(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	zw := zip.NewWriter(f)
	idxZ, err := zstdCompress([]byte(`{}`))
	if err != nil {
		t.Fatalf("zstdCompress: %v", err)
	}
	w, err := zw.Create("k8shark-capture/index.json.zst")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write(idxZ); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file Close: %v", err)
	}
}

// buildZipMalformedMetadata writes a structurally valid ZIP archive whose
// metadata.json entry is present but not valid JSON.
func buildZipMalformedMetadata(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "k8shark-capture/metadata.json", Method: zip.Store})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	if _, err := w.Write([]byte("{")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file Close: %v", err)
	}
}

// TestOpen_CorruptArchives covers every malformed-archive shape a user might
// plausibly hand kshrk — most likely a Ctrl+C'd capture (`kshrk capture`
// writes records in place via os.Create and only writes the index/metadata
// in Finish, so an interrupted capture is a ZIP with records but no central
// directory), plus a truncated transfer, garbage bytes, and a structurally
// valid ZIP with a missing or malformed metadata.json. Every case must
// produce a clear, non-nil error naming the archive path — never a panic,
// a nil-map, or a wrong-but-plausible answer (#248).
func TestOpen_CorruptArchives(t *testing.T) {
	valid := filepath.Join(t.TempDir(), "valid.kshrk")
	sampleArchive(t, valid)
	validData, err := os.ReadFile(valid)
	if err != nil {
		t.Fatalf("reading valid fixture: %v", err)
	}

	cases := []struct {
		name  string
		build func(t *testing.T, path string)
	}{
		{
			name: "truncated at 50% (no central directory, like a Ctrl+C'd capture)",
			build: func(t *testing.T, path string) {
				half := validData[:len(validData)/2]
				if err := os.WriteFile(path, half, 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
		},
		{
			name: "truncated to 10 bytes",
			build: func(t *testing.T, path string) {
				n := min(10, len(validData))
				if err := os.WriteFile(path, validData[:n], 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
		},
		{
			name: "empty file",
			build: func(t *testing.T, path string) {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
		},
		{
			name: "random bytes",
			build: func(t *testing.T, path string) {
				garbage := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, len(validData)/4+1)[:len(validData)]
				if err := os.WriteFile(path, garbage, 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
		},
		{name: "valid ZIP missing metadata.json", build: buildZipMissingMetadata},
		{name: "valid ZIP with malformed metadata.json", build: buildZipMalformedMetadata},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "corrupt.kshrk")
			tc.build(t, path)

			var openErr, metaErr error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panicked: %v", r)
					}
				}()
				ar, err := Open(path)
				openErr = err
				if err != nil {
					return
				}
				defer ar.Close()
				var meta map[string]any
				metaErr = ar.ReadMetadata(&meta)
			}()

			if openErr == nil && metaErr == nil {
				t.Fatal("expected an error from Open or ReadMetadata, got nil for both")
			}
			if openErr != nil && !strings.Contains(openErr.Error(), path) {
				t.Errorf("Open error = %v, want it to name the archive path %q", openErr, path)
			}
		})
	}
}
