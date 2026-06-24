package v2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestServeCaptureInfo_NilStore(t *testing.T) {
	h := &Handler{} // no store
	req := httptest.NewRequest(http.MethodGet, "/v2/api/capture", nil)
	w := httptest.NewRecorder()
	h.serveCaptureInfo(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}
