package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

// watchList is a parsed list body ready to be streamed as watch events.
type watchList struct {
	APIVersion      string
	Kind            string
	ResourceVersion string
	Items           []json.RawMessage
}

// resolveWatchList reconstructs the list for a watch path as-of at, applying the
// same fallback chain used for plain watches (cross-namespace aggregation,
// cluster-scoped namespace filtering, empty-list synthesis), then applies label
// and field selectors. ok is false only when the resource can't be resolved at
// all; err is non-nil only for internal reconstruction failures.
func (h *handler) resolveWatchList(watchPath string, at time.Time, labelSelector, fieldSelector string) (watchList, bool, error) {
	rawBody, code, err := h.store.ReconstructAt(watchPath, at)
	if err != nil {
		return watchList{}, false, err
	}

	if code == 404 {
		// Only aggregate across namespaces for cluster-wide watch paths.
		if _, _, _, reqNS := parseAPIPath(watchPath); reqNS == "" {
			rawBody, code, err = h.store.AggregateAcrossNamespaces(watchPath, at)
			if err != nil {
				return watchList{}, false, err
			}
		}
	}

	if code == 404 {
		// Cluster-scoped fallback: resource was captured at the cluster path
		// (e.g. /api/v1/pods) but the watch targets a specific namespace. Filter
		// by metadata.namespace so namespaced watchers see the right items.
		g, v, resource, ns := parseAPIPath(watchPath)
		if ns != "" && resource != "" {
			var clusterPath string
			if g == "" {
				clusterPath = "/api/" + v + "/" + resource
			} else {
				clusterPath = "/apis/" + g + "/" + v + "/" + resource
			}
			clusterBody, clusterCode, cerr := h.store.ReconstructAt(clusterPath, at)
			if cerr == nil && clusterCode == 200 {
				filtered, ferr := applySelectors(clusterBody, "", "metadata.namespace="+ns)
				if ferr == nil {
					rawBody, code = filtered, 200
				}
			}
		}
	}

	if code == 404 {
		g, v, resource, _ := parseAPIPath(watchPath)
		if resource != "" {
			av := v
			if g != "" {
				av = g + "/" + v
			}
			emptyList, _ := json.Marshal(map[string]any{
				"apiVersion": av,
				"kind":       resourceToKind(resource) + "List",
				"metadata":   map[string]string{"resourceVersion": "0"},
				"items":      []any{},
			})
			rawBody = emptyList
			code = 200
		}
	}

	if code != 200 {
		return watchList{}, false, nil
	}

	body, serr := applySelectors(rawBody, labelSelector, fieldSelector)
	if serr != nil {
		// Best-effort: fall back to the unfiltered list rather than failing the
		// whole watch on a selector/marshal error.
		body = rawBody
	}

	var parsed struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return watchList{}, false, fmt.Errorf("parsing watch list for %s: %w", watchPath, err)
	}
	return watchList{
		APIVersion:      parsed.APIVersion,
		Kind:            parsed.Kind,
		ResourceVersion: parsed.Metadata.ResourceVersion,
		Items:           parsed.Items,
	}, true, nil
}

// replayEvent is one captured watch event on the merged replay timeline.
type replayEvent struct {
	t       time.Time
	typ     string // ADDED | MODIFIED | DELETED
	apiPath string
	seq     int
}

// watchTimeline returns the captured watch events for a watch path, merged and
// sorted by timestamp. For a cluster-wide path (no namespace) it aggregates the
// per-namespace watch-index entries the capture demultiplexed.
func (s *CaptureStore) watchTimeline(watchPath string) []replayEvent {
	var evs []replayEvent
	add := func(apiPath string, wi *capture.WatchIndexEntry) {
		for i := range wi.Seqs {
			if i >= len(wi.Times) || i >= len(wi.EventTypes) {
				break
			}
			evs = append(evs, replayEvent{t: wi.Times[i], typ: wi.EventTypes[i], apiPath: apiPath, seq: wi.Seqs[i]})
		}
	}

	if wi, ok := s.WatchIndex[watchPath]; ok {
		add(watchPath, wi)
	} else if g, v, resource, ns := parseAPIPath(watchPath); ns == "" && resource != "" {
		var prefix string
		if g == "" {
			prefix = "/api/" + v + "/namespaces/"
		} else {
			prefix = "/apis/" + g + "/" + v + "/namespaces/"
		}
		suffix := "/" + resource
		for p, wi := range s.WatchIndex {
			if strings.HasPrefix(p, prefix) && strings.HasSuffix(p, suffix) {
				add(p, wi)
			}
		}
	}

	// Sort by timestamp, with a deterministic tiebreak (apiPath then seq) so
	// equal-time events don't flip order across runs — the merged input order
	// depends on map iteration for the cluster-wide aggregation case.
	sort.Slice(evs, func(i, j int) bool {
		if !evs[i].t.Equal(evs[j].t) {
			return evs[i].t.Before(evs[j].t)
		}
		if evs[i].apiPath != evs[j].apiPath {
			return evs[i].apiPath < evs[j].apiPath
		}
		return evs[i].seq < evs[j].seq
	})
	return evs
}

