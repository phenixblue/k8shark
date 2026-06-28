package v2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/diagnose"
)

func TestServeDiagnostics(t *testing.T) {
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	pods := `{"kind":"PodList","apiVersion":"v1","items":[` +
		`{"metadata":{"name":"web"},"status":{"phase":"Running","containerStatuses":[{"name":"app","ready":false,"restartCount":7,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}]}`
	recs := []*capture.Record{
		{ID: "p1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(pods)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/pods": {APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}, Times: []time.Time{now}},
	}
	meta := &capture.CaptureMetadata{CaptureID: "diag-ui", CapturedAt: now, CapturedUntil: now, RecordCount: 1}
	h := &Handler{Store: buildV2TestStore(t, recs, idx, meta), At: now}

	req := httptest.NewRequest(http.MethodGet, "/v2/api/diagnostics", nil)
	w := httptest.NewRecorder()
	h.serveDiagnostics(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var rep diagnose.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rep.SchemaVersion != diagnose.SchemaVersion {
		t.Errorf("schema_version = %d", rep.SchemaVersion)
	}
	found := false
	for _, f := range rep.Findings {
		if f.RuleID == "pod.crashloopbackoff" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a crashloop finding, got %+v", rep.Findings)
	}
	if rep.Summary.Critical < 1 {
		t.Errorf("expected a critical finding, summary=%+v", rep.Summary)
	}
}

func TestServeDiagnostics_NilStore(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/v2/api/diagnostics", nil)
	w := httptest.NewRecorder()
	h.serveDiagnostics(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}
