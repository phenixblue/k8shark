package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handler is the http.Handler for the mock Kubernetes API server.
type handler struct {
	store   *CaptureStore
	at      time.Time
	verbose bool
}

func newHandler(store *CaptureStore, at time.Time, verbose bool) *handler {
	return &handler{store: store, at: at, verbose: verbose}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Per-request timestamp override via header.
	replayAt := h.at
	if v := r.Header.Get("X-K8shark-At"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			replayAt = t
		}
	}

	path := r.URL.Path
	if h.verbose {
		fmt.Printf("  --> %s %s\n", r.Method, path)
	}

	// Watch requests get a synthetic event stream.
	if r.URL.Query().Get("watch") == "1" || r.URL.Query().Get("watch") == "true" {
		h.handleWatch(w, r, path, replayAt)
		return
	}

	// Route discovery and resource requests.
	switch {
	case path == "/version":
		h.serveVersion(w)
	case path == "/healthz", path == "/readyz", path == "/livez":
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	case path == "/openapi/v2":
		// Return a minimal stub; kubectl tolerates an empty spec.
		writeJSON(w, http.StatusOK, map[string]any{"swagger": "2.0", "info": map[string]any{"title": "k8shark", "version": "0.0.0"}, "paths": map[string]any{}})
	case path == "/api":
		h.serveAPIVersions(w)
	case path == "/apis":
		h.serveAPIGroupList(w)
	case path == "/api/v1":
		h.serveAPIResourceList(w, "", "v1")
	case strings.HasPrefix(path, "/apis/") && isGroupVersionPath(path):
		h.serveGroupResourceList(w, path)
	default:
		h.serveResource(w, r, path, replayAt)
	}
}

// isGroupVersionPath returns true when path is exactly /apis/<group>/<version>.
func isGroupVersionPath(path string) bool {
	rest := strings.TrimPrefix(path, "/apis/")
	return len(strings.Split(rest, "/")) == 2
}

func (h *handler) serveVersion(w http.ResponseWriter) {
	kv := h.store.Metadata.KubernetesVersion
	if kv == "" {
		kv = "v0.0.0-k8shark"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"major":        "1",
		"minor":        "0",
		"gitVersion":   kv,
		"gitCommit":    "k8shark-replay",
		"gitTreeState": "clean",
		"buildDate":    h.store.Metadata.CapturedAt.UTC().Format(time.RFC3339),
		"goVersion":    "go0.0.0",
		"compiler":     "gc",
		"platform":     "linux/amd64",
	})
}

func (h *handler) serveAPIVersions(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":       "APIVersions",
		"apiVersion": "v1",
		"versions":   []string{"v1"},
		"serverAddressByClientCIDRs": []map[string]string{
			{"clientCIDR": "0.0.0.0/0", "serverAddress": h.store.Metadata.ServerAddress},
		},
	})
}

func (h *handler) serveAPIGroupList(w http.ResponseWriter) {
	// Collect non-core API groups present in the capture.
	type gv struct{ group, version, groupVersion string }
	seen := map[string][]gv{}
	for _, ri := range h.store.Resources() {
		if ri.Group == "" {
			continue
		}
		groupVersion := ri.Group + "/" + ri.Version
		duplicate := false
		for _, existing := range seen[ri.Group] {
			if existing.groupVersion == groupVersion {
				duplicate = true
				break
			}
		}
		if !duplicate {
			seen[ri.Group] = append(seen[ri.Group], gv{ri.Group, ri.Version, groupVersion})
		}
	}

	groups := make([]map[string]any, 0, len(seen))
	for g, gvs := range seen {
		versions := make([]map[string]string, 0, len(gvs))
		for _, v := range gvs {
			versions = append(versions, map[string]string{
				"groupVersion": v.groupVersion,
				"version":      v.version,
			})
		}
		groups = append(groups, map[string]any{
			"name":             g,
			"versions":         versions,
			"preferredVersion": versions[0],
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"kind":       "APIGroupList",
		"apiVersion": "v1",
		"groups":     groups,
	})
}

func (h *handler) serveAPIResourceList(w http.ResponseWriter, group, version string) {
	resources := make([]map[string]any, 0)
	for _, ri := range h.store.Resources() {
		if ri.Group != group || ri.Version != version {
			continue
		}
		resources = append(resources, map[string]any{
			"name":       ri.Resource,
			"namespaced": ri.Namespaced,
			"kind":       ri.Kind,
			"verbs":      []string{"get", "list", "watch"},
		})
	}

	groupVersion := version
	if group != "" {
		groupVersion = group + "/" + version
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":         "APIResourceList",
		"apiVersion":   "v1",
		"groupVersion": groupVersion,
		"resources":    resources,
	})
}

