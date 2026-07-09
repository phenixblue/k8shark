package v2

import (
	"net/http"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/server"
)

// serveReplay is the dashboard's transport-control API, backed by the shared
// replay clock. A successful request returns the current status as JSON; when
// the dashboard is not in replay mode it returns {"enabled": false} so the UI
// can hide the transport bar.
//
//	GET  /v2/api/replay              → status
//	POST /v2/api/replay/pause        → pause
//	POST /v2/api/replay/play         → resume
//	POST /v2/api/replay/speed?value= → set speed (2x, 0.5x, …)
//	POST /v2/api/replay/seek?to=     → seek to an RFC3339 time
//	                          ?offset=→   or a duration from the window start
func (h *Handler) serveReplay(w http.ResponseWriter, r *http.Request) {
	action := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v2/api/replay"), "/")

	if h.Clock == nil {
		// GET status reports "not in replay mode"; any control action is a 404 so
		// a non-UI client doesn't see a mutating call succeed as a silent no-op.
		if action == "" && r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"enabled": false, "error": "replay mode is not active"})
		return
	}
	clock := h.Clock

	switch action {
	case "":
		// status — read-only, GET only.
		if !replayRequireMethod(w, r, http.MethodGet) {
			return
		}
	case "pause":
		if !replayRequireMethod(w, r, http.MethodPost) {
			return
		}
		clock.Pause()
	case "play", "resume":
		if !replayRequireMethod(w, r, http.MethodPost) {
			return
		}
		clock.Resume()
	case "speed":
		if !replayRequireMethod(w, r, http.MethodPost) {
			return
		}
		s, err := server.ParseSpeed(r.URL.Query().Get("value"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		clock.SetSpeed(s)
	case "seek":
		if !replayRequireMethod(w, r, http.MethodPost) {
			return
		}
		target, err := replaySeekTarget(clock, r.URL.Query())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		clock.Seek(target)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown replay control " + action})
		return
	}

	writeJSON(w, http.StatusOK, replayStatus(clock))
}

// replayRequireMethod enforces the HTTP method for a control action so that
// state-changing actions can't be triggered by a GET / link navigation.
func replayRequireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": method + " required"})
	return false
}

// replaySeekTarget resolves a seek target from the query: ?to= is an RFC3339
// timestamp; ?offset= is a duration from the window start (e.g. 90s). The clock
// clamps to the window.
func replaySeekTarget(clock *server.ReplayClock, q map[string][]string) (time.Time, error) {
	from, _ := clock.Window()
	if v := firstQuery(q, "to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, err
		}
		return t, nil
	}
	if v := firstQuery(q, "offset"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return time.Time{}, err
		}
		return from.Add(d), nil
	}
	return time.Time{}, errSeekTarget
}

var errSeekTarget = &seekErr{}

type seekErr struct{}

func (*seekErr) Error() string { return "seek requires ?to= (RFC3339) or ?offset= (duration)" }

func firstQuery(q map[string][]string, key string) string {
	if vs := q[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// replayStatus builds the JSON status describing the clock's current position.
func replayStatus(clock *server.ReplayClock) map[string]any {
	from, to := clock.Window()
	pos, epoch, ended := clock.Sample()
	return map[string]any{
		"enabled":         true,
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
