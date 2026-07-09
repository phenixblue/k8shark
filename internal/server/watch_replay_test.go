package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const emptyPodList = `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"1"},"items":[]}`

func podBody(name string) string {
	return `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"` + name + `","namespace":"default"}}`
}

// watchEvent is a decoded frame from a watch stream.
type watchEvent struct {
	Type   string `json:"type"`
	Object struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name            string            `json:"name"`
			ResourceVersion string            `json:"resourceVersion"`
			Annotations     map[string]string `json:"annotations"`
		} `json:"metadata"`
	} `json:"object"`
}

// openWatchStream connects to a watch URL and returns `next` (reads the next
// frame, failing on a 3s timeout), `tryNext` (reads the next frame within a
// caller-supplied timeout, reporting ok=false if none arrives), and a cancel
// that closes the stream.
func openWatchStream(t *testing.T, url string) (next func() watchEvent, tryNext func(time.Duration) (watchEvent, bool), cancel func()) {
	t.Helper()
	ctx, cancelCtx := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancelCtx()
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancelCtx()
		t.Fatalf("watch request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancelCtx()
		t.Fatalf("watch request: status %d: %s", resp.StatusCode, body)
	}
	lines := make(chan watchEvent, 32)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var e watchEvent
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				continue
			}
			select {
			case lines <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	next = func() watchEvent {
		t.Helper()
		select {
		case e := <-lines:
			return e
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for watch event")
			return watchEvent{}
		}
	}
	tryNext = func(d time.Duration) (watchEvent, bool) {
		select {
		case e := <-lines:
			return e, true
		case <-time.After(d):
			return watchEvent{}, false
		}
	}
	cancel = func() { cancelCtx(); resp.Body.Close() }
	return next, tryNext, cancel
}

// TestStreamReplayWatch_EmitsEventsInOrder is the core acceptance test: a watch
// client receives the captured ADDED/MODIFIED/DELETED events in timestamp order
// as the replay clock advances.
func TestStreamReplayWatch_EmitsEventsInOrder(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	to := from.Add(20 * time.Second)
	const podsPath = "/api/v1/namespaces/default/pods"

	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			podsPath: {id: "snap0", at: from, body: emptyPodList},
		},
		[]watchTestEvent{
			{id: "e1", apiPath: podsPath, at: from.Add(2 * time.Second), eventType: "ADDED", objectBody: podBody("pod-a")},
			{id: "e2", apiPath: podsPath, at: from.Add(4 * time.Second), eventType: "ADDED", objectBody: podBody("pod-b")},
			{id: "e3", apiPath: podsPath, at: from.Add(6 * time.Second), eventType: "MODIFIED", objectBody: podBody("pod-a")},
			{id: "e4", apiPath: podsPath, at: from.Add(8 * time.Second), eventType: "DELETED", objectBody: podBody("pod-b")},
		},
	)

	clock, advance := newTestClock(t, from, to, 1, false, false)
	h := newHandler(store, time.Time{}, false)
	h.clock = clock

	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+podsPath+"?watch=1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("watch request: %v", err)
	}
	defer resp.Body.Close()

	type ev struct {
		Type   string `json:"type"`
		Object struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"object"`
	}
	lines := make(chan ev, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var e ev
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				continue
			}
			select {
			case lines <- e:
			case <-ctx.Done():
				return
			}
		}
	}()

	next := func() ev {
		t.Helper()
		select {
		case e := <-lines:
			return e
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for watch event")
			return ev{}
		}
	}

	// The initial list is empty, so the first frame is the BOOKMARK. Only after
	// it do we advance the clock, so the four events are streamed (not folded
	// into the initial snapshot).
	if e := next(); e.Type != "BOOKMARK" {
		t.Fatalf("first frame = %q, want BOOKMARK", e.Type)
	}
	advance(20 * time.Second)

	want := []struct{ typ, name string }{
		{"ADDED", "pod-a"},
		{"ADDED", "pod-b"},
		{"MODIFIED", "pod-a"},
		{"DELETED", "pod-b"},
	}
	for i, w := range want {
		e := next()
		if e.Type != w.typ || e.Object.Metadata.Name != w.name {
			t.Errorf("event %d = (%s %s), want (%s %s)", i, e.Type, e.Object.Metadata.Name, w.typ, w.name)
		}
	}

	if got := clock.EventsEmitted(); got != 4 {
		t.Errorf("EventsEmitted = %d, want 4", got)
	}
}

// TestWatchTimeline_AggregatesAcrossNamespaces covers the `kubectl get pods -A
// --watch` case: a cluster-wide watch path merges the per-namespace watch-index
// entries into one timestamp-ordered timeline.
func TestWatchTimeline_AggregatesAcrossNamespaces(t *testing.T) {
	base := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	ns1 := "/api/v1/namespaces/ns1/pods"
	ns2 := "/api/v1/namespaces/ns2/pods"

	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			ns1: {id: "s1", at: base, body: emptyPodList},
			ns2: {id: "s2", at: base, body: emptyPodList},
		},
		[]watchTestEvent{
			{id: "a", apiPath: ns1, at: base.Add(1 * time.Second), eventType: "ADDED", objectBody: podBody("p1")},
			{id: "b", apiPath: ns2, at: base.Add(2 * time.Second), eventType: "ADDED", objectBody: podBody("p2")},
			{id: "c", apiPath: ns1, at: base.Add(3 * time.Second), eventType: "MODIFIED", objectBody: podBody("p1")},
		},
	)

	timeline := store.buildReplayTimeline("/api/v1/pods")
	if len(timeline) != 3 {
		t.Fatalf("timeline length = %d, want 3", len(timeline))
	}
	for i := 1; i < len(timeline); i++ {
		if timeline[i].t.Before(timeline[i-1].t) {
			t.Errorf("timeline not sorted at %d: %s before %s", i, timeline[i].t, timeline[i-1].t)
		}
		if timeline[i].rv <= timeline[i-1].rv {
			t.Errorf("rv not monotonic at %d: %d <= %d", i, timeline[i].rv, timeline[i-1].rv)
		}
	}
	if timeline[0].apiPath != ns1 || timeline[1].apiPath != ns2 {
		t.Errorf("unexpected merge order: %s then %s", timeline[0].apiPath, timeline[1].apiPath)
	}
}

