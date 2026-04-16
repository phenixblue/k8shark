package archive

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
)

// RecordSink accepts individual capture records as they arrive.
type RecordSink interface {
	WriteRecord(rec any) error
	// Finish writes metadata.json, index.json, and (when watchIndex is non-nil)
	// watch-index.json, then closes the archive.
	Finish(meta, index, watchIndex any) error
	RecordCount() int
}

// pathDir returns a short, filesystem-safe directory name for an API path.
// We use the first 16 hex chars of SHA-256 to avoid path-length issues.
func pathDir(apiPath string) string {
	sum := sha256.Sum256([]byte(apiPath))
	return fmt.Sprintf("%x", sum[:8])
}

// zstdEncoder is a package-level encoder pool for compressing record data.
var zstdEncoderPool = sync.Pool{
	New: func() any {
		enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		return enc
	},
}

func zstdCompress(data []byte) ([]byte, error) {
	enc := zstdEncoderPool.Get().(*zstd.Encoder)
	defer zstdEncoderPool.Put(enc)
	var buf bytes.Buffer
	enc.Reset(&buf)
	if _, err := enc.Write(data); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// zstdDecoder is a package-level decoder pool.
var zstdDecoderPool = sync.Pool{
	New: func() any {
		dec, _ := zstd.NewReader(nil)
		return dec
	},
}

func zstdDecompress(data []byte) ([]byte, error) {
	dec := zstdDecoderPool.Get().(*zstd.Decoder)
	defer zstdDecoderPool.Put(dec)
	if err := dec.Reset(bytes.NewReader(data)); err != nil {
		return nil, err
	}
	return io.ReadAll(dec)
}

// StreamWriter streams each record directly into a .khsrk (ZIP+Zstd) archive.
// metadata.json, index.json, and watch-index.json are written by Finish().
// Thread-safe.
type StreamWriter struct {
	mu      sync.Mutex
	f       *os.File
	zw      *zip.Writer
	n       int
	pathSeq map[string]int // apiPath → next seq number for that path's directory
}

// NewStreamWriter creates a new StreamWriter writing to outputPath.
func NewStreamWriter(outputPath string) (*StreamWriter, error) {
	f, err := os.Create(outputPath)
	if err != nil {
		return nil, fmt.Errorf("creating output file %q: %w", outputPath, err)
	}
	return &StreamWriter{
		f:       f,
		zw:      zip.NewWriter(f),
		pathSeq: make(map[string]int),
	}, nil
}

// WriteRecord marshals rec to JSON, Zstd-compresses it, and appends it
// to the ZIP archive under records/<pathDir(apiPath)>/<seq>.json.zst.
// The record must have both "id" and "api_path" fields.
func (w *StreamWriter) WriteRecord(rec any) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshalling record: %w", err)
	}
	var hdr struct {
		ID      string `json:"id"`
		APIPath string `json:"api_path"`
	}
	if err := json.Unmarshal(data, &hdr); err != nil || hdr.ID == "" || hdr.APIPath == "" {
		return fmt.Errorf("record missing id or api_path field")
	}

	compressed, err := zstdCompress(data)
	if err != nil {
		return fmt.Errorf("compressing record %s: %w", hdr.ID, err)
	}

	dir := pathDir(hdr.APIPath)

	w.mu.Lock()
	defer w.mu.Unlock()

	seq := w.pathSeq[hdr.APIPath]
	w.pathSeq[hdr.APIPath] = seq + 1
	entryName := filepath.Join("k8shark-capture", "records", dir, fmt.Sprintf("%d.json.zst", seq))

	if err := writeBytes(w.zw, entryName, compressed); err != nil {
		return err
	}
	w.n++
	return nil
}

// WriteRecordRaw compresses data and writes it to the archive for the given
// apiPath.  It returns the seq number assigned to this record.
func (w *StreamWriter) WriteRecordRaw(apiPath string, data any) (int, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return 0, fmt.Errorf("marshalling record: %w", err)
	}
	compressed, err := zstdCompress(b)
	if err != nil {
		return 0, fmt.Errorf("compressing record: %w", err)
	}
	dir := pathDir(apiPath)

	w.mu.Lock()
	defer w.mu.Unlock()

	seq := w.pathSeq[apiPath]
	w.pathSeq[apiPath] = seq + 1
	entryName := filepath.Join("k8shark-capture", "records", dir, fmt.Sprintf("%d.json.zst", seq))
	if err := writeBytes(w.zw, entryName, compressed); err != nil {
		return 0, err
	}
	w.n++
	return seq, nil
}

