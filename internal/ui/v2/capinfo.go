package v2

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// CaptureInfo describes the capture archive for the overview "Capture details"
// card. It combines metadata.json with values derived from the index and the
// archive file on disk.
type CaptureInfo struct {
	CaptureID         string    `json:"capture_id"`
	ServerAddress     string    `json:"server_address,omitempty"`
	KubernetesVersion string    `json:"kubernetes_version,omitempty"`
	CapturedAt        time.Time `json:"captured_at"`
	CapturedUntil     time.Time `json:"captured_until"`
	DurationSeconds   int       `json:"duration_seconds"`
	RecordCount       int       `json:"record_count"`
	DeduplicatedCount int       `json:"deduplicated_count"`
	CompressedBytes   int64     `json:"compressed_bytes,omitempty"`
	UncompressedBytes int64     `json:"uncompressed_bytes,omitempty"`
	ResourcePaths     int       `json:"resource_paths"`
	ResourceTypes     int       `json:"resource_types"`
	Namespaces        int       `json:"namespaces"`
	WatchEvents       int       `json:"watch_events"`
	AutoDiscovered    bool      `json:"auto_discovered"`
	WatchEnabled      bool      `json:"watch_enabled"`
	Intervals         []string  `json:"intervals,omitempty"`
	Redacted          bool      `json:"redacted"`
	SecretsRedacted   int       `json:"secrets_redacted,omitempty"`
	FieldsRedacted    int       `json:"fields_redacted,omitempty"`
	ArchivePath       string    `json:"archive_path,omitempty"`
	// HasConfigMeta is false for archives captured before the config-fact
	// metadata fields existed; the UI then shows "not recorded" for them.
	HasConfigMeta bool `json:"has_config_meta"`
}

func (h *Handler) serveCaptureInfo(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	m := h.Store.Metadata
	info := CaptureInfo{
		CaptureID:         m.CaptureID,
		ServerAddress:     m.ServerAddress,
		KubernetesVersion: m.KubernetesVersion,
		CapturedAt:        m.CapturedAt,
		CapturedUntil:     m.CapturedUntil,
		RecordCount:       m.RecordCount,
		DeduplicatedCount: m.DeduplicatedCount,
		UncompressedBytes: m.UncompressedBytes,
		AutoDiscovered:    m.AutoDiscovered,
		WatchEnabled:      m.WatchEnabled,
		Intervals:         m.Intervals,
		Redacted:          m.Redacted,
		SecretsRedacted:   m.SecretsRedacted,
		FieldsRedacted:    m.FieldsRedacted,
		ArchivePath:       h.ArchivePath,
	}
	if m.CapturedUntil.After(m.CapturedAt) {
		info.DurationSeconds = int(m.CapturedUntil.Sub(m.CapturedAt).Round(time.Second).Seconds())
	}
	// New captures always record UncompressedBytes (every record has bytes);
	// its presence is a reliable signal that the config-fact fields are real.
	info.HasConfigMeta = m.UncompressedBytes > 0

	// Compressed archive size from disk.
	if h.ArchivePath != "" {
		if fi, err := os.Stat(h.ArchivePath); err == nil {
			info.CompressedBytes = fi.Size()
		}
	}

	// Index-derived counts (no body reads).
	info.ResourcePaths = len(h.Store.Index)
	types := map[string]bool{}
	for path, entry := range h.Store.Index {
		if entry == nil || len(entry.Seqs) == 0 || strings.Contains(path, "?") {
			continue
		}
		g, v, res, _ := parseAPIPath(path)
		if res != "" {
			types[g+"/"+v+"/"+res] = true
		}
	}
	info.ResourceTypes = len(types)
	info.Namespaces = len(h.Store.NamespaceItemCountsAt(time.Time{}))
	for _, wi := range h.Store.WatchIndex {
		if wi != nil {
			info.WatchEvents += len(wi.Times)
		}
	}

	writeJSON(w, http.StatusOK, info)
}
