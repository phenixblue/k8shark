package capture

import (
	"encoding/json"
	"time"
)

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
	CaptureID         string    `json:"capture_id"`
	CapturedAt        time.Time `json:"captured_at"`
	CapturedUntil     time.Time `json:"captured_until"`
	KubernetesVersion string    `json:"kubernetes_version"`
	ServerAddress     string    `json:"server_address"`
	RecordCount       int       `json:"record_count"`
	DeduplicatedCount int       `json:"deduplicated_count"`
}

// IndexEntry maps an API path to the ordered list of record sequence numbers.
// Seqs[i] is the 0-based sequence index of the i-th record for this path,
// matching the on-disk filename records/<pathDir>/<seq>.json.zst.
type IndexEntry struct {
	APIPath string      `json:"api_path"`
	Seqs    []int       `json:"seqs"`
	Times   []time.Time `json:"times"`
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
