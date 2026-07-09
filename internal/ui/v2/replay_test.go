package v2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/server"
)

func TestServeReplay_DisabledWithoutClock(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.serveReplay(rec, httptest.NewRequest(http.MethodGet, "/v2/api/replay", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["enabled"] != false {
		t.Errorf("enabled = %v, want false", m["enabled"])
	}
}

func TestServeReplay_Control(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	to := from.Add(60 * time.Second)
	h := &Handler{Clock: server.NewReplayClock(from, to, 1, false, false)}

	call := func(method, path string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		h.serveReplay(rec, httptest.NewRequest(method, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d: %s", method, path, rec.Code, rec.Body)
		}
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return m
	}

	if st := call(http.MethodGet, "/v2/api/replay"); st["enabled"] != true {
		t.Errorf("enabled = %v, want true", st["enabled"])
	}
	if st := call(http.MethodPost, "/v2/api/replay/pause"); st["paused"] != true {
		t.Errorf("after pause: paused = %v, want true", st["paused"])
	}
	if st := call(http.MethodPost, "/v2/api/replay/play"); st["paused"] != false {
		t.Errorf("after play: paused = %v, want false", st["paused"])
	}
	if st := call(http.MethodPost, "/v2/api/replay/speed?value=2.5x"); st["speed"].(float64) != 2.5 {
		t.Errorf("speed = %v, want 2.5", st["speed"])
	}

	// Seek (pause first for a deterministic position).
	call(http.MethodPost, "/v2/api/replay/pause")
	target := from.Add(30 * time.Second).Format(time.RFC3339)
	if st := call(http.MethodPost, "/v2/api/replay/seek?to="+target); st["position"] != target {
		t.Errorf("after seek: position = %v, want %s", st["position"], target)
	}
}

func TestServeReplay_Errors(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	h := &Handler{Clock: server.NewReplayClock(from, from.Add(time.Minute), 1, false, false)}

	// Bad speed → 400.
	rec := httptest.NewRecorder()
	h.serveReplay(rec, httptest.NewRequest(http.MethodPost, "/v2/api/replay/speed?value=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad speed: status = %d, want 400", rec.Code)
	}

	// Seek with no target → 400.
	rec = httptest.NewRecorder()
	h.serveReplay(rec, httptest.NewRequest(http.MethodPost, "/v2/api/replay/seek", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty seek: status = %d, want 400", rec.Code)
	}

	// Unknown action → 404.
	rec = httptest.NewRecorder()
	h.serveReplay(rec, httptest.NewRequest(http.MethodPost, "/v2/api/replay/frobnicate", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown action: status = %d, want 404", rec.Code)
	}
}

// TestResolveAt_FollowsClock verifies resolveAt returns the clock position in
// replay mode when no explicit ?at= is given.
func TestResolveAt_FollowsClock(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock := server.NewReplayClock(from, from.Add(time.Hour), 1, false, true) // paused at from
	h := &Handler{Clock: clock}

	got := h.resolveAt(httptest.NewRequest(http.MethodGet, "/v2/api/overview", nil))
	if !got.Equal(from) {
		t.Errorf("resolveAt (no ?at) = %s, want clock position %s", got, from)
	}
	// Explicit ?at= still wins.
	at := from.Add(10 * time.Minute).Format(time.RFC3339)
	got = h.resolveAt(httptest.NewRequest(http.MethodGet, "/v2/api/overview?at="+at, nil))
	if got.Format(time.RFC3339) != at {
		t.Errorf("resolveAt (?at=%s) = %s, want the explicit value", at, got.Format(time.RFC3339))
	}
}