// Finish writes metadata.json, index.json, and watch-index.json, then closes.
func (w *StreamWriter) Finish(meta, index, watchIndex any) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// metadata.json stored uncompressed for fast header reads.
	if meta != nil {
		b, err := json.MarshalIndent(meta, "", "  ")
		if err != nil {
			return fmt.Errorf("marshalling metadata: %w", err)
		}
		if err := writeBytes(w.zw, "k8shark-capture/metadata.json", b); err != nil {
			return err
		}
	}
	if index != nil {
		if err := writeJSONZstd(w.zw, "k8shark-capture/index.json.zst", index); err != nil {
			return err
		}
	}
	if watchIndex != nil {
		if err := writeJSONZstd(w.zw, "k8shark-capture/watch-index.json.zst", watchIndex); err != nil {
			return err
		}
	}
	if err := w.zw.Close(); err != nil {
		return fmt.Errorf("closing zip: %w", err)
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
func (w *NDJSONWriter) Finish(_, _, _ any) error { return nil }

// RecordCount returns the number of records written so far.
func (w *NDJSONWriter) RecordCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// Archive provides random-access reads into a k8shark ZIP+Zstd capture archive.
// It does NOT require extraction to disk.
type Archive struct {
	zr     *zip.ReadCloser
	byName map[string]*zip.File // ZIP entry name → file handle
	size   int64
	path   string
	readN  atomic.Int64 // for diagnostics
}

// Open opens a k8shark archive for reading. The caller must call Close() when done.
func Open(archivePath string) (*Archive, error) {
	fi, err := os.Stat(archivePath)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", archivePath, err)
	}
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("opening zip archive %q: %w", archivePath, err)
	}
	byName := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		byName[f.Name] = f
	}
	return &Archive{zr: zr, byName: byName, size: fi.Size(), path: archivePath}, nil
}

// Close releases the underlying file handle.
func (a *Archive) Close() error { return a.zr.Close() }

// Path returns the archive file path.
func (a *Archive) Path() string { return a.path }

// Size returns the on-disk size of the archive in bytes.
func (a *Archive) Size() int64 { return a.size }

// ReadMetadata reads and parses metadata.json from the archive.
func (a *Archive) ReadMetadata(v any) error {
	data, err := a.readRaw("k8shark-capture/metadata.json")
	if err != nil {
		return fmt.Errorf("reading metadata.json: %w", err)
	}
	return json.Unmarshal(data, v)
}

// ReadIndex reads and parses the Zstd-compressed index.json.zst.
func (a *Archive) ReadIndex(v any) error {
	data, err := a.readZstd("k8shark-capture/index.json.zst")
	if err != nil {
		return fmt.Errorf("reading index.json.zst: %w", err)
	}
	return json.Unmarshal(data, v)
}

// ReadWatchIndex reads and parses watch-index.json.zst, if present.
// Returns (false, nil) when the archive has no watch index.
func (a *Archive) ReadWatchIndex(v any) (bool, error) {
	const name = "k8shark-capture/watch-index.json.zst"
	if _, ok := a.byName[name]; !ok {
		return false, nil
	}
	data, err := a.readZstd(name)
	if err != nil {
		return false, fmt.Errorf("reading watch-index.json.zst: %w", err)
	}
	return true, json.Unmarshal(data, v)
}

// ReadRecord reads the record at sequence seq under apiPath.
// seq is 0-based and matches the order records were written for that path.
func (a *Archive) ReadRecord(apiPath string, seq int) ([]byte, error) {
	dir := pathDir(apiPath)
	name := filepath.ToSlash(filepath.Join("k8shark-capture", "records", dir, fmt.Sprintf("%d.json.zst", seq)))
	data, err := a.readZstd(name)
	if err != nil {
		return nil, fmt.Errorf("reading record path=%s seq=%d: %w", apiPath, seq, err)
	}
	a.readN.Add(1)
	return data, nil
}

// RecordsForPath returns all record bytes in capture order for apiPath.
// Stops at the first missing sequence number.
func (a *Archive) RecordsForPath(apiPath string) ([][]byte, error) {
	dir := pathDir(apiPath)
	var out [][]byte
	for seq := 0; ; seq++ {
		name := filepath.ToSlash(filepath.Join("k8shark-capture", "records", dir, fmt.Sprintf("%d.json.zst", seq)))
		if _, ok := a.byName[name]; !ok {
			break
		}
		data, err := a.readZstd(name)
		if err != nil {
			return nil, err
		}
		out = append(out, data)
	}
	return out, nil
}

// PathDirs returns all distinct path-hash directories found under records/.
// This allows enumeration without the index.
func (a *Archive) PathDirs() []string {
	seen := make(map[string]bool)
	for name := range a.byName {
		// prefix: k8shark-capture/records/<dir>/
		if !strings.HasPrefix(name, "k8shark-capture/records/") {
			continue
		}
		rest := strings.TrimPrefix(name, "k8shark-capture/records/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 && parts[0] != "" {
			seen[parts[0]] = true
		}
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	return dirs
}

// PathDir exposes the pathDir function for use by other packages (e.g. capture engine).
func PathDir(apiPath string) string { return pathDir(apiPath) }

// readRaw reads an uncompressed ZIP entry.
func (a *Archive) readRaw(name string) ([]byte, error) {
	zf, ok := a.byName[name]
	if !ok {
		return nil, fmt.Errorf("entry %q not found in archive", name)
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// readZstd reads a Zstd-compressed ZIP entry and returns decompressed bytes.
func (a *Archive) readZstd(name string) ([]byte, error) {
	compressed, err := a.readRaw(name)
	if err != nil {
		return nil, err
	}
	return zstdDecompress(compressed)
}

// ---- helpers used by StreamWriter ----

func writeBytes(zw *zip.Writer, name string, data []byte) error {
	w, err := zw.Create(name)
	if err != nil {
		return fmt.Errorf("creating zip entry %s: %w", name, err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("writing zip entry %s: %w", name, err)
	}
	return nil
}

func writeJSONZstd(zw *zip.Writer, name string, v any) error {
	plain, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling %s: %w", name, err)
	}
	compressed, err := zstdCompress(plain)
	if err != nil {
		return fmt.Errorf("compressing %s: %w", name, err)
	}
	return writeBytes(zw, name, compressed)
}
