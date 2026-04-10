package capture

import (
	"encoding/json"
	"time"
)

// Record holds one polled API response.
type Record struct {
	ID           string          `json:"id"`
	CapturedAt   time.Time       `json:"captured_at"`
	APIPath      string          `json:"api_path"`
	HTTPMethod   string          `json:"http_method"`
	QueryParams  map[string]string `json:"query_params,omitempty"`
	ResponseCode int             `json:"response_code"`
	ResponseBody json.RawMessage `json:"response_body"`
}

// CaptureMetadata is written as metadata.json inside the archive.
type CaptureMetadata struct {
	CaptureID      string    `json:"capture_id"`
	CapturedAt     time.Time `json:"captured_at"`
	CapturedUntil  time.Time `json:"captured_until"`
	KubernetesVersion string `json:"kubernetes_version"`
	ServerAddress  string    `json:"server_address"`
	RecordCount    int       `json:"record_count"`
}

// IndexEntry maps an API path to the ordered list of record IDs captured for it.
type IndexEntry struct {
	APIPath   string    `json:"api_path"`
	RecordIDs []string  `json:"record_ids"`
	Times     []time.Time `json:"times"`
}

// Index is the top-level index.json written inside the archive.
// Key is the canonical API path.
type Index map[string]*IndexEntry
