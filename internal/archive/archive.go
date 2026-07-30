package archive

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"

	"github.com/phenixblue/k8shark/internal/archive/format"
)

// epochModTime is a fixed, valid timestamp used for every ZIP entry so archives
// are deterministic. The ZIP/DOS time epoch is 1980-01-01; we use noon UTC so
// the date stays on 1980-01-01 across common timezones (a zero time renders as
// the invalid "00-00-1980").
var epochModTime = time.Date(1980, 1, 1, 12, 0, 0, 0, time.UTC)

// RecordSink accepts individual capture records as they arrive.
type RecordSink interface {
	// WriteRecord writes rec and returns the sequence number assigned to it
	// within its record's api_path — the same numbering readers use to
	// address it later (see Archive.ReadRecord). The seq is only meaningful
	// on success; callers must not use it when err is non-nil.
	WriteRecord(rec *format.Record) (int, error)
	// Finish writes metadata.json, index.json, and (when watchIndex is non-nil)
	// watch-index.json, then closes the archive.
	Finish(meta, index, watchIndex any) error
	RecordCount() int
	// UncompressedBytes returns the running total of record JSON bytes written
	// (before compression).
	UncompressedBytes() int64
}

// pathDir returns a short, filesystem-safe directory name for an API path.
// We use the first 16 hex chars of SHA-256 to avoid path-length issues.
func pathDir(apiPath string) string {
	sum := sha256.Sum256([]byte(apiPath))
	return fmt.Sprintf("%x", sum[:8])
}

// quotePath wraps p in double quotes for a human-readable error message,
// without treating it as a Go string literal the way %q does — %q escapes
// every "\" as "\\", which turns an ordinary Windows path (built entirely of
// backslashes) into visibly doubled separators in every error message. Only
// the characters that would otherwise make the quoted output ambiguous or
// multi-line (a literal '"', '\n', or '\r') are escaped; a backslash is
// never touched, so the path renders exactly as passed in on any OS.
func quotePath(p string) string {
	var b strings.Builder
	b.Grow(len(p) + 2)
	b.WriteByte('"')
	for _, r := range p {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
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
	return readAllLimited(dec, maxEntryBytes)
}

// maxEntryBytes bounds how much data a single archive entry may expand to
// when decompressed or read. A .kshrk archive is designed to be handed
// between organizations, so every read path must defend against a crafted
// entry whose declared/compressed size is tiny but whose true decompressed
// size is enormous (a "zip/zstd bomb") — mirroring internal/k8sbin's
// maxExtractedBytes guard against the same class of untrusted input. Applied
// per entry (a record, the index, metadata.json, ...) rather than as a
// cumulative budget across the archive's lifetime, since a long-running
// replay server legitimately reads many records over its lifetime and a
// cumulative cap would eventually reject that regardless of any bomb. High
// enough that legitimate captures (hundreds of MB total, far smaller per
// entry) still open.
const maxEntryBytes = 512 << 20 // 512 MiB

// readAllLimited reads all of r, returning an error instead of allocating
// unbounded memory if more than limit bytes would be read.
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("exceeds %d bytes", limit)
	}
	return data, nil
}

// StreamWriter streams each record directly into a .kshrk (ZIP+Zstd) archive.
// metadata.json, index.json, and watch-index.json are written by Finish().
// Thread-safe.
type StreamWriter struct {
	mu      sync.Mutex
	f       *os.File
	ageW    io.WriteCloser // non-nil when encrypting; wraps f, must close before f
	zw      *zip.Writer
	n       int
	bytes   int64          // running total of uncompressed record JSON bytes
	pathSeq map[string]int // apiPath → next seq number for that path's directory
	closed  bool           // set once Finish or Abort has run the close sequence
}

// NewStreamWriter creates a new StreamWriter writing to outputPath.
func NewStreamWriter(outputPath string) (*StreamWriter, error) {
	return newStreamWriter(outputPath, nil)
}

