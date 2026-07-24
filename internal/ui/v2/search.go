package v2

import (
	"net/http"

	"github.com/phenixblue/k8shark/internal/query"
)

// maxSearchResults caps how many rows /v2/api/search returns in one response,
// so a broad pattern against a large archive can't send an enormous payload
// to the browser. Truncated is set on the response when the cap was hit.
const maxSearchResults = 500

// SearchResult is one row in the dashboard's global search results: a
// deep-linkable object identity, plus either a JSONPath value or a full-text
// match location/snippet depending on the request's mode.
type SearchResult struct {
	Path      string `json:"path"`
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Resource  string `json:"resource,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	// Value is the JSONPath result (mode=jsonpath only).
	Value string `json:"value,omitempty"`
	// Field/Snippet/Log/Container/Previous are set for full-text matches
	// (mode=text/regex only) — see internal/query.TextMatch.
	Field     string `json:"field,omitempty"`
	Snippet   string `json:"snippet,omitempty"`
	Log       bool   `json:"log,omitempty"`
	Container string `json:"container,omitempty"`
	Previous  bool   `json:"previous,omitempty"`
}

// SearchResponse is the /v2/api/search response body.
type SearchResponse struct {
	Mode      string         `json:"mode"`
	Query     string         `json:"query"`
	Results   []SearchResult `json:"results"`
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
}

// serveSearch runs the same query engine as `kshrk query` (internal/query)
// against the capture, for the dashboard's global search box. mode selects
// the query language: "jsonpath" (default), "text" (substring), or "regex".
func (h *Handler) serveSearch(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "missing q query parameter")
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "jsonpath"
	}
	resource := r.URL.Query().Get("resource")
	namespace := r.URL.Query().Get("namespace")
	at := h.resolveAt(r)

	var rows []SearchResult
	var total int
	switch mode {
	case "jsonpath":
		result, err := query.Run(h.Store, query.Options{Expression: q, At: at, Resource: resource, Namespace: namespace})
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		total = len(result.Matches)
		rows = make([]SearchResult, 0, min(total, maxSearchResults))
		for _, m := range result.Matches {
			if len(rows) >= maxSearchResults {
				break
			}
			rows = append(rows, SearchResult{
				Path: m.Path, Group: m.Group, Version: m.Version, Resource: m.Resource,
				Namespace: m.Namespace, Name: m.Name, Value: string(m.Value),
			})
		}
	case "text", "regex":
		result, err := query.SearchText(h.Store, query.TextOptions{
			Pattern: q, Regex: mode == "regex", At: at, Resource: resource, Namespace: namespace,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		total = len(result.Matches)
		rows = make([]SearchResult, 0, min(total, maxSearchResults))
		for _, m := range result.Matches {
			if len(rows) >= maxSearchResults {
				break
			}
			rows = append(rows, SearchResult{
				Path: m.Path, Group: m.Group, Version: m.Version, Resource: m.Resource,
				Namespace: m.Namespace, Name: m.Name, Field: m.Field, Snippet: m.Snippet,
				Log: m.Log, Container: m.Container, Previous: m.Previous,
			})
		}
	default:
		writeError(w, http.StatusBadRequest, "mode must be jsonpath, text, or regex (got "+mode+")")
		return
	}

	writeJSON(w, http.StatusOK, SearchResponse{
		Mode: mode, Query: q, Results: rows, Total: total, Truncated: total > maxSearchResults,
	})
}
