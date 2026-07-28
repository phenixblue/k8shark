// Package format holds the .kshrk on-disk schema types and format-version
// check. It is stdlib-only so internal/archive can depend on it directly
// without an import cycle (internal/capture, which owns the fuller capture
// pipeline, imports internal/archive — not the other way around).
package format

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
//
// Bumped to 2: index.json.zst and watch-index.json.zst moved from a bare
// top-level map to {"entries": {...}}, so the index can gain additional
// top-level fields later without another version bump (#219). Both shapes
// are accepted by this build's reader for the life of the 1.x line — see
// Index.UnmarshalJSON / WatchIndex.UnmarshalJSON.
const CurrentFormatVersion = 2

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
	ID           string          `json:"id"`
	CapturedAt   time.Time       `json:"captured_at"`
	APIPath      string          `json:"api_path"`
	EventType    string          `json:"event_type,omitempty"`
	HTTPMethod   string          `json:"http_method"`
	ResponseCode int             `json:"response_code"`
	ResponseBody json.RawMessage `json:"response_body"`
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
	// Encrypted is true when the archive was written as an encrypted (age)
	// envelope. It is informational only: encryption is detected structurally
	// by sniffing the file, not from this field (which lives inside the
	// ciphertext and so is only readable after decryption). Omitted for
	// plaintext archives.
	Encrypted bool `json:"encrypted,omitempty"`
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
// Key is the canonical API path. On disk (format version 2+) it's wrapped as
// {"entries": {...}} so the index can gain sibling top-level fields later
// without another format-version bump; UnmarshalJSON also accepts a
// version-1 archive's bare top-level map, so this build reads both shapes
// for the life of the 1.x line. The Go-level type stays a plain map — every
// existing caller (idx[apiPath], range idx, len(idx), ...) is unaffected.
type Index map[string]*IndexEntry

// indexWire is the version-2+ on-disk shape of Index.
type indexWire struct {
	Entries map[string]*IndexEntry `json:"entries"`
}

// MarshalJSON always writes the version-2+ wrapped shape, with "entries" as
// an object ({}) even for a nil Index — never null.
func (idx Index) MarshalJSON() ([]byte, error) {
	entries := map[string]*IndexEntry(idx)
	if entries == nil {
		entries = map[string]*IndexEntry{}
	}
	return json.Marshal(indexWire{Entries: entries})
}

// UnmarshalJSON accepts both the version-2+ wrapped shape ({"entries": {...}})
// and a version-1 archive's bare top-level map. The two are unambiguous:
// a real api_path key always starts with "/", never equals "entries". A null
// "entries" value normalizes to an empty map (an index with nothing in it);
// a null individual entry value is rejected outright rather than silently
// stored as a nil *IndexEntry, which would panic the first time a caller
// dereferences it — the version-1 bare-map path below can't hit this because
// unmarshaling JSON null into a struct value (not a pointer) is a no-op that
// leaves a zero-value IndexEntry, not a nil one.
func (idx *Index) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if entriesRaw, ok := raw["entries"]; ok {
		var entries map[string]*IndexEntry
		if err := json.Unmarshal(entriesRaw, &entries); err != nil {
			return fmt.Errorf("parsing index entries: %w", err)
		}
		if entries == nil {
			entries = map[string]*IndexEntry{}
		}
		for apiPath, entry := range entries {
			if entry == nil {
				return fmt.Errorf("index entry %q is null", apiPath)
			}
		}
		*idx = entries
		return nil
	}
	// Version-1 shape: a bare top-level map of apiPath -> *IndexEntry.
	// Unmarshal into a pointer (not a value) so a null entry surfaces as a
	// nil pointer to reject, rather than silently unmarshaling into a
	// zero-value IndexEntry — unmarshaling JSON null into a plain struct
	// value is a documented no-op, so a value target would accept it.
	entries := make(map[string]*IndexEntry, len(raw))
	for apiPath, entryRaw := range raw {
		var entry *IndexEntry
		if err := json.Unmarshal(entryRaw, &entry); err != nil {
			return fmt.Errorf("parsing index entry %q: %w", apiPath, err)
		}
		if entry == nil {
			return fmt.Errorf("index entry %q is null", apiPath)
		}
		entries[apiPath] = entry
	}
	*idx = entries
	return nil
}

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
// Key is the canonical API path. Only present in archives captured with
// watch: true. Wrapped on disk as {"entries": {...}} for the same reason and
// in the same version-2+/version-1-compatible way as Index — see its doc.
type WatchIndex map[string]*WatchIndexEntry

// watchIndexWire is the version-2+ on-disk shape of WatchIndex.
type watchIndexWire struct {
	Entries map[string]*WatchIndexEntry `json:"entries"`
}

// MarshalJSON always writes the version-2+ wrapped shape, with "entries" as
// an object ({}) even for a nil WatchIndex — never null.
func (wi WatchIndex) MarshalJSON() ([]byte, error) {
	entries := map[string]*WatchIndexEntry(wi)
	if entries == nil {
		entries = map[string]*WatchIndexEntry{}
	}
	return json.Marshal(watchIndexWire{Entries: entries})
}

// UnmarshalJSON accepts both the version-2+ wrapped shape and a version-1
// archive's bare top-level map — see Index.UnmarshalJSON for the same logic.
func (wi *WatchIndex) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if entriesRaw, ok := raw["entries"]; ok {
		var entries map[string]*WatchIndexEntry
		if err := json.Unmarshal(entriesRaw, &entries); err != nil {
			return fmt.Errorf("parsing watch-index entries: %w", err)
		}
		if entries == nil {
			entries = map[string]*WatchIndexEntry{}
		}
		for apiPath, entry := range entries {
			if entry == nil {
				return fmt.Errorf("watch-index entry %q is null", apiPath)
			}
		}
		*wi = entries
		return nil
	}
	// Version-1 shape: a bare top-level map of apiPath -> *WatchIndexEntry.
	// See Index.UnmarshalJSON's identical comment for why this unmarshals
	// into a pointer rather than a value.
	entries := make(map[string]*WatchIndexEntry, len(raw))
	for apiPath, entryRaw := range raw {
		var entry *WatchIndexEntry
		if err := json.Unmarshal(entryRaw, &entry); err != nil {
			return fmt.Errorf("parsing watch-index entry %q: %w", apiPath, err)
		}
		if entry == nil {
			return fmt.Errorf("watch-index entry %q is null", apiPath)
		}
		entries[apiPath] = entry
	}
	*wi = entries
	return nil
}