// NewEncryptedStreamWriter creates a new StreamWriter that encrypts
// outputPath as a single age envelope around the ZIP archive: the ZIP writer
// writes into age.Encrypt's stream instead of directly into the file, so
// WriteRecord/WriteRecordRaw/Finish are unchanged and nothing is buffered in
// memory or spilled to a plaintext temp file. recipients must be non-empty;
// per the age spec, a passphrase (ScryptRecipient) recipient must be the only
// recipient for the file — callers must not mix it with other recipient
// types.
func NewEncryptedStreamWriter(outputPath string, recipients []age.Recipient) (*StreamWriter, error) {
	if len(recipients) == 0 {
		return nil, fmt.Errorf("archive encryption requires at least one recipient")
	}
	return newStreamWriter(outputPath, recipients)
}

func newStreamWriter(outputPath string, recipients []age.Recipient) (*StreamWriter, error) {
	f, err := os.Create(outputPath)
	if err != nil {
		return nil, fmt.Errorf("creating output file %s: %w", quotePath(outputPath), err)
	}
	sw := &StreamWriter{f: f, pathSeq: make(map[string]int)}
	var dst io.Writer = f
	if len(recipients) > 0 {
		ageW, err := age.Encrypt(f, recipients...)
		if err != nil {
			// age.Encrypt failed before any archive bytes were written, so
			// remove the just-created empty file rather than leaving a bogus
			// zero-length archive behind for callers to trip over.
			f.Close()
			_ = os.Remove(outputPath)
			return nil, fmt.Errorf("setting up archive encryption: %w", err)
		}
		sw.ageW = ageW
		dst = ageW
	}
	sw.zw = zip.NewWriter(dst)
	return sw, nil
}

// WriteRecord marshals rec to JSON, Zstd-compresses it, and appends it
// to the ZIP archive under records/<pathDir(apiPath)>/<seq>.json.zst.
// The record must have both ID and APIPath set. It returns the seq assigned
// to the record, which is only valid when err is nil.
func (w *StreamWriter) WriteRecord(rec *format.Record) (int, error) {
	if rec == nil || rec.ID == "" || rec.APIPath == "" {
		return 0, fmt.Errorf("record missing id or api_path field")
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return 0, fmt.Errorf("marshalling record: %w", err)
	}

	compressed, err := zstdCompress(data)
	if err != nil {
		return 0, fmt.Errorf("compressing record %s: %w", rec.ID, err)
	}

	dir := pathDir(rec.APIPath)

	w.mu.Lock()
	defer w.mu.Unlock()

	seq := w.pathSeq[rec.APIPath]
	entryName := path.Join("k8shark-capture", "records", dir, fmt.Sprintf("%d.json.zst", seq))

	if err := writeBytes(w.zw, entryName, compressed); err != nil {
		return 0, err
	}
	w.pathSeq[rec.APIPath] = seq + 1
	w.n++
	w.bytes += int64(len(data))
	return seq, nil
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
	entryName := path.Join("k8shark-capture", "records", dir, fmt.Sprintf("%d.json.zst", seq))
	if err := writeBytes(w.zw, entryName, compressed); err != nil {
		return 0, err
	}
	w.n++
	w.bytes += int64(len(b))
	return seq, nil
}

// Finish writes metadata.json, index.json, and watch-index.json, then closes.
func (w *StreamWriter) Finish(meta, index, watchIndex any) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return fmt.Errorf("archive writer already closed")
	}

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
	w.closed = true
	// Close the layers from outermost to innermost, but always attempt every
	// close so a failure partway through can't leak the file descriptor: zw
	// wraps ageW (when encrypting) which wraps f. Return the first error.
	var firstErr error
	if err := w.zw.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("closing zip: %w", err)
	}
	if w.ageW != nil {
		if err := w.ageW.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("closing archive encryption stream: %w", err)
		}
	}
	if err := w.f.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("closing output file: %w", err)
	}
	return firstErr
}

// Abort releases the writer's underlying handles without finalizing the
// archive. It is a no-op once Finish (or a prior Abort) has run, so it is safe
// to defer as error-path cleanup alongside a normal Finish. Closing the file
// handle also lets the caller remove or rename a partially-written output
// (e.g. capture's ".redacting" temp) on platforms where an open handle blocks
// that (Windows).
func (w *StreamWriter) Abort() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true
	// Best-effort close of each layer. zw.Close may error (e.g. nothing
	// written yet); we still close the remaining layers and only surface the
	// file-close error, which is the one that matters for handle release.
	_ = w.zw.Close()
	if w.ageW != nil {
		_ = w.ageW.Close()
	}
	return w.f.Close()
}

