package archive

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// RecordSink accepts individual capture records as they arrive.
// It decouples the engine from the specific output format (tar.gz vs NDJSON).
type RecordSink interface {
	WriteRecord(rec any) error
	Finish(meta, index any) error
	RecordCount() int
}

// StreamWriter streams each record directly into a .tar.gz archive as it
// arrives. metadata.json and index.json are written by Finish(). Thread-safe.
type StreamWriter struct {
	mu sync.Mutex
	f  *os.File
	gw *gzip.Writer
	tw *tar.Writer
	n  int
}

// NewStreamWriter creates a new StreamWriter writing to outputPath.
func NewStreamWriter(outputPath string) (*StreamWriter, error) {
	f, err := os.Create(outputPath)
	if err != nil {
		return nil, fmt.Errorf("creating output file %q: %w", outputPath, err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	return &StreamWriter{f: f, gw: gw, tw: tw}, nil
}

// WriteRecord marshals rec to JSON and appends it to the tar archive.
func (w *StreamWriter) WriteRecord(rec any) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshalling record: %w", err)
	}
	var idHolder struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &idHolder); err != nil || idHolder.ID == "" {
		return fmt.Errorf("record missing id field")
	}
	path := filepath.Join("k8shark-capture", "records", idHolder.ID+".json")

	w.mu.Lock()
	defer w.mu.Unlock()
	if err := writeBytesToTar(w.tw, path, data); err != nil {
		return err
	}
	w.n++
	return nil
}

// Finish writes metadata.json and index.json then closes the archive.
func (w *StreamWriter) Finish(meta, index any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := writeJSON(w.tw, "k8shark-capture/metadata.json", meta); err != nil {
		return err
	}
	if err := writeJSON(w.tw, "k8shark-capture/index.json", index); err != nil {
		return err
	}
	if err := w.tw.Close(); err != nil {
		return fmt.Errorf("closing tar: %w", err)
	}
	if err := w.gw.Close(); err != nil {
		return fmt.Errorf("closing gzip: %w", err)
	}
	return w.f.Close()
}

// RecordCount returns the number of records written so far.
func (w *StreamWriter) RecordCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// NDJSONWriter writes each record as a newline-delimited JSON object to an
// io.Writer (typically os.Stdout). Finish is a no-op. Thread-safe.
type NDJSONWriter struct {
	mu  sync.Mutex
	w   io.Writer
	enc *json.Encoder
	n   int
}

// NewNDJSONWriter creates an NDJSONWriter writing to w.
func NewNDJSONWriter(w io.Writer) *NDJSONWriter {
	return &NDJSONWriter{w: w, enc: json.NewEncoder(w)}
}

// WriteRecord encodes rec as a single JSON line.
func (w *NDJSONWriter) WriteRecord(rec any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(rec); err != nil {
		return err
	}
	w.n++
	return nil
}

// Finish is a no-op for NDJSONWriter.
func (w *NDJSONWriter) Finish(_, _ any) error { return nil }

// RecordCount returns the number of records written so far.
func (w *NDJSONWriter) RecordCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// Writable is anything that can provide records, index, and metadata for writing.
// We use concrete types from capture to avoid circular imports by defining them as
// any — callers pass the real values.

// Write serialises all capture data into a .tar.gz archive at outputPath.
//
// Archive layout:
//
//	k8shark-capture/
//	  metadata.json
//	  index.json
//	  records/<id>.json
func Write(outputPath string, metadata any, records any, index any) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("creating output file %q: %w", outputPath, err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	if err := writeJSON(tw, "k8shark-capture/metadata.json", metadata); err != nil {
		return err
	}
	if err := writeJSON(tw, "k8shark-capture/index.json", index); err != nil {
		return err
	}

	// Use json marshal + unmarshal round-trip to get individual records as
	// []map[string]any so archive package stays import-cycle-free.
	raw, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("marshalling records: %w", err)
	}
	var recs []json.RawMessage
	if err := json.Unmarshal(raw, &recs); err != nil {
		return fmt.Errorf("unmarshalling records slice: %w", err)
	}

	for _, r := range recs {
		// Extract id field only for filename.
		var idHolder struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(r, &idHolder); err != nil || idHolder.ID == "" {
			return fmt.Errorf("record missing id field")
		}
		path := filepath.Join("k8shark-capture", "records", idHolder.ID+".json")
		if err := writeBytesToTar(tw, path, r); err != nil {
			return err
		}
	}

	return nil
}

// Open extracts a k8shark archive to destDir and returns the opened *Reader.
func Open(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening archive %q: %w", archivePath, err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("reading gzip stream: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}
		target := filepath.Join(destDir, hdr.Name) // #nosec G305 — path validated below
		if !isPathSafe(destDir, target) {
			return fmt.Errorf("unsafe tar path %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
			continue
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			out, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, 512<<20)); err != nil { // 512 MB per-file limit
				out.Close()
				return err
			}
			out.Close()
		}
	}
	return nil
}

// isPathSafe ensures target is inside destDir (prevents path traversal).
func isPathSafe(destDir, target string) bool {
	rel, err := filepath.Rel(destDir, target)
	if err != nil {
		return false
	}
	if len(rel) >= 2 && rel[:2] == ".." {
		return false
	}
	return true
}

func writeJSON(tw *tar.Writer, path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling %s: %w", path, err)
	}
	return writeBytesToTar(tw, path, data)
}

func writeBytesToTar(tw *tar.Writer, path string, data []byte) error {
	hdr := &tar.Header{
		Name: path,
		Mode: 0o644,
		Size: int64(len(data)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", path, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("writing tar body for %s: %w", path, err)
	}
	return nil
}

// ReadMetadata reads just metadata.json from an already-extracted archive directory.
func ReadMetadata(dir string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(dir, "k8shark-capture", "metadata.json"))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(data, &m)
}

// ReadIndex reads index.json from an already-extracted archive directory.
func ReadIndex(dir string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(dir, "k8shark-capture", "index.json"))
	if err != nil {
		return nil, err
	}
	var idx map[string]any
	return idx, json.Unmarshal(data, &idx)
}

// ReadRecord reads a single record file by ID.
func ReadRecord(dir, id string) ([]byte, error) {
	path := filepath.Join(dir, "k8shark-capture", "records", id+".json")
	return os.ReadFile(path)
}

// ListRecordIDs returns all record IDs present in the records/ directory.
func ListRecordIDs(dir string) ([]string, error) {
	pattern := filepath.Join(dir, "k8shark-capture", "records", "*.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		base := filepath.Base(m)
		ids = append(ids, base[:len(base)-5]) // strip .json
	}
	return ids, nil
}