// buildPodWatchStore builds a store with an empty initial pod list and two
// ADDED events, plus a manually-driven clock over [from, from+windowSecs].
func buildPodWatchStore(t *testing.T, from time.Time, windowSecs int, loop bool) (*handler, *ReplayClock, func(time.Duration)) {
	t.Helper()
	const podsPath = "/api/v1/namespaces/default/pods"
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			podsPath: {id: "snap0", at: from, body: emptyPodList},
		},
		[]watchTestEvent{
			{id: "e1", apiPath: podsPath, at: from.Add(2 * time.Second), eventType: "ADDED", objectBody: podBody("pod-a")},
			{id: "e2", apiPath: podsPath, at: from.Add(4 * time.Second), eventType: "ADDED", objectBody: podBody("pod-b")},
		},
	)
	clock, advance := newTestClock(t, from, from.Add(time.Duration(windowSecs)*time.Second), 1, loop, false)
	h := newHandler(store, time.Time{}, false)
	h.clock = clock
	return h, clock, advance
}

// TestStreamReplayWatch_SeekTriggersRelist verifies that a seek during an idle
// watch (timeline exhausted) restarts the stream: it re-lists (BOOKMARK) and
// replays events after the new position.
func TestStreamReplayWatch_SeekTriggersRelist(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	h, clock, advance := buildPodWatchStore(t, from, 20, false)

	srv := httptest.NewServer(h)
	defer srv.Close()
	next, _, cancel := openWatchStream(t, srv.URL+"/api/v1/namespaces/default/pods?watch=1")
	defer cancel()

	if e := next(); e.Type != "BOOKMARK" {
		t.Fatalf("first frame = %q, want BOOKMARK", e.Type)
	}
	advance(20 * time.Second) // stream both events
	if e := next(); e.Type != "ADDED" || e.Object.Metadata.Name != "pod-a" {
		t.Fatalf("frame = (%s %s), want ADDED pod-a", e.Type, e.Object.Metadata.Name)
	}
	if e := next(); e.Object.Metadata.Name != "pod-b" {
		t.Fatalf("frame = %s, want pod-b", e.Object.Metadata.Name)
	}

	// Seek back to the start: the idle stream should re-list (BOOKMARK) and then,
	// once the clock advances again, replay the events.
	clock.Seek(from)
	if e := next(); e.Type != "BOOKMARK" {
		t.Fatalf("after seek: frame = %q, want relist BOOKMARK", e.Type)
	}
	advance(20 * time.Second)
	if e := next(); e.Type != "ADDED" || e.Object.Metadata.Name != "pod-a" {
		t.Fatalf("after seek+advance: frame = (%s %s), want ADDED pod-a", e.Type, e.Object.Metadata.Name)
	}
}

