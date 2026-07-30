package capture

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
)

// storeRecord persists body at indexKey with optional dedup. Used both by the
// per-resource doFetch path and by the cluster-wide demux path, which
// constructs synthetic per-namespace bodies and writes them under per-
// namespace index keys.
func (e *Engine) storeRecord(indexKey string, body []byte, statusCode int, dedupEnabled bool) {
	if len(body) == 0 {
		return
	}
	if dedupEnabled {
		h := sha256.Sum256(body)
		e.mu.Lock()
		prev, ok := e.lastHash[indexKey]
		if ok && prev == h {
			e.dedupSkipped++
			e.mu.Unlock()
			if e.verbose {
				fmt.Fprintf(os.Stdout, "  [dedup] %s unchanged; skipping write\n", indexKey)
			}
			return
		}
		e.lastHash[indexKey] = h
		e.mu.Unlock()
	}

	rec := &Record{
		ID:           uuid.New().String(),
		CapturedAt:   time.Now().UTC(),
		APIPath:      indexKey,
		HTTPMethod:   http.MethodGet,
		ResponseCode: statusCode,
		ResponseBody: json.RawMessage(body),
	}
	if e.sink == nil {
		return
	}
	seq, err := e.sink.WriteRecord(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  [warn] writing record %s: %v\n", indexKey, err)
		e.mu.Lock()
		if e.captureErr == nil {
			e.captureErr = fmt.Errorf("writing record %s: %w", indexKey, err)
		}
		if dedupEnabled {
			// The write never landed, so nothing was actually deduplicated
			// against — clear the hash so a later poll of the same
			// (unwritten) body isn't wrongly skipped as "unchanged" and a
			// transient write error can be retried on the next interval.
			delete(e.lastHash, indexKey)
		}
		e.mu.Unlock()
		return
	}
	itemCount := countListItems(body)
	e.mu.Lock()
	if _, ok := e.index[indexKey]; !ok {
		e.index[indexKey] = &IndexEntry{APIPath: indexKey}
	}
	e.index[indexKey].Seqs = append(e.index[indexKey].Seqs, seq)
	e.index[indexKey].Times = append(e.index[indexKey].Times, rec.CapturedAt)
	e.index[indexKey].Counts = append(e.index[indexKey].Counts, itemCount)
	e.mu.Unlock()
}

// countListItems peeks at a JSON response body and returns the number of
// top-level items in it. Recognizes both standard <Kind>List responses
// (items[]) and meta.k8s.io/v1 Table responses (rows[]). Returns 0 for
// non-list bodies (single objects, discovery, OpenAPI). Used to populate
// IndexEntry.Counts so the UI can show namespace card counts without
// loading each record body.
func countListItems(body []byte) int {
	var probe struct {
		Items []json.RawMessage `json:"items"`
		Rows  []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0
	}
	if probe.Items != nil {
		return len(probe.Items)
	}
	return len(probe.Rows)
}
