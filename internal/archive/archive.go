package archive

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

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

	type recorder interface {
		GetID() string
		JSON() ([]byte, error)
	}

	// records is []*capture.Record — iterate via reflection-free type assertion
	type record interface {
		GetID() string
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
