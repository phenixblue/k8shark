package capture

import (
	"encoding/json"
	"fmt"
	"time"
)

// CurrentFormatVersion is the .kshrk archive schema version written by this
// build. It is bumped ONLY on a breaking, structurally-incompatible change.
// Additive, backward-compatible changes (new omitempty metadata fields, new
// optional archive entries) keep the same version. Archives written before this
// field existed report 0 and are treated as version 1 — they are structurally
// identical, the marker is simply the only addition.
const CurrentFormatVersion = 1

// CheckFormatVersion reports whether an archive can be read by this build.
// A version newer than this build understands is rejected so we never silently
// misread an incompatible layout. A zero version (pre-versioning archive) is
// compatible. A negative version is invalid — it only arises from a corrupt or
// tampered metadata.json, so it is rejected rather than rendered as "v-1".
func CheckFormatVersion(m CaptureMetadata) error {
	if m.FormatVersion < 0 {
		return fmt.Errorf("archive format version %d is invalid (corrupt metadata?)", m.FormatVersion)
	}
	if m.FormatVersion > CurrentFormatVersion {
		return fmt.Errorf("archive format version %d is newer than this kshrk supports (%d); upgrade kshrk to read it", m.FormatVersion, CurrentFormatVersion)
	}
	return nil
}

// Record holds one polled API response.
type Record struct {
	ID           string            `json:"id"`
	CapturedAt   time.Time         `json:"captured_at"`
	APIPath      string            `json:"api_path"`
	EventType    string            `json:"event_type,omitempty"`
	HTTPMethod   string            `json:"http_method"`
	QueryParams  map[string]string `json:"query_params,omitempty"`
	ResponseCode int               `json:"response_code"`
	ResponseBody json.RawMessage   `json:"response_body"`
}

// CaptureMetadata is written as metadata.json inside the archive.
type CaptureMetadata struct {
	// FormatVersion is the archive schema version (see CurrentFormatVersion).
	// Omitted/zero in pre-versioning archives, which are treated as version 1.
	FormatVersion     int       `json:"format_version,omitempty"`
	CaptureID         string    `json:"capture_id"`
	CapturedAt        time.Time `json:"captured_at"`
	CapturedUntil     time.Time `json:"captured_until"`
	KubernetesVersion string    `json:"kubernetes_version"`
	ServerAddress     string    `json:"server_address"`
	RecordCount       int       `json:"record_count"`
	DeduplicatedCount int       `json:"deduplicated_count"`
	// Capture configuration facts, recorded so the UI can describe how the
	// archive was produced. Omitted (zero) in archives captured before these
	// fields existed.
	AutoDiscovered    bool     `json:"auto_discovered,omitempty"`
	WatchEnabled      bool     `json:"watch_enabled,omitempty"`
	Intervals         []string `json:"intervals,omitempty"`
	UncompressedBytes int64    `json:"uncompressed_bytes,omitempty"`
	Redacted          bool     `json:"redacted,omitempty"`
	SecretsRedacted   int      `json:"secrets_redacted,omitempty"`
	FieldsRedacted    int      `json:"fields_redacted,omitempty"`
}

// IndexEntry maps an API path to the ordered list of record sequence numbers.
// Seqs[i] is the 0-based sequence index of the i-th record for this path,
// matching the on-disk filename records/<pathDir>/<seq>.json.zst.
type IndexEntry struct {
	APIPath string      `json:"api_path"`
	Seqs    []int       `json:"seqs"`
	Times   []time.Time `json:"times"`
	// Counts[i] is the number of top-level items in record i. Populated for
	// list-shaped responses (anything with an items[] field or rows[] for
	// Table responses); 0 for non-list records (single objects, discovery
	// documents, OpenAPI specs). Optional — older archives omit this field
	// and consumers must treat a nil/short Counts as "unknown" rather than 0.
	Counts []int `json:"counts,omitempty"`
}

// Index is the top-level index.json written inside the archive.
// Key is the canonical API path.
type Index map[string]*IndexEntry

// WatchIndexEntry maps an API path to the ordered watch events captured for it.
// Each watch event is a separate record with event_type ADDED, MODIFIED, or DELETED.
// EventTypes is kept in sync with Seqs and Times for fast filtering without
// reading every record file.
type WatchIndexEntry struct {
	APIPath    string      `json:"api_path"`
	Seqs       []int       `json:"seqs"`
	Times      []time.Time `json:"times"`
	EventTypes []string    `json:"event_types"`
}

// WatchIndex is the top-level watch-index.json written inside the archive.
// Key is the canonical API path. Only present in archives captured with watch: true.
type WatchIndex map[string]*WatchIndexEntry
