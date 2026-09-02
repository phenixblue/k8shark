package v2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestServeCaptureInfo(t *testing.T) {
	h := newObjectTestHandler(t) // reuses the shared 2-record test store
	req := httptest.NewRequest(http.MethodGet, "/v2/api/capture", nil)
	w := httptest.NewRecorder()
	h.serveCaptureInfo(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var info CaptureInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.CaptureID != "v2-obj-test" {
		t.Errorf("CaptureID = %q, want v2-obj-test", info.CaptureID)
	}
	if info.RecordCount != 2 {
		t.Errorf("RecordCount = %d, want 2", info.RecordCount)
	}
	if info.DurationSeconds != 300 {
		t.Errorf("DurationSeconds = %d, want 300", info.DurationSeconds)
	}
	// Two distinct resource paths (pods, replicasets) were indexed.
	if info.ResourcePaths != 2 {
		t.Errorf("ResourcePaths = %d, want 2", info.ResourcePaths)
	}
}

func TestServeCaptureInfo_AnonymizedProvenance(t *testing.T) {
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	podList := `{"apiVersion":"v1","kind":"PodList","items":[]}`
	recs := []*capture.Record{
		{ID: "r1", CapturedAt: now, APIPath: "/api/v1/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podList)},
	}
	idx := capture.Index{"/api/v1/pods": {APIPath: "/api/v1/pods", Seqs: []int{0}, Times: []time.Time{now}}}
	meta := &capture.CaptureMetadata{
		CaptureID: "v2-anonymized-test", CapturedAt: now.Add(-time.Minute), CapturedUntil: now, RecordCount: 1,
		Anonymized: true, AnonymizedCategories: []string{"namespace", "ip"},
	}
	h := &Handler{Store: buildV2TestStore(t, recs, idx, meta), At: now}

	req := httptest.NewRequest(http.MethodGet, "/v2/api/capture", nil)
	w := httptest.NewRecorder()
	h.serveCaptureInfo(w, req)

	var info CaptureInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !info.Anonymized {
		t.Error("Anonymized = false, want true")
	}
	want := []string{"namespace", "ip"}
	if len(info.AnonymizedCategories) != len(want) {
		t.Fatalf("AnonymizedCategories = %v, want %v", info.AnonymizedCategories, want)
	}
	for i := range want {
		if info.AnonymizedCategories[i] != want[i] {
			t.Errorf("AnonymizedCategories[%d] = %q, want %q", i, info.AnonymizedCategories[i], want[i])
		}
	}
}

func TestServeCaptureInfo_NilStore(t *testing.T) {
	h := &Handler{} // no store
	req := httptest.NewRequest(http.MethodGet, "/v2/api/capture", nil)
	w := httptest.NewRecorder()
	h.serveCaptureInfo(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}
