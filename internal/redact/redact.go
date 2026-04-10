package redact

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// redactedB64 is "REDACTED" base64-encoded.  Kubernetes Secret data values are
// base64 strings, so we replace with another valid base64 string.
var redactedB64 = base64.StdEncoding.EncodeToString([]byte("REDACTED"))

// Archive reads srcPath, redacts all Secret records, and writes to dstPath.
// allowList is a set of "namespace/name" keys whose data is preserved unchanged.
func Archive(srcPath, dstPath string, allowList map[string]bool) (int, error) {
	// Extract the source archive to a temp dir.
	tmpDir, err := os.MkdirTemp("", "k8shark-redact-*")
	if err != nil {
		return 0, fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := archive.Open(srcPath, tmpDir); err != nil {
		return 0, fmt.Errorf("opening archive: %w", err)
	}

	// Load metadata and index.
	metaBytes, err := os.ReadFile(filepath.Join(tmpDir, "k8shark-capture", "metadata.json"))
	if err != nil {
		return 0, fmt.Errorf("reading metadata: %w", err)
	}
	var meta capture.CaptureMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return 0, fmt.Errorf("parsing metadata: %w", err)
	}

	idxBytes, err := os.ReadFile(filepath.Join(tmpDir, "k8shark-capture", "index.json"))
	if err != nil {
		return 0, fmt.Errorf("reading index: %w", err)
	}
	var idx capture.Index
	if err := json.Unmarshal(idxBytes, &idx); err != nil {
		return 0, fmt.Errorf("parsing index: %w", err)
	}

	// Walk all records, collect unique IDs, and redact as needed.
	seen := map[string]bool{}
	for _, entry := range idx {
		for _, id := range entry.RecordIDs {
			seen[id] = true
		}
	}

	var records []*capture.Record
	redactedCount := 0

	for id := range seen {
		recPath := filepath.Join(tmpDir, "k8shark-capture", "records", id+".json")
		data, err := os.ReadFile(recPath)
		if err != nil {
			return 0, fmt.Errorf("reading record %s: %w", id, err)
		}
		var rec capture.Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return 0, fmt.Errorf("parsing record %s: %w", id, err)
		}

		redacted, err := redactRecord(&rec, allowList)
		if err != nil {
			return 0, fmt.Errorf("redacting record %s: %w", id, err)
		}
		if redacted {
			redactedCount++
		}
		records = append(records, &rec)
	}

	if err := archive.Write(dstPath, &meta, records, idx); err != nil {
		return 0, fmt.Errorf("writing redacted archive: %w", err)
	}

	return redactedCount, nil
}

// redactRecord modifies rec in-place if it is a Secret not in the allowList.
// Returns true if the record was redacted.
func redactRecord(rec *capture.Record, allowList map[string]bool) (bool, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
		// Not JSON (e.g. Table format) — leave as-is.
		return false, nil
	}

	kindRaw, ok := obj["kind"]
	if !ok {
		return false, nil
	}
	var kind string
	if err := json.Unmarshal(kindRaw, &kind); err != nil || kind != "Secret" {
		return false, nil
	}

	// Check allowlist.
	metaKey := secretKey(obj)
	if allowList[metaKey] {
		return false, nil
	}

	modified := false

	if dataRaw, ok := obj["data"]; ok {
		var dataMap map[string]string
		if err := json.Unmarshal(dataRaw, &dataMap); err == nil {
			for k := range dataMap {
				dataMap[k] = redactedB64
			}
			newData, _ := json.Marshal(dataMap)
			obj["data"] = newData
			modified = true
		}
	}

	if sdRaw, ok := obj["stringData"]; ok {
		var sdMap map[string]string
		if err := json.Unmarshal(sdRaw, &sdMap); err == nil {
			for k := range sdMap {
				sdMap[k] = "REDACTED"
			}
			newSD, _ := json.Marshal(sdMap)
			obj["stringData"] = newSD
			modified = true
		}
	}

	if modified {
		newBody, err := json.Marshal(obj)
		if err != nil {
			return false, fmt.Errorf("re-marshalling secret: %w", err)
		}
		rec.ResponseBody = newBody
	}

	return modified, nil
}

// secretKey returns "namespace/name" for a Secret object map, for allowlist lookup.
func secretKey(obj map[string]json.RawMessage) string {
	metaRaw, ok := obj["metadata"]
	if !ok {
		return ""
	}
	var meta struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return ""
	}
	if meta.Namespace == "" {
		return meta.Name
	}
	return strings.Join([]string{meta.Namespace, meta.Name}, "/")
}
