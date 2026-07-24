package v2

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

func newSearchTestHandler(t *testing.T) *Handler {
	t.Helper()
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	pods := `{"kind":"PodList","apiVersion":"v1","items":[` +
		`{"metadata":{"name":"web-1","namespace":"default","annotations":{"note":"needs a db migration"}},"spec":{"containers":[{"image":"nginx:alpine"}]}}]}`
	recs := []*capture.Record{
		{ID: "p1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(pods)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/pods": {APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}, Times: []time.Time{now}},
	}
	meta := &capture.CaptureMetadata{CaptureID: "search-ui", CapturedAt: now, CapturedUntil: now, RecordCount: 1}
	return &Handler{Store: buildV2TestStore(t, recs, idx, meta), At: now}
}

func searchRequest(query string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/v2/api/search?"+query, nil)
}

func TestServeSearch_JSONPathMode(t *testing.T) {
	h := newSearchTestHandler(t)
	w := httptest.NewRecorder()
	h.serveSearch(w, searchRequest("q="+url.QueryEscape("{.spec.containers[*].image}")))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var resp SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Mode != "jsonpath" || resp.Total != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Results[0].Value != `"nginx:alpine"` || resp.Results[0].Name != "web-1" {
		t.Errorf("unexpected result: %+v", resp.Results[0])
	}
}

func TestServeSearch_TextMode(t *testing.T) {
	h := newSearchTestHandler(t)
	w := httptest.NewRecorder()
	h.serveSearch(w, searchRequest("mode=text&q="+url.QueryEscape("db migration")))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var resp SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Mode != "text" || resp.Total != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Results[0].Field != "metadata.annotations.note" {
		t.Errorf("unexpected field: %+v", resp.Results[0])
	}
}

func TestServeSearch_RegexMode(t *testing.T) {
	h := newSearchTestHandler(t)
	w := httptest.NewRecorder()
	h.serveSearch(w, searchRequest("mode=regex&q="+url.QueryEscape(`nginx:\w+`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var resp SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Mode != "regex" || resp.Total != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestServeSearch_MissingQuery(t *testing.T) {
	h := newSearchTestHandler(t)
	w := httptest.NewRecorder()
	h.serveSearch(w, searchRequest(""))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestServeSearch_BadMode(t *testing.T) {
	h := newSearchTestHandler(t)
	w := httptest.NewRecorder()
	h.serveSearch(w, searchRequest("mode=bogus&q=x"))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestServeSearch_InvalidRegex(t *testing.T) {
	h := newSearchTestHandler(t)
	w := httptest.NewRecorder()
	h.serveSearch(w, searchRequest("mode=regex&q="+url.QueryEscape("(unclosed")))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestServeSearch_NilStore(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	h.serveSearch(w, searchRequest("q=x"))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestServeSearch_Truncates(t *testing.T) {
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	items := make([]string, 0, maxSearchResults+50)
	for i := 0; i < maxSearchResults+50; i++ {
		items = append(items, fmt.Sprintf(`{"metadata":{"name":"pod-%d","namespace":"default"}}`, i))
	}
	body := `{"kind":"PodList","apiVersion":"v1","items":[` + strings.Join(items, ",") + `]}`
	recs := []*capture.Record{
		{ID: "p1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/pods": {APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}, Times: []time.Time{now}},
	}
	meta := &capture.CaptureMetadata{CaptureID: "search-truncate", CapturedAt: now, CapturedUntil: now, RecordCount: 1}
	h := &Handler{Store: buildV2TestStore(t, recs, idx, meta), At: now}

	w := httptest.NewRecorder()
	h.serveSearch(w, searchRequest("mode=text&q="+url.QueryEscape("pod-")))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var resp SearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Truncated {
		t.Errorf("expected truncated=true for %d matches, got %+v", resp.Total, resp)
	}
	if resp.Total != maxSearchResults+50 {
		t.Errorf("Total = %d, want %d (untruncated count)", resp.Total, maxSearchResults+50)
	}
	if len(resp.Results) != maxSearchResults {
		t.Errorf("len(Results) = %d, want %d (capped)", len(resp.Results), maxSearchResults)
	}
}
