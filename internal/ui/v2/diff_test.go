package v2

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestServeDiff(t *testing.T) {
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	t1 := now.Add(time.Minute)
	path := "/api/v1/namespaces/default/configmaps"
	before := `{"apiVersion":"v1","kind":"ConfigMapList","items":[{"metadata":{"name":"cfg","namespace":"default"},"data":{"key":"before-value"}}]}`
	after := `{"apiVersion":"v1","kind":"ConfigMapList","items":[{"metadata":{"name":"cfg","namespace":"default"},"data":{"key":"after-value"}}]}`
	recs := []*capture.Record{
		{ID: "c0", CapturedAt: now, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(before)},
		{ID: "c1", CapturedAt: t1, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(after)},
	}
	idx := capture.Index{path: {APIPath: path, Seqs: []int{0, 1}, Times: []time.Time{now, t1}, Counts: []int{1, 1}}}
	meta := &capture.CaptureMetadata{CaptureID: "diff-test", CapturedAt: now.Add(-time.Minute), CapturedUntil: t1, RecordCount: len(recs)}
	h := &Handler{Store: buildV2TestStore(t, recs, idx, meta), At: t1}

	q := "?path=" + url.QueryEscape(path) + "&name=cfg" +
		"&before=" + url.QueryEscape(now.Format(time.RFC3339)) +
		"&after=" + url.QueryEscape(t1.Format(time.RFC3339))
	var resp DiffResponse
	if code := getJSONInto(t, h, h.serveDiff, "/v2/api/diff", q, &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.BeforeMissing || resp.AfterMissing {
		t.Fatalf("snapshots should both be present: before=%v after=%v", resp.BeforeMissing, resp.AfterMissing)
	}
	if !resp.HasDiff {
		t.Errorf("HasDiff = false, want true")
	}
	var joined strings.Builder
	for _, hk := range resp.Hunks {
		joined.WriteString(hk.Text)
		joined.WriteString("\n")
	}
	text := joined.String()
	if !strings.Contains(text, "before-value") || !strings.Contains(text, "after-value") {
		t.Errorf("expected before/after values in diff hunks:\n%s", text)
	}
}

func TestServeDiff_Errors(t *testing.T) {
	h := newFleetTestHandler(t)
	if code := getJSONInto(t, h, h.serveDiff, "/v2/api/diff", "", nil); code != http.StatusBadRequest {
		t.Errorf("missing path: status = %d, want 400", code)
	}
	q := "?path=" + url.QueryEscape("/api/v1/namespaces/default/pods") + "&before=not-a-timestamp"
	if code := getJSONInto(t, h, h.serveDiff, "/v2/api/diff", q, nil); code != http.StatusBadRequest {
		t.Errorf("invalid before timestamp: status = %d, want 400", code)
	}
}
