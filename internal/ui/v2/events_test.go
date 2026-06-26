package v2

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

func newEventsTestHandler(t *testing.T) *Handler {
	t.Helper()
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	events := `{"apiVersion":"v1","kind":"EventList","items":[` +
		`{"reason":"BackOff","message":"Back-off restarting failed container","type":"Warning","count":7,` +
		`"lastTimestamp":"2026-04-10T10:00:00Z","source":{"component":"kubelet"},` +
		`"involvedObject":{"kind":"Pod","name":"crasher"}},` +
		`{"reason":"Scheduled","message":"Successfully assigned default/web","type":"Normal","count":1,` +
		`"lastTimestamp":"2026-04-10T09:59:00Z","source":{"component":"default-scheduler"},` +
		`"involvedObject":{"kind":"Pod","name":"web"}}]}`
	recs := []*capture.Record{
		{ID: "e1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/events", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(events)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/events": {APIPath: "/api/v1/namespaces/default/events", Seqs: []int{0}, Times: []time.Time{now}, Counts: []int{2}},
	}
	meta := &capture.CaptureMetadata{CaptureID: "events-test", CapturedAt: now.Add(-5 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	return &Handler{Store: buildV2TestStore(t, recs, idx, meta), At: now}
}

func TestServeEvents(t *testing.T) {
	h := newEventsTestHandler(t)
	var resp struct {
		Namespace string     `json:"namespace"`
		Events    []PodEvent `json:"events"`
	}
	if code := getJSONInto(t, h, h.serveEvents, "/v2/api/events", "?ns=default", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.Namespace != "default" {
		t.Errorf("namespace = %q, want default", resp.Namespace)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(resp.Events))
	}
	var backoff *PodEvent
	for i := range resp.Events {
		if resp.Events[i].Reason == "BackOff" {
			backoff = &resp.Events[i]
		}
	}
	if backoff == nil {
		t.Fatalf("no BackOff event in %+v", resp.Events)
	}
	if backoff.Severity != "bad" {
		t.Errorf("BackOff severity = %q, want bad", backoff.Severity)
	}
	if backoff.Count != 7 {
		t.Errorf("BackOff count = %d, want 7", backoff.Count)
	}
	if backoff.Source != "kubelet" {
		t.Errorf("BackOff source = %q, want kubelet", backoff.Source)
	}
}

func TestServeEvents_FilterAndBadRequest(t *testing.T) {
	h := newEventsTestHandler(t)
	var resp struct {
		Events []PodEvent `json:"events"`
	}
	if code := getJSONInto(t, h, h.serveEvents, "/v2/api/events", "?ns=default&kind=Pod&name=web", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(resp.Events) != 1 || resp.Events[0].Reason != "Scheduled" {
		t.Errorf("expected only web's Scheduled event, got %+v", resp.Events)
	}
	if code := getJSONInto(t, h, h.serveEvents, "/v2/api/events", "", nil); code != http.StatusBadRequest {
		t.Errorf("missing ns: status = %d, want 400", code)
	}
}