// matchesSelectors reports whether a single object satisfies the label and
// field selectors. An empty selector matches everything; a malformed selector
// or unparseable object is treated as a match (best-effort, never hides data).
func matchesSelectors(raw json.RawMessage, labelSelector, fieldSelector string) bool {
	if labelSelector == "" && fieldSelector == "" {
		return true
	}
	labelReqs, err := parseRequirements(labelSelector)
	if err != nil {
		return true
	}
	fieldReqs := parseFieldSelector(fieldSelector)
	var obj k8sObject
	if err := json.Unmarshal(raw, &obj); err != nil {
		return true
	}
	return matchesLabels(&obj, labelReqs) && matchesFields(&obj, fieldReqs)
}

// replayPollInterval bounds how long the streamer sleeps between clock checks so
// it stays responsive to pause, seek, speed changes, and loop wraps.
const replayPollInterval = 200 * time.Millisecond

// streamReplayWatch streams a captured watch as the replay clock advances. It
// sends the state as-of the clock's current position as an ADDED burst (the
// informer's initial list), a BOOKMARK, then the captured events in timestamp
// order — each held until the clock crosses its timestamp. Under --loop the
// stream re-lists and replays from the window start on each wrap.
func (h *handler) streamReplayWatch(w http.ResponseWriter, r *http.Request, path string) {
	clock := h.clock
	watchPath := strings.TrimSuffix(path, "/")
	labelSel := r.URL.Query().Get("labelSelector")
	fieldSel := r.URL.Query().Get("fieldSelector")

	windowStart, _ := clock.Window()
	startAt, startEpoch, _ := clock.Sample()

	list, ok, err := h.resolveWatchList(watchPath, startAt, labelSel, fieldSel)
	if err != nil {
		h.writeStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		h.writeStatus(w, http.StatusNotFound, fmt.Sprintf("%q not found in capture", path))
		return
	}

	timeline := h.store.watchTimeline(watchPath)

	// Honor ?timeoutSeconds: nil channel blocks forever (no timeout).
	var timer <-chan time.Time
	if secs := r.URL.Query().Get("timeoutSeconds"); secs != "" {
		if n, err := strconv.Atoi(secs); err == nil && n > 0 {
			timer = time.After(time.Duration(n) * time.Second)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	emit := func(typ string, obj json.RawMessage) {
		data, _ := json.Marshal(map[string]any{"type": typ, "object": obj})
		_, _ = fmt.Fprintf(w, "%s\n", data)
		if canFlush {
			flusher.Flush()
		}
	}
	emitBookmark := func(l watchList) {
		// Treat "0" as unspecified — aggregated/synthesized empty lists carry RV
		// "0", but watch clients expect a non-zero BOOKMARK resourceVersion. In
		// replay mode the window start is always a valid, positive time.
		rv := l.ResourceVersion
		if rv == "" || rv == "0" {
			rv = bookmarkResourceVersion(windowStart, h.store.Metadata.CapturedAt, h.store.Metadata.CapturedUntil)
		}
		bookmarkKind := strings.TrimSuffix(l.Kind, "List")
		bookmarkAPIVersion := l.APIVersion
		if bookmarkKind == "" {
			bookmarkKind = "Status"
		}
		if bookmarkAPIVersion == "" {
			bookmarkAPIVersion = "v1"
		}
		data, _ := json.Marshal(map[string]any{
			"type": "BOOKMARK",
			"object": map[string]any{
				"apiVersion": bookmarkAPIVersion,
				"kind":       bookmarkKind,
				"metadata":   map[string]string{"resourceVersion": rv},
			},
		})
		_, _ = fmt.Fprintf(w, "%s\n", data)
		if canFlush {
			flusher.Flush()
		}
	}

	ctx := r.Context()

	// relistAt re-emits the state as-of `at` as an ADDED burst + BOOKMARK, so a
	// long-lived watcher sees a fresh, clearly-delimited list after a loop wrap
	// or a seek — mirroring the initial list behavior.
	relistAt := func(at time.Time) {
		l, ok, err := h.resolveWatchList(watchPath, at, labelSel, fieldSel)
		if err != nil || !ok {
			return
		}
		for _, item := range l.Items {
			emit("ADDED", item)
		}
		emitBookmark(l)
	}

	// Initial list burst + bookmark.
	for _, item := range list.Items {
		emit("ADDED", item)
	}
	emitBookmark(list)

	after := startAt
	epoch := startEpoch
	seekGen := clock.SeekGen()
	for {
		res := h.replayPass(ctx, timer, clock, timeline, after, epoch, seekGen, labelSel, fieldSel, emit)
		if res == passCanceled {
			return
		}
		if res == passDone {
			// Timeline exhausted for this epoch. Wait for a restart trigger (a
			// loop wrap or a seek) or shutdown.
			switch waitForRestart(ctx, timer, clock, epoch, seekGen) {
			case restartNone:
				return
			case restartLoop:
				res = passLooped
			case restartSeek:
				res = passSeeked
			}
		}

		// Re-list and choose the next start point: a loop wrap replays the whole
		// window from the start; a seek resumes from the new clock position.
		if res == passLooped {
			relistAt(windowStart)
			after = windowStart
		} else { // passSeeked
			pos := clock.Now()
			relistAt(pos)
			after = pos
		}
		_, epoch, _ = clock.Sample()
		seekGen = clock.SeekGen()
	}
}

// restartTrigger explains why a paused/idle stream should resume streaming.
type restartTrigger int

const (
	restartNone restartTrigger = iota // canceled or timed out
	restartLoop                       // the clock wrapped (loop)
	restartSeek                       // the clock was seeked
)

// waitForRestart blocks until the clock wraps past `epoch`, is seeked past
// `seekGen`, or the request is canceled/times out. A single ticker avoids
// allocating a timer on every poll.
func waitForRestart(ctx context.Context, timer <-chan time.Time, clock *ReplayClock, epoch, seekGen int) restartTrigger {
	ticker := time.NewTicker(replayPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return restartNone
		case <-timer:
			return restartNone
		case <-ticker.C:
			if clock.SeekGen() != seekGen {
				return restartSeek
			}
			if _, e, _ := clock.Sample(); e != epoch {
				return restartLoop
			}
		}
	}
}

type passResult int

const (
	passDone     passResult = iota // reached the end of the timeline
	passLooped                     // clock wrapped mid-pass; caller should re-list at window start
	passSeeked                     // clock was seeked mid-pass; caller should re-list at new position
	passCanceled                   // client disconnected or timeout elapsed
)

// sleepInterruptible waits for d, returning true if the request is canceled or
// the watch times out first. It uses a stoppable timer (stopped on return) so
// canceled waits don't leave timers lingering under load; a non-positive d
// returns immediately after a non-blocking cancellation check.
func sleepInterruptible(ctx context.Context, timer <-chan time.Time, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return true
		case <-timer:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-timer:
		return true
	case <-t.C:
		return false
	}
}

