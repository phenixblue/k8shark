package v2

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
)

type v2WatchEvent struct {
	id, apiPath, eventType, body string
	at                           time.Time
}

// buildV2WatchStore writes watch event records plus a watch index, mirroring
// the watch-store helper in internal/server's tests. Seqs are assigned per path
// in write order, matching how StreamWriter numbers records.
func buildV2WatchStore(t *testing.T, events []v2WatchEvent, captureStart time.Time) *server.CaptureStore {
	t.Helper()
	out := filepath.Join(t.TempDir(), "watch.kshrk")
	sw, err := archive.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	watchIndex := make(capture.WatchIndex)
	seqByPath := map[string]int{}
	var until time.Time
	for _, ev := range events {
		rec := capture.Record{
			ID: ev.id, CapturedAt: ev.at, APIPath: ev.apiPath, EventType: ev.eventType,
			HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(ev.body),
		}
		if err := sw.WriteRecord(&rec); err != nil {
			t.Fatalf("WriteRecord(%s): %v", ev.id, err)
		}
		wi := watchIndex[ev.apiPath]
		if wi == nil {
			wi = &capture.WatchIndexEntry{APIPath: ev.apiPath}
			watchIndex[ev.apiPath] = wi
		}
		seq := seqByPath[ev.apiPath]
		seqByPath[ev.apiPath] = seq + 1
		wi.Seqs = append(wi.Seqs, seq)
		wi.Times = append(wi.Times, ev.at)
		wi.EventTypes = append(wi.EventTypes, ev.eventType)
		if ev.at.After(until) {
			until = ev.at
		}
	}
	meta := &capture.CaptureMetadata{CaptureID: "watch-test", CapturedAt: captureStart, CapturedUntil: until, RecordCount: len(events)}
	if err := sw.Finish(meta, capture.Index{}, watchIndex); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ar, err := archive.Open(out)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })
	store, err := server.LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}

func TestServeObjectHistory(t *testing.T) {
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	path := "/api/v1/namespaces/default/pods"
	pod := func(phase string) string {
		return `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web","namespace":"default"},"status":{"phase":"` + phase + `"}}`
	}
	other := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"db","namespace":"default"},"status":{"phase":"Running"}}`
	events := []v2WatchEvent{
		{id: "w1", apiPath: path, at: now, eventType: "ADDED", body: pod("Pending")},
		{id: "w2", apiPath: path, at: now.Add(10 * time.Second), eventType: "MODIFIED", body: pod("Running")},
		{id: "w3", apiPath: path, at: now.Add(20 * time.Second), eventType: "ADDED", body: other},
	}
	h := &Handler{Store: buildV2WatchStore(t, events, now.Add(-time.Minute)), At: now.Add(time.Minute)}

	var all ObjectHistoryResponse
	if code := getJSONInto(t, h, h.serveObjectHistory, "/v2/api/object-history", "?path="+url.QueryEscape(path), &all); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if all.Total != 3 {
		t.Errorf("Total = %d, want 3 (all events for path)", all.Total)
	}

	var web ObjectHistoryResponse
	q := "?path=" + url.QueryEscape(path) + "&name=web"
	if code := getJSONInto(t, h, h.serveObjectHistory, "/v2/api/object-history", q, &web); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if web.Total != 2 {
		t.Errorf("filtered Total = %d, want 2 (web only)", web.Total)
	}
	// Events are sorted newest-first, so web's MODIFIED (at +10s) leads.
	if len(web.Events) > 0 && web.Events[0].EventType != "MODIFIED" {
		t.Errorf("first event type = %q, want MODIFIED (descending sort)", web.Events[0].EventType)
	}
}

func TestServeObjectHistory_BadRequest(t *testing.T) {
	h := newFleetTestHandler(t)
	if code := getJSONInto(t, h, h.serveObjectHistory, "/v2/api/object-history", "", nil); code != http.StatusBadRequest {
		t.Errorf("missing path: status = %d, want 400", code)
	}
}
