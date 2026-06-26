package v2

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestServeLogs(t *testing.T) {
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	// Captured logs are keyed by the log path plus a ?container= suffix, and the
	// body is a JSON-encoded string of the log text.
	logKey := "/api/v1/namespaces/default/pods/web/log?container=app"
	body, err := json.Marshal("line one\nline two\n")
	if err != nil {
		t.Fatalf("marshal log body: %v", err)
	}
	recs := []*capture.Record{
		{ID: "l1", CapturedAt: now, APIPath: logKey, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body)},
	}
	idx := capture.Index{logKey: {APIPath: logKey, Seqs: []int{0}, Times: []time.Time{now}}}
	meta := &capture.CaptureMetadata{CaptureID: "logs-test", CapturedAt: now.Add(-time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	h := &Handler{Store: buildV2TestStore(t, recs, idx, meta), At: now}

	var resp LogsResponse
	if code := getJSONInto(t, h, h.serveLogs, "/v2/api/logs", "?ns=default&pod=web", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.Container != "app" {
		t.Errorf("Container = %q, want app (auto-selected)", resp.Container)
	}
	if !strings.Contains(resp.Text, "line one") {
		t.Errorf("Text = %q, want it to contain the log lines", resp.Text)
	}
	if resp.LineCount != 2 {
		t.Errorf("LineCount = %d, want 2", resp.LineCount)
	}
	if len(resp.Containers) != 1 || resp.Containers[0].Name != "app" || !resp.Containers[0].HasCurrent {
		t.Errorf("Containers = %+v", resp.Containers)
	}
}

func TestServeLogs_Errors(t *testing.T) {
	h := newFleetTestHandler(t)
	if code := getJSONInto(t, h, h.serveLogs, "/v2/api/logs", "?ns=default", nil); code != http.StatusBadRequest {
		t.Errorf("missing pod: status = %d, want 400", code)
	}
	// The fleet store has no captured log records, so this is a 404.
	if code := getJSONInto(t, h, h.serveLogs, "/v2/api/logs", "?ns=default&pod=web", nil); code != http.StatusNotFound {
		t.Errorf("no captured logs: status = %d, want 404", code)
	}
}
