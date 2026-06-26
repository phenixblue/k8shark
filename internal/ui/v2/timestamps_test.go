package v2

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestServeTimestamps(t *testing.T) {
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	t1 := now.Add(30 * time.Second)
	pods := `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"web","namespace":"default"}}]}`
	path := "/api/v1/namespaces/default/pods"
	recs := []*capture.Record{
		{ID: "p0", CapturedAt: now, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(pods)},
		{ID: "p1", CapturedAt: t1, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(pods)},
	}
	idx := capture.Index{path: {APIPath: path, Seqs: []int{0, 1}, Times: []time.Time{now, t1}, Counts: []int{1, 1}}}
	meta := &capture.CaptureMetadata{CaptureID: "ts-test", CapturedAt: now.Add(-time.Minute), CapturedUntil: t1, RecordCount: len(recs)}
	h := &Handler{Store: buildV2TestStore(t, recs, idx, meta), At: t1}

	var resp TimestampsResponse
	if code := getJSONInto(t, h, h.serveTimestamps, "/v2/api/timestamps", "", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.TotalCount != 2 {
		t.Errorf("TotalCount = %d, want 2", resp.TotalCount)
	}
	if len(resp.Timestamps) != 2 {
		t.Errorf("len(Timestamps) = %d, want 2", len(resp.Timestamps))
	}
	if resp.Sampled {
		t.Errorf("Sampled = true, want false for only 2 stops")
	}
	if !resp.CapturedUntil.Equal(t1) {
		t.Errorf("CapturedUntil = %v, want %v", resp.CapturedUntil, t1)
	}
}
