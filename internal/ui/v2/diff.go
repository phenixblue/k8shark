package v2

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/pmezard/go-difflib/difflib"
	"gopkg.in/yaml.v3"
)

// DiffResponse is the result of /v2/api/diff?path=…&before=T1&after=T2.
type DiffResponse struct {
	Path          string     `json:"path"`
	Name          string     `json:"name,omitempty"`
	Before        string     `json:"before"`
	After         string     `json:"after"`
	HasDiff       bool       `json:"has_diff"`
	Hunks         []DiffHunk `json:"hunks"`
	BeforeMissing bool       `json:"before_missing,omitempty"`
	AfterMissing  bool       `json:"after_missing,omitempty"`
}

// DiffHunk is one line of the unified diff with a type tag.
type DiffHunk struct {
	Type string `json:"type"` // "add" | "del" | "ctx" | "hunk"
	Text string `json:"text"`
}

func (h *Handler) serveDiff(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "missing path query parameter")
		return
	}
	name := r.URL.Query().Get("name") // optional — narrow to a single item by metadata.name
	beforeStr := r.URL.Query().Get("before")
	afterStr := r.URL.Query().Get("after")

	beforeAt, err := parseAt(beforeStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid before timestamp: "+err.Error())
		return
	}
	afterAt, err := parseAt(afterStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid after timestamp: "+err.Error())
		return
	}

	resp := &DiffResponse{Path: path, Name: name}
	if !beforeAt.IsZero() {
		resp.Before = beforeAt.UTC().Format(time.RFC3339)
	}
	if !afterAt.IsZero() {
		resp.After = afterAt.UTC().Format(time.RFC3339)
	}

	beforeYAML, beforeOK := h.readObjectYAML(path, name, beforeAt)
	afterYAML, afterOK := h.readObjectYAML(path, name, afterAt)
	if !beforeOK {
		resp.BeforeMissing = true
	}
	if !afterOK {
		resp.AfterMissing = true
	}
	if beforeOK || afterOK {
		resp.Hunks = unifiedDiffLines(beforeYAML, afterYAML, resp.Before, resp.After)
		for _, line := range resp.Hunks {
			if line.Type == "add" || line.Type == "del" {
				resp.HasDiff = true
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func parseAt(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

// readObjectYAML pulls the object at `path` (optionally filtered to a single
// item by name from a list body) and renders it as YAML for diffing.
func (h *Handler) readObjectYAML(path, name string, at time.Time) (string, bool) {
	body, code, err := h.Store.ReconstructAt(path, at)
	if err != nil || code != http.StatusOK || len(body) == 0 {
		return "", false
	}
	if name != "" {
		// Extract the matching item from the list.
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return toYAML(body), true
		}
		for _, it := range list.Items {
			if getName(it) == name {
				return toYAML(it), true
			}
		}
		return "", false
	}
	return toYAML(body), true
}

func toYAML(body []byte) string {
	var obj any
	if err := json.Unmarshal(body, &obj); err != nil {
		return string(body)
	}
	out, err := yaml.Marshal(obj)
	if err != nil {
		return string(body)
	}
	return string(out)
}

// unifiedDiffLines computes a unified diff and breaks it into typed lines so
// the frontend can colour add/del/context distinctly.
func unifiedDiffLines(before, after, beforeName, afterName string) []DiffHunk {
	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(before),
		B:        difflib.SplitLines(after),
		FromFile: "before",
		ToFile:   "after",
		FromDate: beforeName,
		ToDate:   afterName,
		Context:  3,
	}
	text, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		return []DiffHunk{{Type: "ctx", Text: "diff error: " + err.Error()}}
	}
	var hunks []DiffHunk
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			hunks = append(hunks, DiffHunk{Type: "hunk", Text: line})
		case strings.HasPrefix(line, "@@"):
			hunks = append(hunks, DiffHunk{Type: "hunk", Text: line})
		case strings.HasPrefix(line, "+"):
			hunks = append(hunks, DiffHunk{Type: "add", Text: line})
		case strings.HasPrefix(line, "-"):
			hunks = append(hunks, DiffHunk{Type: "del", Text: line})
		default:
			hunks = append(hunks, DiffHunk{Type: "ctx", Text: line})
		}
	}
	return hunks
}
