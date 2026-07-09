package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// buildPollStore writes a capture with multiple snapshots for one path and NO
// watch index, so replay must infer events by diffing snapshots.
func buildPollStore(t *testing.T, apiPath string, snaps []struct {
	at   time.Time
	body string
}) *CaptureStore {
	t.Helper()
	out := filepath.Join(t.TempDir(), "poll.kshrk")
	sw, err := archive.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	idx := capture.Index{}
	entry := &capture.IndexEntry{APIPath: apiPath}
	for i, s := range snaps {
		rec := &capture.Record{ID: apiPath, CapturedAt: s.at, APIPath: apiPath, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(s.body)}
		if err := sw.WriteRecord(rec); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
		entry.Seqs = append(entry.Seqs, i)
		entry.Times = append(entry.Times, s.at)
	}
	idx[apiPath] = entry
	meta := &capture.CaptureMetadata{
		FormatVersion: capture.CurrentFormatVersion, CaptureID: "poll-test",
		KubernetesVersion: "v1.30.0", CapturedAt: snaps[0].at, CapturedUntil: snaps[len(snaps)-1].at,
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ar, err := archive.Open(out)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })
	store, err := LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}

func podList(names ...string) string {
	items := ""
	for i, n := range names {
		if i > 0 {
			items += ","
		}
		items += podBody(n)
	}
	return `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"999"},"items":[` + items + `]}`
}

// TestReplayList_ResourceVersionTracksClock verifies the LIST resourceVersion is
// the coherent rvAsOf(clock), advancing as the clock crosses events.
func TestReplayList_ResourceVersionTracksClock(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	h, _, advance := buildPodWatchStore(t, from, 20, false)
	srv := httptest.NewServer(h)
	defer srv.Close()

	listRV := func() string {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/v1/namespaces/default/pods")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var l struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(body, &l); err != nil {
			t.Fatalf("decode list: %v\n%s", err, body)
		}
		return l.Metadata.ResourceVersion
	}

	// At window start (no events yet), RV is the base.
	if got := listRV(); got != "1" {
		t.Errorf("list RV at start = %q, want \"1\"", got)
	}
	advance(20 * time.Second) // clock crosses both events
	if got := listRV(); got != "3" {
		t.Errorf("list RV after both events = %q, want \"3\" (base 1 + 2 events)", got)
	}
}

// TestReplayWatch_ResumeFromRV verifies a watch with ?resourceVersion=X streams
// only events with rv > X, with no initial ADDED burst, and stamps each object
// with its monotonic rv.
func TestReplayWatch_ResumeFromRV(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	h, _, advance := buildPodWatchStore(t, from, 20, false)
	srv := httptest.NewServer(h)
	defer srv.Close()

	advance(20 * time.Second) // both events are now in the past

	// Resume from RV=1 (the window-start RV): expect pod-a (rv 2) then pod-b (rv 3),
	// with no leading BOOKMARK/ADDED-burst.
	next, _, cancel := openWatchStream(t, srv.URL+"/api/v1/namespaces/default/pods?watch=1&resourceVersion=1")
	defer cancel()

	e := next()
	if e.Type != "ADDED" || e.Object.Metadata.Name != "pod-a" {
		t.Fatalf("first frame = (%s %s), want ADDED pod-a", e.Type, e.Object.Metadata.Name)
	}
	if e.Object.Metadata.ResourceVersion != "2" {
		t.Errorf("pod-a rv = %q, want \"2\"", e.Object.Metadata.ResourceVersion)
	}
	e = next()
	if e.Object.Metadata.Name != "pod-b" || e.Object.Metadata.ResourceVersion != "3" {
		t.Errorf("second frame = (%s rv=%s), want pod-b rv=3", e.Object.Metadata.Name, e.Object.Metadata.ResourceVersion)
	}
}

// TestReplayWatch_ReconnectNoDuplicate simulates a client that saw pod-a (rv 2)
// then dropped: reconnecting with resourceVersion=2 must resume at pod-b (rv 3)
// with no re-delivery of pod-a.
func TestReplayWatch_ReconnectNoDuplicate(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	h, _, advance := buildPodWatchStore(t, from, 20, false)
	srv := httptest.NewServer(h)
	defer srv.Close()
	advance(20 * time.Second)

	next, _, cancel := openWatchStream(t, srv.URL+"/api/v1/namespaces/default/pods?watch=1&resourceVersion=2")
	defer cancel()

	e := next()
	if e.Object.Metadata.Name != "pod-b" || e.Object.Metadata.ResourceVersion != "3" {
		t.Fatalf("resume from rv=2: first frame = (%s rv=%s), want pod-b rv=3 (pod-a must not repeat)",
			e.Object.Metadata.Name, e.Object.Metadata.ResourceVersion)
	}
}