// replayPass streams timeline events with timestamp after `after`, each held
// until the clock crosses it, until the timeline is exhausted (passDone), the
// clock loops past `epoch` (passLooped) or is seeked past `seekGen`
// (passSeeked), or the request is canceled/times out (passCanceled).
func (h *handler) replayPass(ctx context.Context, timer <-chan time.Time, clock *ReplayClock, timeline []replayEvent, after time.Time, epoch, seekGen int, labelSel, fieldSel string, emit func(string, json.RawMessage)) passResult {
	for _, ev := range timeline {
		if !ev.t.After(after) {
			continue
		}
		// Wait until the clock reaches this event's timestamp.
		for {
			if clock.SeekGen() != seekGen {
				return passSeeked
			}
			pos, ep, _ := clock.Sample()
			if ep != epoch {
				return passLooped
			}
			if !pos.Before(ev.t) {
				break
			}
			wait := time.Duration(float64(ev.t.Sub(pos)) / clock.Speed())
			// Cap the wait so we stay responsive to pause/seek/loop, but never
			// impose a floor: closely-spaced events at high speed must not each
			// incur an artificial poll-interval delay.
			if wait > replayPollInterval {
				wait = replayPollInterval
			}
			if wait < 0 {
				wait = 0
			}
			if sleepInterruptible(ctx, timer, wait) {
				return passCanceled
			}
		}

		rec, err := h.store.readRecord(ev.apiPath, ev.seq)
		if err != nil {
			continue
		}
		if !matchesSelectors(rec.ResponseBody, labelSel, fieldSel) {
			continue
		}
		emit(ev.typ, rec.ResponseBody)
		clock.AddEvents(1)
	}
	return passDone
}
