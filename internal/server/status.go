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

// statusObj builds a generic Kubernetes Status object for an error response.
// Includes reason (when code maps to one) alongside code — client-go's
// apierrors helpers (IsForbidden, IsBadRequest, IsMethodNotSupported,
// IsInternalError, ...) key off reason, not code, the same requirement
// notFoundStatus already handles for 404 (#177). Any controller or operator
// replayed against the mock depends on this to handle errors correctly.
func statusObj(code int, msg string) map[string]any {
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "Status",
		"status":     "Failure",
		"message":    msg,
		"code":       code,
	}
	if reason := statusReasonForCode(code); reason != "" {
		obj["reason"] = reason
	}
	return obj
}

// statusReasonForCode maps an HTTP status code to the metav1.StatusReason
// string a real apiserver would set, for the status codes this mock server
// actually returns (see statusObj callers). Codes with no well-known reason
// return "" (metav1.StatusReasonUnknown), which statusObj treats as "omit
// the reason field from the JSON body" rather than emitting reason:"" —
// matching upstream, where metav1.Status.Reason is `json:",omitempty"` too.
//
// Deliberately excludes http.StatusNotFound. A plain writeStatus/statusObj
// 404 is used for two different things, and only one of them should carry
// reason: "NotFound":
//   - A resource genuinely unknown to the capture (wrong group/resource
//     entirely, not in discovery or the index) — this must NOT carry
//     reason: "NotFound", or apierrors.IsNotFound() would treat a
//     capture-config problem as an ordinary "object doesn't exist" (#177).
//   - An ordinary missing object on a known resource (e.g. PUT/PATCH/DELETE
//     against a name that doesn't exist in the overlay or replay state) —
//     these should carry reason: "NotFound", so call sites for this case
//     use the dedicated notFoundStatus instead of statusObj, which sets
//     reason (and details) itself.
//
// If a new writeStatus(w, http.StatusNotFound, ...) call site is added,
// decide which of the two it is and use notFoundStatus if it's the latter —
// don't assume every 404 belongs to the "genuinely unknown" case above.
//
// Also deliberately excludes http.StatusGone: the one 410 this mock server
// produces (a stale watch resourceVersion, replay_rv.go's writeGone) is
// reason: "Expired" per real kube-apiserver semantics — not the generic
// "Gone" — and it builds its own Status map directly rather than calling
// statusObj. Mapping 410 to "Gone" here would be wrong the moment any
// future statusObj(410, ...) call site is added for that case.
func statusReasonForCode(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "BadRequest"
	case http.StatusUnauthorized:
		return "Unauthorized"
	case http.StatusForbidden:
		return "Forbidden"
	case http.StatusConflict:
		return "Conflict"
	case http.StatusUnprocessableEntity:
		return "Invalid"
	case http.StatusMethodNotAllowed:
		return "MethodNotAllowed"
	case http.StatusNotAcceptable:
		return "NotAcceptable"
	case http.StatusRequestEntityTooLarge:
		return "RequestEntityTooLarge"
	case http.StatusUnsupportedMediaType:
		return "UnsupportedMediaType"
	case http.StatusInternalServerError:
		return "InternalError"
	case http.StatusServiceUnavailable:
		return "ServiceUnavailable"
	case http.StatusGatewayTimeout:
		return "Timeout"
	case http.StatusTooManyRequests:
		return "TooManyRequests"
	default:
		return ""
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
