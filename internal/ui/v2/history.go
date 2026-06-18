package v2

import (
	"net/http"
	"sort"
	"time"
)

// ObjectHistoryResponse is what /v2/api/object-history returns — a list of
// all watch events for an object identified by (path, name).
type ObjectHistoryResponse struct {
	Path   string             `json:"path"`
	Name   string             `json:"name,omitempty"`
	Events []ObjectHistoryRow `json:"events"`
	Total  int                `json:"total"`
}

// ObjectHistoryRow is one entry in the history list.
type ObjectHistoryRow struct {
	Time      string `json:"time"`
	EventType string `json:"event_type"`
	Detail    string `json:"detail,omitempty"`
	Seq       int    `json:"seq"`
}

func (h *Handler) serveObjectHistory(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "missing path query parameter")
		return
	}
	name := r.URL.Query().Get("name")

	wi := h.Store.WatchIndex[path]
	resp := &ObjectHistoryResponse{Path: path, Name: name}
	if wi == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	for i := range wi.Seqs {
		t := wi.Times[i]
		evType := ""
		if i < len(wi.EventTypes) {
			evType = wi.EventTypes[i]
		}
		seq := wi.Seqs[i]
		// Filter by name if requested — reads the record body to check.
		if name != "" {
			rec, err := h.Store.ReadRecord(path, seq)
			if err != nil {
				continue
			}
			if getName(rec.ResponseBody) != name {
				continue
			}
			ph := ClassifyPod(rec.ResponseBody)
			detail := ph.Phase
			if !ph.IsHealthy() && len(ph.Issues) > 0 {
				detail = ph.Issues[0]
			}
			resp.Events = append(resp.Events, ObjectHistoryRow{
				Time:      t.UTC().Format(time.RFC3339),
				EventType: evType,
				Detail:    detail,
				Seq:       seq,
			})
			continue
		}
		resp.Events = append(resp.Events, ObjectHistoryRow{
			Time:      t.UTC().Format(time.RFC3339),
			EventType: evType,
			Seq:       seq,
		})
	}
	sort.SliceStable(resp.Events, func(i, j int) bool { return resp.Events[i].Time > resp.Events[j].Time })
	resp.Total = len(resp.Events)
	writeJSON(w, http.StatusOK, resp)
}
