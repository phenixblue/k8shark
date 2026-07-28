package archive

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// buildZipMissingMetadata writes a structurally valid ZIP archive with an
// index.json.zst entry but no metadata.json — e.g. a capture interrupted
// between Finish's writes. Uses zip.Store (via CreateHeader), matching how
// writeBytes in archive.go actually writes entries.
func buildZipMissingMetadata(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	// Safety net for any t.Fatalf below: on the happy path these are already
	// closed by the explicit, error-checked calls at the end of the
	// function, so this second Close just returns (and discards) an
	// "already closed" error rather than doing anything harmful.
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	idxZ, err := zstdCompress([]byte(`{}`))
	if err != nil {
		t.Fatalf("zstdCompress: %v", err)
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "k8shark-capture/index.json.zst", Method: zip.Store})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
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
	defer f.Close() // safety net; see buildZipMissingMetadata
	zw := zip.NewWriter(f)
	defer zw.Close()

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

// buildZipMalformedWatchIndex writes a structurally valid ZIP archive with
// valid metadata.json and index.json.zst entries, but whose
// watch-index.json.zst entry decompresses to invalid JSON.
func buildZipMalformedWatchIndex(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	defer f.Close() // safety net; see buildZipMissingMetadata
	zw := zip.NewWriter(f)
	defer zw.Close()

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
	idxZ, err := zstdCompress([]byte(`{}`))
	if err != nil {
		t.Fatalf("zstdCompress(index): %v", err)
	}
	writeEntry("k8shark-capture/index.json.zst", idxZ)
	watchZ, err := zstdCompress([]byte("{"))
	if err != nil {
		t.Fatalf("zstdCompress(watch-index): %v", err)
	}
	writeEntry("k8shark-capture/watch-index.json.zst", watchZ)

	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file Close: %v", err)
	}
}

// TestReadWatchIndex_MalformedEntry_StillReportsPresent is a regression test:
// ReadWatchIndex's returned bool means "the entry is present in the
// archive", independent of whether it parsed. A malformed watch-index.json.zst
// must still report present=true (with a non-nil error) so a caller that
// checks err first never mistakes a corrupt watch index for an archive that
// simply predates the watch-index feature.
func TestReadWatchIndex_MalformedEntry_StillReportsPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "malformed-watch-index.kshrk")
	buildZipMalformedWatchIndex(t, path)

	ar, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ar.Close()

	_, found, err := ar.ReadWatchIndex()
	if err == nil {
		t.Fatal("ReadWatchIndex on a malformed entry succeeded, want error")
	}
	if !found {
		t.Error("ReadWatchIndex found = false, want true — the entry is present even though it failed to parse")
	}
}

// buildZipNullWatchIndexEntry writes a structurally valid ZIP archive whose
// watch-index.json.zst is valid JSON but maps a path to a null entry — the
// writer never produces this shape (it only ever inserts a populated
// *WatchIndexEntry), so it can only come from a hand-crafted or corrupted
// file.
func buildZipNullWatchIndexEntry(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	defer f.Close() // safety net; see buildZipMissingMetadata
	zw := zip.NewWriter(f)
	defer zw.Close()

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

	writeEntry("k8shark-capture/metadata.json", []byte(`{"format_version":1,"capture_id":"null-watch-index-entry"}`))
	idxZ, err := zstdCompress([]byte(`{}`))
	if err != nil {
		t.Fatalf("zstdCompress(index): %v", err)
	}
	writeEntry("k8shark-capture/index.json.zst", idxZ)
	watchZ, err := zstdCompress([]byte(`{"/api/v1/namespaces/default/pods":null}`))
	if err != nil {
		t.Fatalf("zstdCompress(watch-index): %v", err)
	}
	writeEntry("k8shark-capture/watch-index.json.zst", watchZ)

	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file Close: %v", err)
	}
}

// buildZipUndecompressableWatchIndex writes a structurally valid ZIP archive
// whose watch-index.json.zst entry is not valid Zstd data at all (as opposed
// to buildZipMalformedWatchIndex, whose entry decompresses fine but fails to
// parse as JSON) — e.g. bit-rot in the compressed bytes themselves.
func buildZipUndecompressableWatchIndex(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	defer f.Close() // safety net; see buildZipMissingMetadata
	zw := zip.NewWriter(f)
	defer zw.Close()

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

	writeEntry("k8shark-capture/metadata.json", []byte(`{"format_version":1,"capture_id":"undecompressable-watch-index"}`))
	idxZ, err := zstdCompress([]byte(`{}`))
	if err != nil {
		t.Fatalf("zstdCompress(index): %v", err)
	}
	writeEntry("k8shark-capture/index.json.zst", idxZ)
	writeEntry("k8shark-capture/watch-index.json.zst", []byte("not zstd data at all"))

	if err := zw.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file Close: %v", err)
	}
}

// TestReadWatchIndex_UndecompressableEntry_StillReportsPresent is a
// regression test: ReadWatchIndex's found return value means "the entry is
// present in the archive", independent of whether it could even be
// decompressed. A watch-index.json.zst entry that isn't valid Zstd data must
// still report found=true (with a non-nil error), the same as an entry that
// decompresses but fails to parse as JSON — otherwise a caller that checks
// err first would still be fine, but found would inconsistently read
// "absent" for one corruption mode and "present" for another.
func TestReadWatchIndex_UndecompressableEntry_StillReportsPresent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "undecompressable-watch-index.kshrk")
	buildZipUndecompressableWatchIndex(t, path)

	ar, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ar.Close()

	_, found, err := ar.ReadWatchIndex()
	if err == nil {
		t.Fatal("ReadWatchIndex on an undecompressable entry succeeded, want error")
	}
	if !found {
		t.Error("ReadWatchIndex found = false, want true — the entry is present even though it couldn't be decompressed")
	}
}

