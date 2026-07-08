package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/transitions"
)

// Replay resourceVersion (RV) scheme
// ----------------------------------
// For informer-grade correctness, replay assigns every object a monotonic RV
// derived from a single time-sorted event timeline per watch path:
//
//   - rv(event i) = i + 1            (strictly increasing, 1-based)
//   - rvAsOf(T)   = count(t <= T)    (the RV of the state as-of clock time T)
//
// The LIST served at clock T returns metadata.resourceVersion = rvAsOf(T); a
// WATCH streams events with rv > the requested RV, each carrying its own rv on
// the object. Because rv is a strict sequence (not a raw timestamp), events that
// share a timestamp — common for poll-only captures whose diffs all carry the
// snapshot time — still get distinct, ordered RVs, so a reconnecting client
// resumes cleanly with no dropped or duplicated events.

// replayRVBase is the smallest resourceVersion the scheme emits. Starting at 1
// (rather than 0) keeps the window-start / empty-timeline RV non-zero, which
// clients require (RV "0" has special "unset" meaning to client-go).
const replayRVBase = 1

// watchIndexPaths returns the capture watch-index paths feeding a watch on
// watchPath: the exact path if captured, else (for a cluster-wide path) the
// per-namespace demultiplexed paths. The result is sorted for determinism.
func (s *CaptureStore) watchIndexPaths(watchPath string) []string {
	if _, ok := s.WatchIndex[watchPath]; ok {
		return []string{watchPath}
	}
	prefix, suffix, ok := clusterWideChildPrefix(watchPath)
	if !ok {
		return nil
	}
	var out []string
	for p := range s.WatchIndex {
		if strings.HasPrefix(p, prefix) && strings.HasSuffix(p, suffix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// snapshotPaths returns the capture snapshot (index) paths feeding a watch on
// watchPath for poll-only inference. Same resolution as watchIndexPaths but over
// the snapshot index, skipping Table-format keys.
func (s *CaptureStore) snapshotPaths(watchPath string) []string {
	if _, ok := s.Index[watchPath]; ok {
		return []string{watchPath}
	}
	prefix, suffix, ok := clusterWideChildPrefix(watchPath)
	if !ok {
		return nil
	}
	var out []string
	for p := range s.Index {
		if strings.Contains(p, "?") {
			continue
		}
		if strings.HasPrefix(p, prefix) && strings.HasSuffix(p, suffix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// clusterWideChildPrefix returns the "/namespaces/" prefix and "/<resource>"
// suffix used to find the per-namespace children of a cluster-wide watch path.
// ok is false when watchPath is namespaced or not a resource path.
func clusterWideChildPrefix(watchPath string) (prefix, suffix string, ok bool) {
	g, v, resource, ns := parseAPIPath(watchPath)
	if ns != "" || resource == "" {
		return "", "", false
	}
	if g == "" {
		prefix = "/api/" + v + "/namespaces/"
	} else {
		prefix = "/apis/" + g + "/" + v + "/namespaces/"
	}
	return prefix, "/" + resource, true
}

// buildReplayTimeline builds the merged, time-sorted event timeline for a watch
// path and assigns each event a monotonic rv. Watch-enabled paths use the watch
// index (bodies read lazily); poll-only paths synthesize events by diffing
// consecutive snapshots (bodies inlined).
func (s *CaptureStore) buildReplayTimeline(watchPath string) []replayEvent {
	var evs []replayEvent

	if wps := s.watchIndexPaths(watchPath); len(wps) > 0 {
		for _, p := range wps {
			wi := s.WatchIndex[p]
			for i := range wi.Seqs {
				if i >= len(wi.Times) || i >= len(wi.EventTypes) {
					break
				}
				evs = append(evs, replayEvent{t: wi.Times[i], typ: wi.EventTypes[i], apiPath: p, seq: wi.Seqs[i]})
			}
		}
	} else {
		// Poll-only: infer events from snapshot diffs.
		for _, p := range s.snapshotPaths(watchPath) {
			entry := s.Index[p]
			if entry == nil {
				continue
			}
			trs, err := transitions.InferPollTransitions(s.ar, p, entry)
			if err != nil {
				continue
			}
			for _, tr := range trs {
				body := tr.After
				if tr.EventType == "DELETED" {
					body = tr.Before
				}
				evs = append(evs, replayEvent{t: tr.Time, typ: tr.EventType, apiPath: p, seq: -1, body: body})
			}
		}
	}

	// Sort by time with a deterministic tiebreak so equal-time events keep a
	// stable order (SliceStable preserves per-path append order for poll events,
	// whose seq is -1).
	sort.SliceStable(evs, func(i, j int) bool {
		if !evs[i].t.Equal(evs[j].t) {
			return evs[i].t.Before(evs[j].t)
		}
		if evs[i].apiPath != evs[j].apiPath {
			return evs[i].apiPath < evs[j].apiPath
		}
		return evs[i].seq < evs[j].seq
	})
	for i := range evs {
		evs[i].rv = replayRVBase + int64(i) + 1
	}
	return evs
}

// rvAsOf returns the resourceVersion of the state as-of time at: replayRVBase
// plus the number of timeline events with t <= at. This equals the rv of the
// last such event, and is never below replayRVBase (so it's never "0").
func rvAsOf(timeline []replayEvent, at time.Time) int64 {
	n := sort.Search(len(timeline), func(i int) bool { return timeline[i].t.After(at) })
	return replayRVBase + int64(n)
}

// withResourceVersion returns obj with metadata.resourceVersion set to rv,
// preserving all other fields. On any parse error it returns obj unchanged.
func withResourceVersion(obj json.RawMessage, rv int64) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(obj, &m); err != nil {
		return obj
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := m["metadata"]; ok {
		if err := json.Unmarshal(raw, &meta); err != nil {
			meta = map[string]json.RawMessage{}
		}
	}
	meta["resourceVersion"], _ = json.Marshal(strconv.FormatInt(rv, 10))
	m["metadata"], _ = json.Marshal(meta)
	out, err := json.Marshal(m)
	if err != nil {
		return obj
	}
	return out
}

// rewriteListResourceVersion sets metadata.resourceVersion to rv on a list body
// (one with an "items" field), leaving non-list bodies untouched.
func rewriteListResourceVersion(body []byte, rv int64) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if _, ok := m["items"]; !ok {
		return body
	}
	meta := map[string]json.RawMessage{}
	if raw, ok := m["metadata"]; ok {
		if err := json.Unmarshal(raw, &meta); err != nil {
			meta = map[string]json.RawMessage{}
		}
	}
	meta["resourceVersion"], _ = json.Marshal(strconv.FormatInt(rv, 10))
	m["metadata"], _ = json.Marshal(meta)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// writeGone writes a 410 Status (reason "Expired") so a client-go informer
// relists cleanly instead of trying to resume from a stale resourceVersion.
func (h *handler) writeGone(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusGone, map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"status":     "Failure",
		"reason":     "Expired",
		"message":    msg,
		"code":       http.StatusGone,
	})
}