// RecordCount returns the number of records written so far.
func (w *StreamWriter) RecordCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// UncompressedBytes returns the total uncompressed record JSON bytes written.
func (w *StreamWriter) UncompressedBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes
}

// NDJSONWriter writes each record as a newline-delimited JSON object to an
// io.Writer (typically os.Stdout). Finish is a no-op. Thread-safe.
type NDJSONWriter struct {
	mu      sync.Mutex
	w       io.Writer
	enc     *json.Encoder
	n       int
	bytes   int64
	pathSeq map[string]int // apiPath → next seq, mirrors StreamWriter's numbering
}

// NewNDJSONWriter creates an NDJSONWriter writing to w.
func NewNDJSONWriter(w io.Writer) *NDJSONWriter {
	return &NDJSONWriter{w: w, enc: json.NewEncoder(w), pathSeq: make(map[string]int)}
}

// WriteRecord encodes rec as a single JSON line and returns the seq assigned
// to it within its api_path, matching StreamWriter's numbering even though
// NDJSON output has no index to look it up by later.
func (w *NDJSONWriter) WriteRecord(rec *format.Record) (int, error) {
	if rec == nil {
		return 0, fmt.Errorf("record missing id or api_path field")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	b, err := json.Marshal(rec)
	if err != nil {
		return 0, fmt.Errorf("marshalling record: %w", err)
	}

	var seq int
	if rec.APIPath != "" {
		seq = w.pathSeq[rec.APIPath]
	}

	if err := w.enc.Encode(rec); err != nil {
		return 0, err
	}
	// Only advance state once Encode has actually succeeded — mirrors seq's
	// own only-meaningful-on-success rule, so a broken pipe mid-stream can't
	// over-report bytes for a record that was never written.
	w.bytes += int64(len(b))
	if rec.APIPath != "" {
		w.pathSeq[rec.APIPath] = seq + 1
	}
	w.n++
	return seq, nil
}

// Finish is a no-op for NDJSONWriter.
func (w *NDJSONWriter) Finish(_, _, _ any) error { return nil }

// RecordCount returns the number of records written so far.
func (w *NDJSONWriter) RecordCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// UncompressedBytes returns the total record JSON bytes written.
func (w *NDJSONWriter) UncompressedBytes() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes
}

// Archive provides random-access reads into a k8shark ZIP+Zstd capture archive.
// It does NOT require extraction to disk. The archive may itself be wrapped
// in a single age encryption envelope (see NewEncryptedStreamWriter); in that
// case age.DecryptReaderAt supplies the io.ReaderAt that zip.NewReader reads
// from directly, so an encrypted archive gets the same random-access reads
// as a plaintext one, with nothing ever decrypted to a temp file.
type Archive struct {
	zr     *zip.Reader
	closer io.Closer
	byName map[string]*zip.File // ZIP entry name → file handle
	size   int64
	path   string
	readN  atomic.Int64 // for diagnostics
	meta   format.CaptureMetadata
}

// Open opens a k8shark archive for reading. The caller must call Close() when
// done. If the archive is age-encrypted, Open returns a clear error instead
// of a raw "not a valid zip file" failure; use OpenWithIdentities with key
// material to open it. Open also reads and validates metadata.json against
// format.CheckFormatVersion, so every caller gets the format-version gate for
// free instead of needing to call it themselves after ReadMetadata.
func Open(archivePath string) (*Archive, error) {
	return openArchive(archivePath, nil)
}

// OpenWithIdentities opens a k8shark archive for reading, decrypting it first
// if it is age-encrypted. identities is ignored for a plaintext archive, so
// callers can use this unconditionally regardless of whether the archive
// turns out to be encrypted.
func OpenWithIdentities(archivePath string, identities []age.Identity) (*Archive, error) {
	return openArchive(archivePath, identities)
}