// TestReadWatchIndex_NullEntry_Rejected is a regression test: a
// watch-index.json.zst that is valid JSON but maps a path to a null entry is
// still corrupt, since every reader dereferences the entry (e.g. entry.Seqs)
// without a nil check and would panic. ReadWatchIndex must reject this shape
// rather than returning a WatchIndex containing a nil *WatchIndexEntry.
func TestReadWatchIndex_NullEntry_Rejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "null-watch-index-entry.kshrk")
	buildZipNullWatchIndexEntry(t, path)

	ar, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ar.Close()

	_, found, err := ar.ReadWatchIndex()
	if err == nil {
		t.Fatal("ReadWatchIndex on a null entry succeeded, want error")
	}
	if !found {
		t.Error("ReadWatchIndex found = false, want true — the entry is present even though it failed validation")
	}
}

// TestOpen_CorruptArchives covers every malformed-archive shape a user might
// plausibly hand kshrk — most likely a Ctrl+C'd capture (`kshrk capture`
// writes records in place via os.Create and only writes the index/metadata
// in Finish, so an interrupted capture is a ZIP with records but no central
// directory), plus a truncated transfer, garbage bytes, and a structurally
// valid ZIP with a missing or malformed metadata.json. Open reads and
// validates metadata.json eagerly (#233), so every case here fails at Open
// time — there's no longer a separate "Open succeeds, ReadMetadata fails"
// path to exercise. Every case must produce a clear, non-nil error naming
// the archive path — never a panic, a nil-map, or a wrong-but-plausible
// answer (#248).
func TestOpen_CorruptArchives(t *testing.T) {
	valid := filepath.Join(t.TempDir(), "valid.kshrk")
	sampleArchive(t, valid)
	validData, err := os.ReadFile(valid)
	if err != nil {
		t.Fatalf("reading valid fixture: %v", err)
	}

	// wantActionable cases hit zip.NewReader's ErrFormat — Go's archive/zip
	// package doesn't distinguish a missing central directory (truncation)
	// from garbage bytes, so the most honest "recognized as such" diagnosis
	// available is a single message naming both plausible causes (#248's
	// acceptance criteria: "an interrupted capture is recognized as such and
	// says so").
	cases := []struct {
		name           string
		build          func(t *testing.T, path string)
		wantActionable bool
	}{
		{
			name: "truncated at 50% (no central directory, like a Ctrl+C'd capture)",
			build: func(t *testing.T, path string) {
				half := validData[:len(validData)/2]
				if err := os.WriteFile(path, half, 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
			wantActionable: true,
		},
		{
			name: "truncated to 10 bytes",
			build: func(t *testing.T, path string) {
				n := min(10, len(validData))
				if err := os.WriteFile(path, validData[:n], 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
			wantActionable: true,
		},
		{
			name: "empty file",
			build: func(t *testing.T, path string) {
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
			wantActionable: true,
		},
		{
			name: "random bytes",
			build: func(t *testing.T, path string) {
				garbage := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, len(validData)/4+1)[:len(validData)]
				if err := os.WriteFile(path, garbage, 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			},
			wantActionable: true,
		},
		{name: "valid ZIP missing metadata.json", build: buildZipMissingMetadata},
		{name: "valid ZIP with malformed metadata.json", build: buildZipMalformedMetadata},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "corrupt.kshrk")
			tc.build(t, path)

			var openErr error
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panicked: %v", r)
					}
				}()
				ar, err := Open(path)
				openErr = err
				if err == nil {
					ar.Close()
				}
			}()

			if openErr == nil {
				t.Fatal("expected Open to fail, got nil")
			}
			if !strings.Contains(openErr.Error(), path) {
				t.Errorf("Open error = %v, want it to name the archive path %q", openErr, path)
			}
			if tc.wantActionable && !strings.Contains(openErr.Error(), "corrupt or incomplete") {
				t.Errorf("Open error = %v, want it to recognize the archive as corrupt or incomplete", openErr)
			}
		})
	}
}

// TestOpenWithIdentities_EncryptedCorruptZip covers the encrypted counterpart
// of TestOpen_CorruptArchives's corrupt-zip cases. A truncated encrypted
// capture is normally caught earlier, by age's own AEAD authentication (the
// ciphertext fails to decrypt at all), so to exercise the case where
// decryption succeeds but the recovered plaintext still isn't a valid zip —
// e.g. a bug elsewhere produced a broken archive before it was ever
// encrypted — this encrypts garbage bytes directly rather than going through
// StreamWriter, bypassing that earlier check on purpose (#248).
func TestOpenWithIdentities_EncryptedCorruptZip(t *testing.T) {
	recipients, err := RecipientsFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("RecipientsFromPassphrase: %v", err)
	}
	path := filepath.Join(t.TempDir(), "encrypted-corrupt.kshrk")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	defer f.Close() // safety net; see buildZipMissingMetadata
	w, err := age.Encrypt(f, recipients...)
	if err != nil {
		t.Fatalf("age.Encrypt: %v", err)
	}
	defer w.Close() // safety net; see buildZipMissingMetadata
	if _, err := w.Write(bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 64)); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("age writer Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file Close: %v", err)
	}

	identities, err := IdentitiesFromPassphrase(testPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	_, err = OpenWithIdentities(path, identities)
	if err == nil {
		t.Fatal("OpenWithIdentities on an encrypted-but-corrupt-plaintext archive succeeded, want error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %v, want it to name the archive path %q", err, path)
	}
	if !strings.Contains(err.Error(), "corrupt or incomplete") {
		t.Errorf("error = %v, want it to recognize the archive as corrupt or incomplete", err)
	}
}
