package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const emptyPodList = `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"1"},"items":[]}`

func podBody(name string) string {
	return `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"` + name + `","namespace":"default"}}`
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

	timeline := store.watchTimeline("/api/v1/pods")
	if len(timeline) != 3 {
		t.Fatalf("timeline length = %d, want 3", len(timeline))
	}
	for i := 1; i < len(timeline); i++ {
		if timeline[i].t.Before(timeline[i-1].t) {
			t.Errorf("timeline not sorted at %d: %s before %s", i, timeline[i].t, timeline[i-1].t)
		}
	}
	if timeline[0].apiPath != ns1 || timeline[1].apiPath != ns2 {
		t.Errorf("unexpected merge order: %s then %s", timeline[0].apiPath, timeline[1].apiPath)
	}
}