// wrapZipFormatErr gives zip.NewReader's ErrFormat an actionable diagnosis:
// Go's archive/zip returns that identical error for a truncated capture
// (missing central directory), an empty file, and garbage bytes — it can't
// distinguish the causes — so name both plausible ones instead of surfacing
// the raw "not a valid zip file". Applies equally to a plaintext archive and
// the plaintext zip data recovered from decrypting an age-encrypted one,
// since an interrupted encrypted capture truncates the same underlying zip
// stream before it's ever encrypted.
func wrapZipFormatErr(archivePath string, err error) error {
	if errors.Is(err, zip.ErrFormat) {
		return fmt.Errorf("archive %s is corrupt or incomplete: not a valid zip file — this can happen if a capture was interrupted (e.g. Ctrl+C) or the file was truncated in transfer: %w", quotePath(archivePath), err)
	}
	return fmt.Errorf("opening zip archive %s: %w", quotePath(archivePath), err)
}

func openArchive(archivePath string, identities []age.Identity) (*Archive, error) {
	fi, err := os.Stat(archivePath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", quotePath(archivePath), err)
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("opening archive %s: %w", quotePath(archivePath), err)
	}

	encrypted, err := isAgeEncrypted(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("reading %s: %w", quotePath(archivePath), err)
	}

	var zr *zip.Reader
	if !encrypted {
		zr, err = zip.NewReader(f, fi.Size())
		if err != nil {
			f.Close()
			return nil, wrapZipFormatErr(archivePath, err)
		}
	} else {
		if len(identities) == 0 {
			f.Close()
			return nil, fmt.Errorf("archive %s is encrypted: supply a decryption key", quotePath(archivePath))
		}
		ra, plainSize, err := age.DecryptReaderAt(f, fi.Size(), identities...)
		if err != nil {
			f.Close()
			if isNoIdentityMatch(err) {
				return nil, fmt.Errorf("failed to decrypt archive %s: incorrect passphrase or key", quotePath(archivePath))
			}
			return nil, fmt.Errorf("decrypting archive %s: %w", quotePath(archivePath), err)
		}
		zr, err = zip.NewReader(ra, plainSize)
		if err != nil {
			f.Close()
			return nil, wrapZipFormatErr(archivePath, err)
		}
	}

	byName := make(map[string]*zip.File, len(zr.File))
	for _, zf := range zr.File {
		byName[zf.Name] = zf
	}
	ar := &Archive{zr: zr, closer: f, byName: byName, size: fi.Size(), path: archivePath}

	metaData, err := ar.readRaw("k8shark-capture/metadata.json")
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("reading metadata.json: %w", err)
	}
	if err := json.Unmarshal(metaData, &ar.meta); err != nil {
		f.Close()
		return nil, fmt.Errorf("parsing metadata.json in archive %s: %w", quotePath(archivePath), err)
	}
	if err := format.CheckFormatVersion(ar.meta); err != nil {
		f.Close()
		return nil, err
	}

	return ar, nil
}

// Close releases the underlying file handle.
func (a *Archive) Close() error { return a.closer.Close() }

// Path returns the archive file path.
func (a *Archive) Path() string { return a.path }

// Size returns the on-disk size of the archive in bytes.
func (a *Archive) Size() int64 { return a.size }

// ReadMetadata returns the archive's metadata.json, already parsed and
// validated against format.CheckFormatVersion by Open — Open cannot have
// succeeded otherwise, so this never actually fails, but it still returns an
// error for symmetry with ReadIndex/ReadWatchIndex and to leave room for a
// future on-demand read without a signature change.
func (a *Archive) ReadMetadata() (format.CaptureMetadata, error) {
	return a.meta, nil
}

// ReadIndex reads and parses the Zstd-compressed index.json.zst.
func (a *Archive) ReadIndex() (format.Index, error) {
	data, err := a.readZstd("k8shark-capture/index.json.zst")
	if err != nil {
		return nil, fmt.Errorf("reading index.json.zst: %w", err)
	}
	var idx format.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("parsing index.json.zst in archive %s: %w", quotePath(a.path), err)
	}
	return idx, nil
}