// TestStreamReplayWatch_LoopRelistsWithBookmark verifies that on a loop wrap the
// stream re-lists with a BOOKMARK (matching the initial list behavior).
func TestStreamReplayWatch_LoopRelistsWithBookmark(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	h, _, advance := buildPodWatchStore(t, from, 10, true)

	srv := httptest.NewServer(h)
	defer srv.Close()
	next, _, cancel := openWatchStream(t, srv.URL+"/api/v1/namespaces/default/pods?watch=1")
	defer cancel()

	if e := next(); e.Type != "BOOKMARK" {
		t.Fatalf("first frame = %q, want BOOKMARK", e.Type)
	}
	advance(5 * time.Second) // within the window: stream both events
	if e := next(); e.Object.Metadata.Name != "pod-a" {
		t.Fatalf("frame = %s, want pod-a", e.Object.Metadata.Name)
	}
	if e := next(); e.Object.Metadata.Name != "pod-b" {
		t.Fatalf("frame = %s, want pod-b", e.Object.Metadata.Name)
	}
	advance(6 * time.Second) // cross the window end → loop wrap
	if e := next(); e.Type != "BOOKMARK" {
		t.Fatalf("on loop wrap: frame = %q, want relist BOOKMARK", e.Type)
	}
}

// TestStreamReplayWatch_BookmarkRVNonZero verifies that a synthesized empty list
// (a watch on a resource with no captured data) still yields a non-zero BOOKMARK
// resourceVersion, which watch clients require.
func TestStreamReplayWatch_BookmarkRVNonZero(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	h, _, _ := buildPodWatchStore(t, from, 20, false)

	srv := httptest.NewServer(h)
	defer srv.Close()
	// Watch a resource with no captured data → empty-list synthesis (RV "0").
	next, _, cancel := openWatchStream(t, srv.URL+"/api/v1/namespaces/default/services?watch=1")
	defer cancel()

	e := next()
	if e.Type != "BOOKMARK" {
		t.Fatalf("first frame = %q, want BOOKMARK", e.Type)
	}
	if rv := e.Object.Metadata.ResourceVersion; rv == "" || rv == "0" {
		t.Errorf("BOOKMARK resourceVersion = %q, want non-zero", rv)
	}
}

// TestStreamReplayWatch_EventsAfterWindowEnd verifies that events timestamped
// after a --to sub-range end don't hang the stream: in-window events are emitted
// and the stream goes idle (rather than waiting forever for the clock to reach
// an event it will never advance to).
func TestStreamReplayWatch_EventsAfterWindowEnd(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	// Window ends at from+3s: pod-a (at +2s) is inside; pod-b (at +4s) is after.
	h, _, advance := buildPodWatchStore(t, from, 3, false)

	srv := httptest.NewServer(h)
	defer srv.Close()
	next, tryNext, cancel := openWatchStream(t, srv.URL+"/api/v1/namespaces/default/pods?watch=1")
	defer cancel()

	if e := next(); e.Type != "BOOKMARK" {
		t.Fatalf("first frame = %q, want BOOKMARK", e.Type)
	}
	advance(10 * time.Second) // clock clamps at the window end (from+3s)

	if e := next(); e.Type != "ADDED" || e.Object.Metadata.Name != "pod-a" {
		t.Fatalf("frame = (%s %s), want ADDED pod-a", e.Type, e.Object.Metadata.Name)
	}
	// pod-b is past the window end: it must not be emitted, and the stream must
	// not hang — it should simply idle.
	if e, ok := tryNext(500 * time.Millisecond); ok {
		t.Errorf("unexpected frame after window end: (%s %s)", e.Type, e.Object.Metadata.Name)
	}
}
