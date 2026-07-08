package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newControlTestServer(t *testing.T, loop bool) (*httptest.Server, *ReplayClock, func(time.Duration)) {
	t.Helper()
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	to := from.Add(60 * time.Second)
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			"/api/v1/namespaces/default/pods": {id: "s", at: from, body: emptyPodList},
		}, nil)
	clock, advance := newTestClock(t, from, to, 1, loop, false)
	h := newHandler(store, time.Time{}, false)
	h.clock = clock
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, clock, advance
}

// doControl issues a control request and decodes the status response.
func doControl(t *testing.T, srv *httptest.Server, method, path string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: status %d: %s", method, path, resp.StatusCode, body)
	}
	var status map[string]any
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("decode status: %v\n%s", err, body)
	}
	return status
}

func TestReplayControl_StatusPauseResume(t *testing.T) {
	srv, _, _ := newControlTestServer(t, false)

	st := doControl(t, srv, http.MethodGet, "/_k8shark/replay")
	if st["paused"] != false {
		t.Errorf("initial paused = %v, want false", st["paused"])
	}
	if st["total_seconds"].(float64) != 60 {
		t.Errorf("total_seconds = %v, want 60", st["total_seconds"])
	}

	st = doControl(t, srv, http.MethodPost, "/_k8shark/replay/pause")
	if st["paused"] != true {
		t.Errorf("after pause: paused = %v, want true", st["paused"])
	}

	st = doControl(t, srv, http.MethodPost, "/_k8shark/replay/play")
	if st["paused"] != false {
		t.Errorf("after play: paused = %v, want false", st["paused"])
	}
}

func TestReplayControl_Speed(t *testing.T) {
	srv, clock, _ := newControlTestServer(t, false)
	st := doControl(t, srv, http.MethodPost, "/_k8shark/replay/speed?value=2.5x")
	if st["speed"].(float64) != 2.5 {
		t.Errorf("speed = %v, want 2.5", st["speed"])
	}
	if clock.Speed() != 2.5 {
		t.Errorf("clock speed = %v, want 2.5", clock.Speed())
	}
}

func TestReplayControl_Seek(t *testing.T) {
	srv, clock, _ := newControlTestServer(t, false)
	from, _ := clock.Window()

	// Seek by offset from the window start.
	st := doControl(t, srv, http.MethodPost, "/_k8shark/replay/seek?offset=30s")
	if got, want := st["position"].(string), from.Add(30*time.Second).Format(time.RFC3339); got != want {
		t.Errorf("after offset seek: position = %s, want %s", got, want)
	}

	// Seek to an absolute RFC3339 time.
	target := from.Add(10 * time.Second).Format(time.RFC3339)
	st = doControl(t, srv, http.MethodPost, "/_k8shark/replay/seek?to="+target)
	if got := st["position"].(string); got != target {
		t.Errorf("after absolute seek: position = %s, want %s", got, target)
	}
}

func TestReplayControl_Errors(t *testing.T) {
	srv, _, _ := newControlTestServer(t, false)

	// Wrong method.
	resp, err := http.Get(srv.URL + "/_k8shark/replay/pause")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET pause: status = %d, want 405", resp.StatusCode)
	}

	// Bad speed.
	resp, err = http.Post(srv.URL+"/_k8shark/replay/speed?value=nope", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad speed: status = %d, want 400", resp.StatusCode)
	}

	// Seek with no target.
	resp, err = http.Post(srv.URL+"/_k8shark/replay/seek", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty seek: status = %d, want 400", resp.StatusCode)
	}

	// Unknown action.
	resp, err = http.Post(srv.URL+"/_k8shark/replay/frobnicate", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown action: status = %d, want 404", resp.StatusCode)
	}
}