// ReadWatchIndex reads and parses watch-index.json.zst, if present.
// Returns (nil, false, nil) when the archive has no watch index.
func (a *Archive) ReadWatchIndex() (format.WatchIndex, bool, error) {
	const name = "k8shark-capture/watch-index.json.zst"
	if _, ok := a.byName[name]; !ok {
		return nil, false, nil
	}
	data, err := a.readZstd(name)
	if err != nil {
		return nil, true, fmt.Errorf("reading watch-index.json.zst: %w", err)
	}
	var wi format.WatchIndex
	if err := json.Unmarshal(data, &wi); err != nil {
		return nil, true, fmt.Errorf("parsing watch-index.json.zst in archive %s: %w", quotePath(a.path), err)
	}
	// The writer always emits at least "{}" (never a bare "null") for
	// watch-index.json.zst, so a top-level JSON null here — which unmarshals
	// to a nil map without error — is corrupt, not simply an empty index.
	if wi == nil {
		return nil, true, fmt.Errorf("parsing watch-index.json.zst in archive %s: top-level null", quotePath(a.path))
	}
	// A well-formed watch-index.json never has a null entry for a path — the
	// writer only ever inserts a populated *WatchIndexEntry. A null entry here
	// means the JSON is structurally valid but semantically corrupt, and every
	// reader dereferences the entry (e.g. entry.Seqs) without a nil check.
	for apiPath, entry := range wi {
		if entry == nil {
			return nil, true, fmt.Errorf("parsing watch-index.json.zst in archive %s: null entry for path %q", quotePath(a.path), apiPath)
		}
	}
	return wi, true, nil
}

// ReadRecord reads the record at sequence seq under apiPath.
// seq is 0-based and matches the order records were written for that path.
func (a *Archive) ReadRecord(apiPath string, seq int) ([]byte, error) {
	dir := pathDir(apiPath)
	name := path.Join("k8shark-capture", "records", dir, fmt.Sprintf("%d.json.zst", seq))
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
		name := path.Join("k8shark-capture", "records", dir, fmt.Sprintf("%d.json.zst", seq))
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

// readRaw reads an uncompressed ZIP entry. A ZIP entry may itself use Deflate
// (see writeBytes's doc comment — the method is not part of the format
// contract, so older archives may still use it), which zf.Open() decompresses
// on the fly; readAllLimited bounds that against a deflate bomb the same way
// zstdDecompress bounds a zstd one.
func (a *Archive) readRaw(name string) ([]byte, error) {
	zf, ok := a.byName[name]
	if !ok {
		return nil, fmt.Errorf("entry %q not found in archive %s", name, quotePath(a.path))
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, fmt.Errorf("opening entry %q in archive %s: %w", name, quotePath(a.path), err)
	}
	defer rc.Close()
	data, err := readAllLimited(rc, maxEntryBytes)
	if err != nil {
		return nil, fmt.Errorf("reading entry %q in archive %s: %w", name, quotePath(a.path), err)
	}
	return data, nil
}

// readZstd reads a Zstd-compressed ZIP entry and returns decompressed bytes.
func (a *Archive) readZstd(name string) ([]byte, error) {
	compressed, err := a.readRaw(name)
	if err != nil {
		return nil, err
	}
	data, err := zstdDecompress(compressed)
	if err != nil {
		return nil, fmt.Errorf("decompressing entry %q in archive %s: %w", name, quotePath(a.path), err)
	}
	return data, nil
}

// ---- helpers used by StreamWriter ----

// writeBytes adds a ZIP entry using the Store method (no ZIP-level
// compression). Record/index/watch payloads are already Zstd-compressed, so
// running them through the ZIP writer's default Deflate would burn CPU for no
// size benefit; metadata.json is small and kept uncompressed for fast header
// reads. The ZIP method is an implementation detail, not part of the archive
// format contract — readers handle any method, so older Deflate archives still
// open. ZIP still records a per-entry CRC32 for integrity.
func writeBytes(zw *zip.Writer, name string, data []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store, Modified: epochModTime})
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