func (h *handler) serveGroupResourceList(w http.ResponseWriter, path string) {
	// path is /apis/<group>/<version>
	parts := strings.SplitN(strings.TrimPrefix(path, "/apis/"), "/", 2)
	if len(parts) != 2 {
		h.writeStatus(w, http.StatusNotFound, path+" not found")
		return
	}
	h.serveAPIResourceList(w, parts[0], parts[1])
}

func (h *handler) serveResource(w http.ResponseWriter, r *http.Request, path string, at time.Time) {
	body, code, err := h.store.Latest(path, at)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, statusObj(500, err.Error()))
		return
	}

	if code == 404 {
		// Try single-item GET by looking up the parent list and filtering by name.
		body, code = h.trySingleItemGet(path, at)
	}

	if code == 404 {
		h.writeStatus(w, http.StatusNotFound, fmt.Sprintf("%q not found in capture", path))
		return
	}

	// Apply label/field selectors if present.
	body, err = applySelectors(body, r.URL.Query().Get("labelSelector"), r.URL.Query().Get("fieldSelector"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, statusObj(500, err.Error()))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// trySingleItemGet handles GET .../resource/{name} by finding the parent list
// and scanning its items for a matching metadata.name.
func (h *handler) trySingleItemGet(path string, at time.Time) ([]byte, int) {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return nil, 404
	}
	name := path[i+1:]
	parentPath := path[:i]

	body, code, err := h.store.Latest(parentPath, at)
	if err != nil || code != 200 {
		return nil, 404
	}

	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, 404
	}
	for _, item := range list.Items {
		var obj struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(item, &obj); err != nil {
			continue
		}
		if obj.Metadata.Name == name {
			return item, 200
		}
	}
	return nil, 404
}

func (h *handler) handleWatch(w http.ResponseWriter, r *http.Request, path string, at time.Time) {
	rawBody, code, err := h.store.Latest(strings.TrimSuffix(path, "/"), at)
	if err != nil || code != 200 {
		h.writeStatus(w, http.StatusNotFound, fmt.Sprintf("%q not found in capture", path))
		return
	}

	// Apply selectors before streaming watch events.
	body, _ := applySelectors(rawBody, r.URL.Query().Get("labelSelector"), r.URL.Query().Get("fieldSelector"))

	var list struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		h.writeStatus(w, http.StatusInternalServerError, "parsing list")
		return
	}

	// Honor ?timeoutSeconds: nil channel blocks forever (no timeout).
	var timer <-chan time.Time
	if secs := r.URL.Query().Get("timeoutSeconds"); secs != "" {
		if n, err := strconv.Atoi(secs); err == nil && n > 0 {
			timer = time.After(time.Duration(n) * time.Second)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	for _, item := range list.Items {
		event := map[string]any{"type": "ADDED", "object": json.RawMessage(item)}
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "%s\n", data)
		if canFlush {
			flusher.Flush()
		}
	}

	// Use resourceVersion from the list metadata; fall back to capture timestamp.
	rv := list.Metadata.ResourceVersion
	if rv == "" {
		rv = fmt.Sprintf("%d", h.store.Metadata.CapturedAt.Unix())
	}

	// BOOKMARK signals end of initial list; kubectl -w then waits for new events.
	bookmark := map[string]any{
		"type": "BOOKMARK",
		"object": map[string]any{
			"apiVersion": "v1",
			"kind":       "Status",
			"metadata":   map[string]string{"resourceVersion": rv},
		},
	}
	data, _ := json.Marshal(bookmark)
	_, _ = fmt.Fprintf(w, "%s\n", data)
	if canFlush {
		flusher.Flush()
	}

	// Hold until the client disconnects or timeoutSeconds elapses.
	select {
	case <-r.Context().Done():
	case <-timer:
	}
}

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
