package v2

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// LogsResponse is what /v2/api/logs returns. Aside from the requested
// container's log text it also reports the other containers captured for
// the pod so the UI can render a picker.
type LogsResponse struct {
	Namespace          string             `json:"namespace"`
	Pod                string             `json:"pod"`
	Container          string             `json:"container"`
	Previous           bool               `json:"previous"`
	Text               string             `json:"text"`
	LineCount          int                `json:"line_count"`
	HasPreviousVariant bool               `json:"has_previous_variant"`
	Containers         []LogsContainerOpt `json:"containers"`
}

// LogsContainerOpt is one entry in the container picker.
type LogsContainerOpt struct {
	Name        string `json:"name"`
	HasCurrent  bool   `json:"has_current"`
	HasPrevious bool   `json:"has_previous"`
}

func (h *Handler) serveLogs(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	ns := r.URL.Query().Get("ns")
	pod := r.URL.Query().Get("pod")
	if ns == "" || pod == "" {
		writeError(w, http.StatusBadRequest, "missing ns or pod query parameter")
		return
	}
	container := r.URL.Query().Get("container")
	previous := r.URL.Query().Get("previous") == "true"

	resp, err := h.buildLogsResponse(ns, pod, container, previous)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if resp == nil {
		writeError(w, http.StatusNotFound, "no captured logs for this pod")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) buildLogsResponse(ns, pod, container string, previous bool) (*LogsResponse, error) {
	store := h.Store
	logPath := "/api/v1/namespaces/" + ns + "/pods/" + pod + "/log"

	// Discover every container we have a log for, plus whether current and/or
	// previous are present, by walking the index for matching keys.
	options := map[string]*LogsContainerOpt{}
	prefix := logPath + "?container="
	for key := range store.Index {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		rest := strings.TrimPrefix(key, prefix)
		isPrev := strings.HasSuffix(rest, "&previous=true")
		c := strings.TrimSuffix(rest, "&previous=true")
		if c == "" {
			continue
		}
		opt, ok := options[c]
		if !ok {
			opt = &LogsContainerOpt{Name: c}
			options[c] = opt
		}
		if isPrev {
			opt.HasPrevious = true
		} else {
			opt.HasCurrent = true
		}
	}
	if len(options) == 0 {
		return nil, nil
	}

	// Default container = first that has the requested variant.
	if container == "" {
		// Prefer "main" looking containers (not init / sidecar).
		var pick string
		for _, k := range sortedKeys(options) {
			c := options[k]
			if previous && !c.HasPrevious {
				continue
			}
			if !previous && !c.HasCurrent {
				continue
			}
			pick = c.Name
			if !isSidecarName(c.Name) {
				break
			}
		}
		container = pick
	}
	if container == "" {
		return nil, fmt.Errorf("no container has a captured log matching the requested variant")
	}

	indexKey := prefix + container
	if previous {
		indexKey += "&previous=true"
	}
	body, code, err := store.Latest(indexKey, h.At)
	if err != nil || code != http.StatusOK || len(body) == 0 {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(body, &text); err != nil {
		text = string(body)
	}

	resp := &LogsResponse{
		Namespace: ns,
		Pod:       pod,
		Container: container,
		Previous:  previous,
		Text:      text,
		LineCount: countLines(text),
	}
	if opt, ok := options[container]; ok {
		resp.HasPreviousVariant = opt.HasPrevious
	}
	resp.Containers = make([]LogsContainerOpt, 0, len(options))
	for _, k := range sortedKeys(options) {
		resp.Containers = append(resp.Containers, *options[k])
	}
	return resp, nil
}

func sortedKeys(m map[string]*LogsContainerOpt) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}