// TestReplayWatch_StaleRVReturns410 verifies bogus and below-window RVs yield a
// 410 Gone so the client relists.
func TestReplayWatch_StaleRVReturns410(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			"/api/v1/namespaces/default/pods": {id: "s", at: from, body: emptyPodList},
		},
		[]watchTestEvent{
			{id: "e1", apiPath: "/api/v1/namespaces/default/pods", at: from.Add(2 * time.Second), eventType: "ADDED", objectBody: podBody("pod-a")},
		})
	// Window starts after pod-a, so rvAsOf(windowStart) = 2; RV 1 is "too old".
	clock, _ := newTestClock(t, from.Add(3*time.Second), from.Add(20*time.Second), 1, false, false)
	h := newHandler(store, time.Time{}, false)
	h.clock = clock
	srv := httptest.NewServer(h)
	defer srv.Close()

	// not-a-number → invalid; 1 → below window; 999999999 → newer than any event.
	for _, rv := range []string{"not-a-number", "1", "999999999"} {
		resp, err := http.Get(srv.URL + "/api/v1/namespaces/default/pods?watch=1&resourceVersion=" + rv)
		if err != nil {
			t.Fatalf("watch rv=%s: %v", rv, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusGone {
			t.Errorf("watch rv=%s: status = %d, want 410\n%s", rv, resp.StatusCode, body)
		}
	}
}

// TestReplayWatch_ListBurstCarriesCoherentRV verifies the initial ADDED burst
// stamps items with the coherent rvAsOf value, not the captured object RV, so a
// client resuming from an observed object RV aligns with the event stream.
func TestReplayWatch_ListBurstCarriesCoherentRV(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	path := "/api/v1/namespaces/default/pods"
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{path: {id: "s", at: from, body: podList("pod-x")}},
		[]watchTestEvent{
			{id: "e1", apiPath: path, at: from.Add(2 * time.Second), eventType: "ADDED", objectBody: podBody("pod-y")},
		})
	clock, _ := newTestClock(t, from, from.Add(20*time.Second), 1, false, false)
	h := newHandler(store, time.Time{}, false)
	h.clock = clock
	srv := httptest.NewServer(h)
	defer srv.Close()

	next, _, cancel := openWatchStream(t, srv.URL+path+"?watch=1")
	defer cancel()

	e := next()
	if e.Type != "ADDED" || e.Object.Metadata.Name != "pod-x" {
		t.Fatalf("first frame = (%s %s), want ADDED pod-x", e.Type, e.Object.Metadata.Name)
	}
	if e.Object.Metadata.ResourceVersion != "1" {
		t.Errorf("burst item rv = %q, want \"1\" (rvAsOf(start), not captured RV)", e.Object.Metadata.ResourceVersion)
	}
}

// TestReplayWatch_ZeroRVListsNotGone verifies any zero-valued resourceVersion
// ("0", "00", …) is treated as unset (list+stream), not resume/410.
func TestReplayWatch_ZeroRVListsNotGone(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	h, _, _ := buildPodWatchStore(t, from, 20, false)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, rv := range []string{"0", "00"} {
		next, _, cancel := openWatchStream(t, srv.URL+"/api/v1/namespaces/default/pods?watch=1&resourceVersion="+rv)
		// Initial list is empty → first frame is the BOOKMARK (a 410 would have
		// failed openWatchStream on the non-200 status).
		if e := next(); e.Type != "BOOKMARK" {
			t.Errorf("rv=%q: first frame = %q, want BOOKMARK", rv, e.Type)
		}
		cancel()
	}
}

// TestReplayWatch_PollOnly verifies replay works for a capture with no watch
// index by inferring events from snapshot diffs.
func TestReplayWatch_PollOnly(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	path := "/api/v1/namespaces/default/pods"
	store := buildPollStore(t, path, []struct {
		at   time.Time
		body string
	}{
		{from, podList()}, // empty
		{from.Add(2 * time.Second), podList("pod-a")},          // pod-a ADDED
		{from.Add(4 * time.Second), podList("pod-a", "pod-b")}, // pod-b ADDED
	})

	// No watch index → timeline is inferred from diffs.
	if len(store.WatchIndex) != 0 {
		t.Fatalf("expected poll-only store (no watch index), got %d entries", len(store.WatchIndex))
	}
	tl := store.buildReplayTimeline(path)
	if len(tl) != 2 {
		t.Fatalf("inferred timeline length = %d, want 2 (pod-a, pod-b ADDED)", len(tl))
	}

	clock, advance := newTestClock(t, from, from.Add(10*time.Second), 1, false, false)
	h := newHandler(store, time.Time{}, false)
	h.clock = clock
	srv := httptest.NewServer(h)
	defer srv.Close()

	next, _, cancel := openWatchStream(t, srv.URL+path+"?watch=1")
	defer cancel()

	if e := next(); e.Type != "BOOKMARK" {
		t.Fatalf("first frame = %q, want BOOKMARK (empty initial list)", e.Type)
	}
	advance(10 * time.Second)
	if e := next(); e.Type != "ADDED" || e.Object.Metadata.Name != "pod-a" {
		t.Fatalf("frame = (%s %s), want ADDED pod-a", e.Type, e.Object.Metadata.Name)
	}
	if e := next(); e.Object.Metadata.Name != "pod-b" {
		t.Fatalf("frame = %s, want pod-b", e.Object.Metadata.Name)
	}
}
