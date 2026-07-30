package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

func (h *handler) writeStatus(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, statusObj(code, msg))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func statusObj(code int, msg string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Status",
		"status":     "Failure",
		"message":    msg,
		"code":       code,
	}
}

// notFoundStatus builds a standard Kubernetes NotFound Status object for a
// missing item of group/resource/name — reason: "NotFound" (so client-go's
// apierrors.IsNotFound() recognizes it) and the real apiserver's message
// format (e.g. "ingresses.networking.k8s.io \"nope\" not found", or just
// "pods \"nope\" not found" for the core group), matching
// k8s.io/apimachinery/pkg/api/errors.NewNotFound (#177). details.kind is the
// plural resource name, not the singular Kind — an apiserver quirk this
// mirrors for parity.
func notFoundStatus(group, resource, name string) map[string]any {
	qualified := resource
	if group != "" {
		qualified = resource + "." + group
	}
	details := map[string]any{"name": name, "kind": resource}
	if group != "" {
		details["group"] = group
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Status",
		"metadata":   map[string]any{},
		"status":     "Failure",
		"message":    fmt.Sprintf("%s %q not found", qualified, name),
		"reason":     "NotFound",
		"details":    details,
		"code":       http.StatusNotFound,
	}
}

// watchTimeout parses ?timeoutSeconds into a channel that fires after the given
// duration, plus a stop function to release the underlying timer when the watch
// ends early (a plain time.After can't be stopped and would linger). An empty or
// non-positive value yields a nil channel (no timeout) and a no-op stop.
func watchTimeout(secs string) (<-chan time.Time, func()) {
	if secs == "" {
		return nil, func() {}
	}
	n, err := strconv.Atoi(secs)
	if err != nil || n <= 0 {
		return nil, func() {}
	}
	t := time.NewTimer(time.Duration(n) * time.Second)
	return t.C, func() {
		// Drain if the timer already fired, so no value stays buffered on the
		// channel keeping the timer reachable.
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
	}
}

// bookmarkResourceVersion returns a non-zero, non-negative resourceVersion for a
// BOOKMARK. It uses the first candidate time with a positive Unix value, falling
// back to wall-clock so watch clients get a sensible RV even for older/corrupt
// archives whose metadata bounds are missing (zero → negative Unix).
func bookmarkResourceVersion(candidates ...time.Time) string {
	for _, t := range candidates {
		if u := t.Unix(); u > 0 {
			return strconv.FormatInt(u, 10)
		}
	}
	return strconv.FormatInt(time.Now().Unix(), 10)
}
