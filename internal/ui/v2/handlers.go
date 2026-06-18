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

// Stubs for endpoints that land in later commits on this branch.

func (h *Handler) serveEvents(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "events not implemented yet")
}

func (h *Handler) serveLogs(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "logs not implemented yet")
}

func (h *Handler) serveDiff(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "diff not implemented yet")
}

func (h *Handler) serveObjectHistory(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotImplemented, "object-history not implemented yet")
}

func (h *Handler) serveTimestamps(w http.ResponseWriter, r *http.Request) {
	h.timestampsHandler(w, r)
}
