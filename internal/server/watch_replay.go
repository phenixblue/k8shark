package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
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

// replayEvent is one event on the merged, RV-assigned replay timeline. Watch
// events read their body lazily via (apiPath, seq); poll-only inferred events
// carry the body inline (seq is -1).
type replayEvent struct {
	t       time.Time
	typ     string // ADDED | MODIFIED | DELETED
	rv      int64  // monotonic resourceVersion (1-based sequence)
	apiPath string
	seq     int
	body    json.RawMessage // inline body for poll-only events; nil → read lazily
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

// streamReplayWatch streams a captured watch as the replay clock advances, with
// informer-grade resourceVersion (RV) semantics:
//
//   - No resourceVersion (or "0"): send the state as-of the clock as an ADDED
//     burst + BOOKMARK, then stream events with rv > rvAsOf(clock).
//   - resourceVersion=X (X>0): resume — no initial burst; stream events rv > X.
//     A bogus or below-window X returns 410 Gone so the client relists cleanly.
//
// Each streamed event object carries its own monotonic rv; the BOOKMARK carries
// rvAsOf of its list-as-of time. Under --loop / seek the stream re-lists and
// resumes from the new position.
func (h *handler) streamReplayWatch(w http.ResponseWriter, r *http.Request, path string) {
	clock := h.clock
	watchPath := strings.TrimSuffix(path, "/")
	labelSel := r.URL.Query().Get("labelSelector")
	fieldSel := r.URL.Query().Get("fieldSelector")

	// Validate resourceVersion first — a malformed/negative value returns 410
	// before we build the (possibly expensive, poll-only) timeline. Any value
	// that parses to 0 ("0", "00", …) is unset → list+stream.
	reqRV, hasReqRV := int64(0), false
	if rvParam := r.URL.Query().Get("resourceVersion"); rvParam != "" {
		v, err := strconv.ParseInt(rvParam, 10, 64)
		if err != nil || v < 0 {
			h.writeGone(w, fmt.Sprintf("resourceVersion %q is not valid; relist", rvParam))
			return
		}
		reqRV, hasReqRV = v, v > 0
	}

	timeline := h.timelineFor(watchPath)
	windowStart, _ := clock.Window()

	// Resume vs list+stream, plus stale/unknown-RV → 410 (needs the timeline).
	resume := false
	var minRV int64
	if hasReqRV {
		if reqRV < rvAsOf(timeline, windowStart) {
			h.writeGone(w, fmt.Sprintf("resourceVersion %d is before the replay window; relist", reqRV))
			return
		}
		// Too large / unknown (e.g. a raw etcd RV from the original capture):
		// relist rather than silently streaming nothing. Include the overlay's RV
		// so a LIST RV bumped by an overlay write is still a valid resume point
		// (otherwise a LIST→WATCH informer would loop on 410).
		maxRV := replayRVBase + int64(len(timeline))
		if h.overlay != nil {
			if orv := h.overlay.currentRV(); orv > maxRV {
				maxRV = orv
			}
		}
		if reqRV > maxRV {
			h.writeGone(w, fmt.Sprintf("resourceVersion %d is newer than any replay event (max %d); relist", reqRV, maxRV))
			return
		}
		resume = true
		minRV = reqRV
	}

	startAt, startEpoch, _ := clock.Sample()

	// Path-derived defaults for the BOOKMARK object kind/apiVersion.
	g, v, resource, _ := parseAPIPath(watchPath)
	defKind := resourceToKind(resource)
	defAPIVersion := v
	if g != "" {
		defAPIVersion = g + "/" + v
	}

	// Always resolve the list — even when resuming — so a watch on a path that
	// can't be resolved returns 404 rather than a 200 that streams nothing. The
	// reconstruction is response-cached; resume mode just doesn't emit the burst.
	list, ok, err := h.resolveWatchList(watchPath, startAt, labelSel, fieldSel)
	if err != nil {
		h.writeStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		h.writeStatus(w, http.StatusNotFound, fmt.Sprintf("%q not found in capture", path))
		return
	}
	if !resume {
		minRV = rvAsOf(timeline, startAt)
	}

	// Honor ?timeoutSeconds: nil channel blocks forever (no timeout).
	timer, stopTimer := watchTimeout(r.URL.Query().Get("timeoutSeconds"))
	defer stopTimer()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	// emit writes a watch frame and reports whether it was actually written, so
	// callers only count frames that reached the client.
	emit := func(typ string, obj json.RawMessage) bool {
		// Skip malformed frames rather than writing a blank line: a captured
		// object body could be invalid JSON, which would fail to marshal.
		data, err := json.Marshal(map[string]any{"type": typ, "object": obj})
		if err != nil {
			return false
		}
		_, _ = fmt.Fprintf(w, "%s\n", data)
		if canFlush {
			flusher.Flush()
		}
		return true
	}
	// emitBookmark writes the BOOKMARK marking the end of a (re)list, carrying rv
	// as the resourceVersion so clients resume from a coherent point.
	emitBookmark := func(l watchList, rv int64) {
		bookmarkKind := strings.TrimSuffix(l.Kind, "List")
		if bookmarkKind == "" {
			bookmarkKind = defKind
		}
		if bookmarkKind == "" {
			bookmarkKind = "Status"
		}
		bookmarkAPIVersion := l.APIVersion
		if bookmarkAPIVersion == "" {
			bookmarkAPIVersion = defAPIVersion
		}
		if bookmarkAPIVersion == "" {
			bookmarkAPIVersion = "v1"
		}
		data, _ := json.Marshal(map[string]any{
			"type": "BOOKMARK",
			"object": map[string]any{
				"apiVersion": bookmarkAPIVersion,
				"kind":       bookmarkKind,
				"metadata":   map[string]string{"resourceVersion": strconv.FormatInt(rv, 10)},
			},
		})
		_, _ = fmt.Fprintf(w, "%s\n", data)
		if canFlush {
			flusher.Flush()
		}
	}

	ctx := r.Context()

	// relistAt re-emits the state as-of `at` as an ADDED burst + BOOKMARK and
	// returns the RV that a subsequent stream should resume above.
	relistAt := func(at time.Time) int64 {
		rv := rvAsOf(timeline, at)
		l, ok, err := h.resolveWatchList(watchPath, at, labelSel, fieldSel)
		if err != nil || !ok {
			return rv
		}
		for _, item := range l.Items {
			// Stamp burst items with the list's coherent rv so a client that
			// resumes from an observed object RV aligns with the event stream.
			emit("ADDED", withResourceVersion(item, rv))
		}
		emitBookmark(l, rv)
		return rv
	}

	// Initial list burst + bookmark, unless resuming from a client-supplied RV.
	// Burst items carry the list's coherent rv (minRV = rvAsOf(startAt)) so a
	// client resuming from an observed object RV aligns with the event stream.
	if !resume {
		for _, item := range list.Items {
			emit("ADDED", withResourceVersion(item, minRV))
		}
		emitBookmark(list, minRV)
	}

	epoch := startEpoch
	seekGen := clock.SeekGen()
	for {
		res := h.replayPass(ctx, timer, clock, timeline, minRV, epoch, seekGen, labelSel, fieldSel, emit)
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

		// Re-list and choose the next resume RV: a loop wrap replays the whole
		// window from the start; a seek resumes from the new clock position.
		if res == passLooped {
			minRV = relistAt(windowStart)
		} else { // passSeeked
			minRV = relistAt(clock.Now())
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

// replayPass streams timeline events with rv > minRV, each held until the clock
// crosses its timestamp, until the timeline is exhausted (passDone), the clock
// loops past `epoch` (passLooped) or is seeked past `seekGen` (passSeeked), or
// the request is canceled/times out (passCanceled). Each emitted object carries
// its own monotonic rv so clients can resume coherently.
func (h *handler) replayPass(ctx context.Context, timer <-chan time.Time, clock *ReplayClock, timeline []replayEvent, minRV int64, epoch, seekGen int, labelSel, fieldSel string, emit func(string, json.RawMessage) bool) passResult {
	for _, ev := range timeline {
		if ev.rv <= minRV {
			continue
		}
		// Wait until the clock reaches this event's timestamp.
		for {
			if clock.SeekGen() != seekGen {
				return passSeeked
			}
			pos, ep, ended := clock.Sample()
			if ep != epoch {
				return passLooped
			}
			if !pos.Before(ev.t) {
				break
			}
			// Window ended (loop disabled) and the clock won't advance to this
			// event's timestamp — e.g. events after a --to sub-range end. Stop the
			// pass so the stream goes idle (a later seek restarts it) instead of
			// waiting forever.
			if ended {
				return passDone
			}
			// Compute the scaled wait in float space and cap it BEFORE converting
			// to a time.Duration: a very small speed makes the division huge, and
			// converting that to int64 nanoseconds would overflow to a negative
			// value (collapsing to 0 and spinning). Cap keeps us responsive to
			// pause/seek/loop; no floor, so closely-spaced events at high speed
			// aren't each delayed a poll interval.
			scaled := float64(ev.t.Sub(pos)) / clock.Speed()
			var wait time.Duration
			switch {
			case scaled <= 0:
				wait = 0
			case scaled >= float64(replayPollInterval):
				wait = replayPollInterval
			default:
				wait = time.Duration(scaled)
			}
			if sleepInterruptible(ctx, timer, wait) {
				return passCanceled
			}
		}

		// Poll-only events carry their body inline; watch events read it lazily.
		body := ev.body
		if body == nil {
			rec, err := h.store.readRecord(ev.apiPath, ev.seq)
			if err != nil {
				continue
			}
			body = rec.ResponseBody
		}
		if !matchesSelectors(body, labelSel, fieldSel) {
			continue
		}
		// Stamp the object with its monotonic rv so a reconnecting client resumes
		// from a coherent resourceVersion.
		if emit(ev.typ, withResourceVersion(body, ev.rv)) {
			clock.AddEvents(1)
		}
	}
	return passDone
}
