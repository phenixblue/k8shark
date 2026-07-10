package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// nextNonBookmark reads watch frames until a non-BOOKMARK event arrives.
func nextNonBookmark(next func() watchEvent) watchEvent {
	for {
		if e := next(); e.Type != "BOOKMARK" {
			return e
		}
	}
}

// TestOverlay_WatchFeedback_LiveWrites is the core PR-2 acceptance test: an
// active watch observes overlay create/update/delete as live ADDED/MODIFIED/
// DELETED events, so a controller sees its own (and others') writes.
func TestOverlay_WatchFeedback_LiveWrites(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	// Empty pod list at the watch path so the initial burst is just a BOOKMARK.
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{podsPath: {id: "s", at: from, body: emptyPodList}}, nil)
	srv := newWritableServer(t, store, clock)

	next, _, cancel := openWatchStream(t, srv.URL+podsPath+"?watch=1")
	defer cancel()

	// Create → ADDED.
	if code, body := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-live")); code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", code, body)
	}
	if e := nextNonBookmark(next); e.Type != "ADDED" || e.Object.Metadata.Name != "pod-live" {
		t.Fatalf("live create: got %s/%s, want ADDED/pod-live", e.Type, e.Object.Metadata.Name)
	}

	// Patch → MODIFIED.
	if code, body := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-live",
		"application/merge-patch+json", `{"metadata":{"labels":{"tier":"web"}}}`); code != 200 {
		t.Fatalf("patch: status %d: %s", code, body)
	}
	if e := nextNonBookmark(next); e.Type != "MODIFIED" || e.Object.Metadata.Name != "pod-live" {
		t.Fatalf("live patch: got %s/%s, want MODIFIED/pod-live", e.Type, e.Object.Metadata.Name)
	}

	// Delete → DELETED.
	if code, body := doReq(t, http.MethodDelete, srv.URL+podsPath+"/pod-live", "", ""); code != 200 {
		t.Fatalf("delete: status %d: %s", code, body)
	}
	if e := nextNonBookmark(next); e.Type != "DELETED" || e.Object.Metadata.Name != "pod-live" {
		t.Fatalf("live delete: got %s/%s, want DELETED/pod-live", e.Type, e.Object.Metadata.Name)
	}
}

// TestOverlay_WatchFeedback_OverlayWinsSuppression verifies that once the overlay
// owns an identity, replayed captured events for that identity are suppressed —
// the overlay copy wins — while events for unowned identities still replay.
func TestOverlay_WatchFeedback_OverlayWinsSuppression(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Minute)
	// Captured events: a MODIFIED for pod-x (which the overlay will own) and an
	// ADDED for pod-y (which it won't), both partway through the window.
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{podsPath: {id: "s", at: from, body: emptyPodList}},
		[]watchTestEvent{
			{id: "e1", apiPath: podsPath, at: from.Add(10 * time.Second), eventType: "MODIFIED", objectBody: podBody("pod-x")},
			{id: "e2", apiPath: podsPath, at: from.Add(12 * time.Second), eventType: "ADDED", objectBody: podBody("pod-y")},
		})
	clock, advance := newTestClock(t, from, to, 1, false, false)
	srv := newWritableServer(t, store, clock)

	// The controller owns pod-x before the watch starts.
	if code, _ := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-x")); code != http.StatusCreated {
		t.Fatalf("create pod-x: status %d", code)
	}

	next, _, cancel := openWatchStream(t, srv.URL+podsPath+"?watch=1")
	defer cancel()

	// The initial burst carries the overlay-owned pod-x as ADDED, ending in a
	// BOOKMARK. Consume up to the bookmark.
	sawOwnedBurst := false
	for {
		e := next()
		if e.Type == "BOOKMARK" {
			break
		}
		if e.Object.Metadata.Name == "pod-x" && e.Type == "ADDED" {
			sawOwnedBurst = true
		}
	}
	if !sawOwnedBurst {
		t.Error("overlay-owned pod-x missing from the initial burst")
	}

	// Replay the captured events. The MODIFIED for owned pod-x (e1) must be
	// suppressed, so the first streamed event is the unowned pod-y (e2).
	advance(20 * time.Second)
	if e := nextNonBookmark(next); e.Type != "ADDED" || e.Object.Metadata.Name != "pod-y" {
		t.Fatalf("got %s/%s, want ADDED/pod-y — a captured event for overlay-owned pod-x was not suppressed",
			e.Type, e.Object.Metadata.Name)
	}
}

// TestOverlay_WatchFeedback_Informer is the KWOK-shaped acceptance test: a real
// client-go dynamic informer observes an overlay create and delete live, exactly
// as a client-go controller (e.g. KWOK) would when pointed at the writable
// overlay.
func TestOverlay_WatchFeedback_Informer(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{podsPath: {id: "s", at: from, body: emptyPodList}}, nil)
	srv := newWritableServer(t, store, clock)

	client, err := dynamic.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, "default", nil)
	inf := factory.ForResource(gvr).Informer()

	added := make(chan string, 8)
	deleted := make(chan string, 8)
	_, err = inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				added <- u.GetName()
			}
		},
		DeleteFunc: func(obj any) {
			// A relist (which the overflow path can trigger) delivers deletes as a
			// DeletedFinalStateUnknown tombstone rather than the object directly.
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			if u, ok := obj.(*unstructured.Unstructured); ok {
				deleted <- u.GetName()
			}
		},
	})
	if err != nil {
		t.Fatalf("add event handler: %v", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	factory.Start(stop)
	if !cache.WaitForCacheSync(stop, inf.HasSynced) {
		t.Fatal("informer failed to sync")
	}
	// Drain any adds from the (empty) initial sync.
	drain(added)

	// Create into the overlay — the informer's watch must observe it.
	if code, body := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-informer")); code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", code, body)
	}
	if got := waitFor(t, added, 10*time.Second); got != "pod-informer" {
		t.Fatalf("informer add: got %q, want pod-informer", got)
	}

	// Delete it — the informer must observe the removal.
	if code, body := doReq(t, http.MethodDelete, srv.URL+podsPath+"/pod-informer", "", ""); code != 200 {
		t.Fatalf("delete: status %d: %s", code, body)
	}
	if got := waitFor(t, deleted, 10*time.Second); got != "pod-informer" {
		t.Fatalf("informer delete: got %q, want pod-informer", got)
	}
}

// TestOverlay_SubscribeOverflow verifies that a subscriber whose buffer fills has
// its overflowCh closed — so the stream tears down without needing to receive a
// further event.
func TestOverlay_SubscribeOverflow(t *testing.T) {
	o := newOverlay()
	_, sub := o.subscribe("", "v1", "pods", "default")

	// Publish past the buffer capacity without ever draining sub.ch.
	for i := 0; i < overlaySubBuffer+2; i++ {
		o.mu.Lock()
		o.publishLocked(overlayWatchEvent{
			typ: "ADDED", rv: int64(i + 1), obj: json.RawMessage(`{}`),
			group: "", version: "v1", resource: "pods", namespace: "default",
		})
		o.mu.Unlock()
	}

	select {
	case <-sub.overflowCh:
		// closed as expected
	default:
		t.Fatal("overflowCh was not closed after the buffer overflowed")
	}
}

func drain(ch chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

func waitFor(t *testing.T, ch chan string, d time.Duration) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(d):
		t.Fatal("timed out waiting for informer event")
		return ""
	}
}
