package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const podsPath = "/api/v1/namespaces/default/pods"

// newWritableServer builds a writable replay handler over the given store/clock.
func newWritableServer(t *testing.T, store *CaptureStore, clock *ReplayClock) *httptest.Server {
	t.Helper()
	h := newHandler(store, time.Time{}, false)
	h.clock = clock
	h.overlay = newOverlay()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func doReq(t *testing.T, method, url, ctype, body string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func bodyRV(t *testing.T, b []byte) string {
	t.Helper()
	return metaString(b, "resourceVersion")
}

func listNames(t *testing.T, b []byte) []string {
	t.Helper()
	var l struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatalf("decode list: %v\n%s", err, b)
	}
	var names []string
	for _, it := range l.Items {
		names = append(names, metaString(it, "name"))
	}
	return names
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func writableTestStore(t *testing.T, from time.Time) *CaptureStore {
	return buildTestStoreWithWatch(t,
		map[string]watchTestRecord{podsPath: {id: "s", at: from, body: podList("pod-base")}},
		nil)
}

func TestOverlay_CreateGetList(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// Create pod-new.
	code, body := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-new"))
	if code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", code, body)
	}
	if rv := bodyRV(t, body); rv == "" || rv == "0" {
		t.Errorf("created object rv = %q, want non-zero", rv)
	}
	if metaString(body, "uid") == "" || metaString(body, "creationTimestamp") == "" {
		t.Errorf("created object missing uid/creationTimestamp: %s", body)
	}

	// GET the object.
	code, got := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-new", "", "")
	if code != 200 || metaString(got, "name") != "pod-new" {
		t.Fatalf("GET pod-new: status %d name %q", code, metaString(got, "name"))
	}

	// LIST includes both the replay base and the overlay object.
	code, list := doReq(t, http.MethodGet, srv.URL+podsPath, "", "")
	if code != 200 {
		t.Fatalf("list: status %d", code)
	}
	names := listNames(t, list)
	if !contains(names, "pod-base") || !contains(names, "pod-new") {
		t.Errorf("list = %v, want both pod-base and pod-new", names)
	}
}

func TestOverlay_ReplaceAndPatch(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-x"))

	// PUT replace with a label.
	replaced := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-x","namespace":"default","labels":{"team":"a"}}}`
	code, body := doReq(t, http.MethodPut, srv.URL+podsPath+"/pod-x", "application/json", replaced)
	if code != 200 {
		t.Fatalf("put: status %d: %s", code, body)
	}

	// JSON merge patch adds another label.
	code, patched := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-x",
		"application/merge-patch+json", `{"metadata":{"labels":{"tier":"web"}}}`)
	if code != 200 {
		t.Fatalf("patch: status %d: %s", code, patched)
	}
	var obj struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(patched, &obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if obj.Metadata.Labels["team"] != "a" || obj.Metadata.Labels["tier"] != "web" {
		t.Errorf("merged labels = %v, want team=a tier=web", obj.Metadata.Labels)
	}
}

func TestOverlay_DeleteTombstone(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// Delete a replay-base object → tombstone; GET 404, LIST excludes it.
	code, _ := doReq(t, http.MethodDelete, srv.URL+podsPath+"/pod-base", "", "")
	if code != 200 {
		t.Fatalf("delete: status %d", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-base", "", ""); code != 404 {
		t.Errorf("GET after delete: status %d, want 404", code)
	}
	_, list := doReq(t, http.MethodGet, srv.URL+podsPath, "", "")
	if contains(listNames(t, list), "pod-base") {
		t.Errorf("list still contains deleted pod-base: %v", listNames(t, list))
	}
}

func TestOverlay_WinsOverReplayAndRVMonotonic(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// Overwrite the replay-base object; the list must show the overlay copy.
	_, b1 := doReq(t, http.MethodPut, srv.URL+podsPath+"/pod-base", "application/json",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-base","namespace":"default","labels":{"owned":"yes"}}}`)
	_, b2 := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-2"))
	rv1, rv2 := bodyRV(t, b1), bodyRV(t, b2)
	if rv1 == "" || rv2 == "" || !(rv2 > rv1) { // string compare ok for equal-width ints here
		t.Errorf("RVs not monotonic: rv1=%q rv2=%q", rv1, rv2)
	}

	code, got := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-base", "", "")
	if code != 200 {
		t.Fatalf("get pod-base: %d", code)
	}
	if !strings.Contains(string(got), `"owned":"yes"`) {
		t.Errorf("overlay did not win for pod-base: %s", got)
	}
}

func TestOverlay_ResetOnLoop(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, advance := newTestClock(t, from, from.Add(10*time.Second), 1, true /*loop*/, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-tmp"))
	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-tmp", "", ""); code != 200 {
		t.Fatalf("pod-tmp should exist before loop: %d", code)
	}

	advance(15 * time.Second) // cross the window end → loop wrap (epoch advances)

	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-tmp", "", ""); code != 404 {
		t.Errorf("pod-tmp should be cleared after loop wrap, got status %d", code)
	}
}

func TestOverlay_ManualReset(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-tmp"))
	code, _ := doReq(t, http.MethodPost, srv.URL+"/_k8shark/replay/reset-overlay", "", "")
	if code != 200 {
		t.Fatalf("reset-overlay: status %d", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-tmp", "", ""); code != 404 {
		t.Errorf("pod-tmp should be gone after manual reset, got %d", code)
	}
}

func TestOverlay_ReadOnlyRejectsWrites(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	// Read-only replay handler (no overlay).
	h := newHandler(writableTestStore(t, from), time.Time{}, false)
	h.clock = clock
	srv := httptest.NewServer(h)
	defer srv.Close()

	code, _ := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("nope"))
	if code != http.StatusMethodNotAllowed {
		t.Errorf("read-only POST: status %d, want 405", code)
	}
}
