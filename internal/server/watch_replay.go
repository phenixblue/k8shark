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

	body, _ := applySelectors(rawBody, labelSelector, fieldSelector)

	var parsed struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return watchList{}, false, fmt.Errorf("parsing list")
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

	sort.SliceStable(evs, func(i, j int) bool { return evs[i].t.Before(evs[j].t) })
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
		rv := l.ResourceVersion
		if rv == "" {
			rv = fmt.Sprintf("%d", h.store.Metadata.CapturedAt.Unix())
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

	// relist re-emits the state as-of the window start as an ADDED burst; used on
	// each loop wrap so a long-lived watcher sees the timeline replay again.
	relist := func() {
		l, ok, err := h.resolveWatchList(watchPath, windowStart, labelSel, fieldSel)
		if err != nil || !ok {
			return
		}
		for _, item := range l.Items {
			emit("ADDED", item)
		}
	}

	// Initial list burst + bookmark.
	for _, item := range list.Items {
		emit("ADDED", item)
	}
	emitBookmark(list)

	after := startAt
	epoch := startEpoch
	for {
		switch h.replayPass(ctx, timer, clock, timeline, after, epoch, labelSel, fieldSel, emit) {
		case passCanceled:
			return
		case passLooped:
			// Clock wrapped mid-pass: re-list and replay from the window start.
			relist()
			_, epoch, _ = clock.Sample()
			after = windowStart
		case passDone:
			// Timeline exhausted for this epoch. Without loop, hold open until the
			// client disconnects or the watch times out.
			if !clock.Loop() {
				select {
				case <-ctx.Done():
				case <-timer:
				}
				return
			}
			// With loop, wait for the next wrap, then re-list and replay again.
			if !waitForLoop(ctx, timer, clock, epoch) {
				return
			}
			relist()
			_, epoch, _ = clock.Sample()
			after = windowStart
		}
	}
}

// waitForLoop blocks until the clock's epoch advances past `epoch` (a loop
// wrap), returning true. It returns false if the request is canceled or the
// watch times out first.
func waitForLoop(ctx context.Context, timer <-chan time.Time, clock *ReplayClock, epoch int) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer:
			return false
		case <-time.After(replayPollInterval):
			if _, e, _ := clock.Sample(); e != epoch {
				return true
			}
		}
	}
}

type passResult int

const (
	passDone     passResult = iota // reached the end of the timeline
	passLooped                     // clock wrapped mid-pass; caller should re-list
	passCanceled                   // client disconnected or timeout elapsed
)

// replayPass streams timeline events with timestamp after `after`, each held
// until the clock crosses it, until the timeline is exhausted (passDone), the
// clock loops past the current epoch (passLooped), or the request is canceled
// or times out (passCanceled).
func (h *handler) replayPass(ctx context.Context, timer <-chan time.Time, clock *ReplayClock, timeline []replayEvent, after time.Time, epoch int, labelSel, fieldSel string, emit func(string, json.RawMessage)) passResult {
	for _, ev := range timeline {
		if !ev.t.After(after) {
			continue
		}
		// Wait until the clock reaches this event's timestamp.
		for {
			pos, ep, _ := clock.Sample()
			if ep != epoch {
				return passLooped
			}
			if !pos.Before(ev.t) {
				break
			}
			wait := time.Duration(float64(ev.t.Sub(pos)) / clock.Speed())
			if wait > replayPollInterval || wait <= 0 {
				wait = replayPollInterval
			}
			select {
			case <-ctx.Done():
				return passCanceled
			case <-timer:
				return passCanceled
			case <-time.After(wait):
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
