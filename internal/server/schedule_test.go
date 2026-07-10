package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newWritableServerSched is newWritableServer with the pod-scheduling shim
// enabled (as production --writable enables it). The default newWritableServer
// leaves it off so the many existing pod-create tests aren't perturbed.
func newWritableServerSched(t *testing.T, store *CaptureStore, clock *ReplayClock) *httptest.Server {
	t.Helper()
	h := newHandler(store, time.Time{}, false)
	h.clock = clock
	h.overlay = newOverlay()
	h.schedulePods = true
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// nodeList builds a NodeList body with the given node names.
func nodeList(names ...string) string {
	items := ""
	for i, n := range names {
		if i > 0 {
			items += ","
		}
		items += `{"apiVersion":"v1","kind":"Node","metadata":{"name":"` + n + `"}}`
	}
	return `{"apiVersion":"v1","kind":"NodeList","metadata":{"resourceVersion":"1"},"items":[` + items + `]}`
}

func schedStore(t *testing.T, from time.Time, nodes ...string) *CaptureStore {
	t.Helper()
	snaps := map[string]watchTestRecord{podsPath: {id: "p", at: from, body: emptyPodList}}
	if nodes != nil {
		snaps["/api/v1/nodes"] = watchTestRecord{id: "n", at: from, body: nodeList(nodes...)}
	}
	return buildTestStoreWithWatch(t, snaps, nil)
}

// TestSchedule_SynthesizesNode: with no nodes in the capture, creating a pod
// synthesizes a node and binds the pod to it.
func TestSchedule_SynthesizesNode(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServerSched(t, schedStore(t, from), clock)

	code, body := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-1"))
	if code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", code, body)
	}
	if got := podNodeName(body); got != defaultSyntheticNode {
		t.Errorf("pod nodeName = %q, want %q", got, defaultSyntheticNode)
	}
	// The synthetic node is a real overlay object, fetchable by name.
	code, node := doReq(t, http.MethodGet, srv.URL+"/api/v1/nodes/"+defaultSyntheticNode, "", "")
	if code != 200 || metaString(node, "name") != defaultSyntheticNode {
		t.Fatalf("GET synthetic node: status %d name %q", code, metaString(node, "name"))
	}
	// It carries the annotation that makes a stock `kwok` run manage it.
	if !strings.Contains(string(node), "kwok.x-k8s.io/node") {
		t.Errorf("synthetic node missing kwok management annotation: %s", node)
	}
}

// TestSchedule_UsesCapturedNode: an existing captured node is used; no synthetic
// node is created.
func TestSchedule_UsesCapturedNode(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServerSched(t, schedStore(t, from, "node-a"), clock)

	_, body := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-1"))
	if got := podNodeName(body); got != "node-a" {
		t.Errorf("pod nodeName = %q, want node-a", got)
	}
	// No synthetic node should have been created.
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/nodes/"+defaultSyntheticNode, "", ""); code != 404 {
		t.Errorf("synthetic node was created despite a captured node existing (status %d)", code)
	}
}

// TestSchedule_RoundRobin: successive pods spread across the known nodes.
func TestSchedule_RoundRobin(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServerSched(t, schedStore(t, from, "node-a", "node-b"), clock)

	var got []string
	for _, name := range []string{"pod-1", "pod-2", "pod-3"} {
		_, body := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody(name))
		got = append(got, podNodeName(body))
	}
	// Names are sorted; round-robin over [node-a, node-b].
	want := []string{"node-a", "node-b", "node-a"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pod %d scheduled to %q, want %q (round-robin over %v)", i+1, got[i], want[i], want)
		}
	}
}

// TestSchedule_RespectsExplicitNodeName: a pod that already names a node is left
// alone (the shim is a scheduler, not an override).
func TestSchedule_RespectsExplicitNodeName(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServerSched(t, schedStore(t, from, "node-a"), clock)

	pinned := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pinned","namespace":"default"},"spec":{"nodeName":"chosen-node"}}`
	_, body := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", pinned)
	if got := podNodeName(body); got != "chosen-node" {
		t.Errorf("explicit nodeName overwritten: got %q, want chosen-node", got)
	}
}

// TestSchedule_OffByDefault: without the shim enabled, a pod keeps an empty
// nodeName (default read-only-ish overlay semantics).
func TestSchedule_OffByDefault(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, schedStore(t, from, "node-a"), clock) // scheduling off

	_, body := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-1"))
	if got := podNodeName(body); got != "" {
		t.Errorf("nodeName = %q with scheduling off, want empty", got)
	}
}
