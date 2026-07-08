package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// replayControlPrefix is the reserved path under which the replay transport
// controls are served. It cannot collide with the Kubernetes API, which lives
// under /api and /apis.
const replayControlPrefix = "/_k8shark/replay"

// handleReplayControl implements the replay transport-control API. A successful
// request returns the current replay status as JSON so a client (CLI, UI
// scrubber, script) sees the resulting state; an invalid request returns a
// Kubernetes-style Status JSON body with the appropriate code (405/400/404).
//
//	GET  /_k8shark/replay              → current status
//	POST /_k8shark/replay/pause        → pause the clock
//	POST /_k8shark/replay/play         → resume the clock
//	POST /_k8shark/replay/speed?value= → set speed (e.g. 2x, 0.5x)
//	POST /_k8shark/replay/seek?to=     → seek to an RFC3339 time or duration
//	                          ?offset= →   relative to the window end / start
func (h *handler) handleReplayControl(w http.ResponseWriter, r *http.Request, path string) {
	clock := h.clock
	action := strings.Trim(strings.TrimPrefix(path, replayControlPrefix), "/")

	switch action {
	case "":
		if !h.requireMethod(w, r, http.MethodGet) {
			return
		}
	case "pause":
		if !h.requireMethod(w, r, http.MethodPost) {
			return
		}
		clock.Pause()
	case "play", "resume":
		if !h.requireMethod(w, r, http.MethodPost) {
			return
		}
		clock.Resume()
	case "speed":
		if !h.requireMethod(w, r, http.MethodPost) {
			return
		}
		s, err := parseSpeed(r.URL.Query().Get("value"))
		if err != nil {
			h.writeStatus(w, http.StatusBadRequest, err.Error())
			return
		}
		clock.SetSpeed(s)
	case "seek":
		if !h.requireMethod(w, r, http.MethodPost) {
			return
		}
		target, err := parseSeekTarget(clock, r.URL.Query())
		if err != nil {
			h.writeStatus(w, http.StatusBadRequest, err.Error())
			return
		}
		clock.Seek(target)
	default:
		h.writeStatus(w, http.StatusNotFound, fmt.Sprintf("unknown replay control %q", action))
		return
	}

	writeJSON(w, http.StatusOK, replayStatus(clock))
}

// requireMethod writes a 405 Status JSON (with an Allow header) and returns
// false if the request method doesn't match, keeping control-API error
// responses consistently JSON.
func (h *handler) requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	h.writeStatus(w, http.StatusMethodNotAllowed, fmt.Sprintf("%s required for this replay control", method))
	return false
}

// parseSeekTarget resolves a seek target from the query. `to` is an RFC3339
// timestamp or a duration relative to the window end (e.g. -2m); `offset` is a
// duration from the window start (e.g. 90s). The clock clamps to the window.
func parseSeekTarget(clock *ReplayClock, q url.Values) (time.Time, error) {
	from, to := clock.Window()
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t, nil
		}
		if d, err := time.ParseDuration(v); err == nil {
			return to.Add(d), nil
		}
		return time.Time{}, fmt.Errorf("invalid seek to %q: use RFC3339 or a duration like -2m", v)
	}
	if v := q.Get("offset"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid seek offset %q: use a duration like 90s", v)
		}
		return from.Add(d), nil
	}
	return time.Time{}, fmt.Errorf("seek requires ?to= (RFC3339 or duration) or ?offset= (duration)")
}

// replayStatus builds the status map returned by every control-API response.
func replayStatus(clock *ReplayClock) map[string]any {
	from, to := clock.Window()
	pos, epoch, ended := clock.Sample()
	return map[string]any{
		"position":        pos.Format(time.RFC3339),
		"from":            from.Format(time.RFC3339),
		"to":              to.Format(time.RFC3339),
		"elapsed_seconds": int64(pos.Sub(from).Round(time.Second) / time.Second),
		"total_seconds":   int64(to.Sub(from).Round(time.Second) / time.Second),
		"speed":           clock.Speed(),
		"paused":          clock.Paused(),
		"loop":            clock.Loop(),
		"ended":           ended,
		"epoch":           epoch,
		"events_emitted":  clock.EventsEmitted(),
	}
}
