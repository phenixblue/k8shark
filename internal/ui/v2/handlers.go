package v2

import (
	"encoding/json"
	"net/http"
)

// writeJSON marshals v and writes it as application/json with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// writeError sends a structured JSON error.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// serveEvents returns events filtered by involvedObject. Used by the pod
// drilldown for its events panel.
func (h *Handler) serveEvents(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	ns := r.URL.Query().Get("ns")
	if ns == "" {
		writeError(w, http.StatusBadRequest, "missing ns query parameter")
		return
	}
	kind := r.URL.Query().Get("kind")
	name := r.URL.Query().Get("name")
	at := h.resolveAt(r)

	listPath := "/api/v1/namespaces/" + ns + "/events"
	body, code, err := h.Store.ReconstructAt(listPath, at)
	if err != nil || code != 200 || len(body) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"events": []any{}, "namespace": ns})
		return
	}
	var list struct {
		Items []eventObject `json:"items"`
	}
	if err := jsonUnmarshalBody(body, &list); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var out []PodEvent
	for _, ev := range list.Items {
		if kind != "" && ev.InvolvedObject.Kind != kind {
			continue
		}
		if name != "" && ev.InvolvedObject.Name != name {
			continue
		}
		t := ev.LastTimestamp
		if t.IsZero() {
			t = ev.EventTime
		}
		if t.IsZero() {
			t = ev.FirstTimestamp
		}
		sev := "normal"
		if ev.Type == "Warning" {
			sev = "warn"
		}
		if isBadEventReason(ev.Reason) {
			sev = "bad"
		}
		out = append(out, PodEvent{
			Severity: sev,
			Reason:   ev.Reason,
			Message:  ev.Message,
			Time:     t.UTC().Format("2006-01-02T15:04:05Z"),
			Count:    ev.Count,
			Source:   ev.Source.Component,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"namespace": ns, "events": out})
}

func jsonUnmarshalBody(b []byte, v any) error { return json.Unmarshal(b, v) }

// serveTimestamps delegates to the scrubber timestamp endpoint defined in
// timestamps.go.
func (h *Handler) serveTimestamps(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	h.timestampsHandler(w, r)
}
