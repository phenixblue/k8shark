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

	// Intercept interactive sub-resources that can never work against a capture
	// replay. Doing this before the method check ensures we return a specific,
	// actionable error instead of the generic "write operations are not
	// supported" message — and we don't hang waiting for a protocol upgrade.
	//   kubectl exec / kubectl cp  → POST .../pods/<name>/exec
	//   kubectl port-forward       → POST .../pods/<name>/portforward
	//   kubectl attach             → POST .../pods/<name>/attach
	if strings.HasSuffix(path, "/exec") ||
		strings.HasSuffix(path, "/portforward") ||
		strings.HasSuffix(path, "/attach") {
		w.Header().Set("Allow", "")
		h.writeStatus(w, http.StatusMethodNotAllowed,
			"k8shark capture replay: exec, cp, and port-forward are not supported — "+
				"this mock server replays a captured snapshot and cannot run commands "+
				"or open connections on pods")
		return
	}

	// Reject all write operations — k8shark replay is read-only.
	// RFC 7231 §6.5.5 requires an Allow header with a 405 response.
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// allowed
	default:
		w.Header().Set("Allow", "GET, HEAD")
		h.writeStatus(w, http.StatusMethodNotAllowed,
			"k8shark replay server is read-only; write operations are not supported")
		return
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
		if !h.tryServeFromStore(w, path, replayAt) {
			// Minimal stub so kubectl tolerates missing spec gracefully.
			writeJSON(w, http.StatusOK, map[string]any{"swagger": "2.0", "info": map[string]any{"title": "k8shark", "version": "0.0.0"}, "paths": map[string]any{}})
		}
	case path == "/openapi/v3", strings.HasPrefix(path, "/openapi/v3/"):
		if !h.tryServeFromStore(w, path, replayAt) {
			h.writeStatus(w, http.StatusNotFound, path+" not in capture")
		}
	case path == "/api":
		if !h.tryServeFromStore(w, path, replayAt) {
			h.serveAPIVersions(w)
		}
	case path == "/apis":
		if !h.tryServeFromStore(w, path, replayAt) {
			h.serveAPIGroupList(w)
		}
	case path == "/api/v1":
		if !h.tryServeFromStore(w, path, replayAt) {
			h.serveAPIResourceList(w, "", "v1")
		}
	case strings.HasPrefix(path, "/apis/") && isGroupVersionPath(path):
		if !h.tryServeFromStore(w, path, replayAt) {
			h.serveGroupResourceList(w, path)
		}
	case strings.HasSuffix(path, "/log"):
		// Pod log sub-resource: serve captured content or a helpful stub.
		h.serveLog(w, path, replayAt)
	default:
		h.serveResource(w, r, path, replayAt)
	}
}

// tryServeFromStore writes the stored response for path and returns true if
// a successful record was found. Used to serve captured discovery responses.
func (h *handler) tryServeFromStore(w http.ResponseWriter, path string, at time.Time) bool {
	body, code, err := h.store.Latest(path, at)
	if err != nil || code != 200 {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
	return true
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

// shortNamesFor returns well-known kubectl short names for a resource.
func shortNamesFor(resource string) []string {
	known := map[string][]string{
		"pods":                      {"po"},
		"services":                  {"svc"},
		"deployments":               {"deploy"},
		"daemonsets":                {"ds"},
		"namespaces":                {"ns"},
		"nodes":                     {"no"},
		"configmaps":                {"cm"},
		"persistentvolumeclaims":    {"pvc"},
		"persistentvolumes":         {"pv"},
		"serviceaccounts":           {"sa"},
		"replicasets":               {"rs"},
		"statefulsets":              {"sts"},
		"jobs":                      {"job"},
		"cronjobs":                  {"cj"},
		"ingresses":                 {"ing"},
		"horizontalpodautoscalers":  {"hpa"},
		"replicationcontrollers":    {"rc"},
		"resourcequotas":            {"quota"},
		"limitranges":               {"limits"},
		"events":                    {"ev"},
		"endpoints":                 {"ep"},
		"networkpolicies":           {"netpol"},
		"poddisruptionbudgets":      {"pdb"},
		"clusterrolebindings":       {"crb"},
		"clusterroles":              {"cr"},
		"rolebindings":              {"rb"},
		"storageclasses":            {"sc"},
		"customresourcedefinitions": {"crd"},
	}
	return known[resource]
}

func (h *handler) serveAPIResourceList(w http.ResponseWriter, group, version string) {
	resources := make([]map[string]any, 0)
	for _, ri := range h.store.Resources() {
		if ri.Group != group || ri.Version != version {
			continue
		}
		entry := map[string]any{
			"name":       ri.Resource,
			"namespaced": ri.Namespaced,
			"kind":       ri.Kind,
			"verbs":      []string{"get", "list", "watch"},
		}
		if sn := shortNamesFor(ri.Resource); len(sn) > 0 {
			entry["shortNames"] = sn
		}
		resources = append(resources, entry)
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
		// Try all-namespaces aggregation: kubectl -A issues /api/v1/pods etc.
		body, code, err = h.store.AggregateAcrossNamespaces(path, at)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, statusObj(500, err.Error()))
			return
		}
	}

	if code == 404 {
		// If the path parses as a list-level resource (not an item GET), return
		// an empty list with a Warning header so kubectl shows
		// "No resources found" rather than "Error from server: not found".
		// Item-level GETs (path has more segments than parseAPIPath handles)
		// still get a proper 404.
		g, v, resource, _ := parseAPIPath(path)
		if resource != "" {
			av := v
			if g != "" {
				av = g + "/" + v
			}
			emptyList, _ := json.Marshal(map[string]any{
				"apiVersion": av,
				"kind":       resourceToKind(resource) + "List",
				"metadata":   map[string]string{"resourceVersion": "0"},
				"items":      []any{},
			})
			w.Header().Set("Warning", fmt.Sprintf(`299 k8shark %q`,
				resource+" not found in capture; was it included in the capture config?"))
			body, code = emptyList, 200
		} else {
			h.writeStatus(w, http.StatusNotFound, fmt.Sprintf("%q not found in capture", path))
			return
		}
	}

	// Apply label/field selectors if present.
	labelSel := r.URL.Query().Get("labelSelector")
	fieldSel := r.URL.Query().Get("fieldSelector")
	body, err = applySelectors(body, labelSel, fieldSel)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, statusObj(500, err.Error()))
		return
	}

	// tableFiltered applies label/field selectors to a stored Table-format body,
	// removing rows whose embedded object does not match. Returns tb unchanged
	// when selectors are empty or if filtering fails (best-effort).
	tableFiltered := func(tb []byte) []byte {
		if labelSel == "" && fieldSel == "" {
			return tb
		}
		if out, ferr := filterTableRows(tb, labelSel, fieldSel); ferr == nil {
			return out
		}
		return tb
	}

	// If kubectl requests Table format, try the captured Table response first
	// (real column defs + pre-computed cell values from the actual cluster).
	// Fall back to buildTable only for captures predating this feature.
	if strings.Contains(r.Header.Get("Accept"), "as=Table") {
		// Exact-path stored Table (namespace-scoped list).
		if tb, tbCode, _ := h.store.Latest(path+"?as=Table", at); tbCode == 200 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write(tableFiltered(tb))
			return
		}
		// Single-item GET: extract the matching row from the parent list's Table.
		// Selectors on single-item GETs are resolved by name, not labels — no
		// row filtering needed here.
		if i := strings.LastIndex(path, "/"); i > 0 {
			parentTable, ptCode, _ := h.store.Latest(path[:i]+"?as=Table", at)
			if ptCode == 200 {
				if tb, err2 := extractTableRow(parentTable, path[i+1:]); err2 == nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					_, _ = w.Write(tb)
					return
				}
			}
		}
		// Aggregated Table across namespaces (for -A / cluster-scoped paths).
		if tb, tbCode, _ := h.store.AggregateTableAcrossNamespaces(path, at); tbCode == 200 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write(tableFiltered(tb))
			return
		}
		// Last resort: synthesize a minimal Table from the list body (old captures).
		// body is already selector-filtered above, so buildTable sees filtered items.
		if tb, err2 := buildTable(body); err2 == nil {
			body = tb
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// extractTableRow finds the row matching name in a Table response and returns
// a single-row Table with the same columnDefinitions. This is used when kubectl
// requests a single object by name — the per-name ?as=Table isn't captured, but
// the parent list's Table is, and we can slice the right row out of it.
func extractTableRow(tableBody []byte, name string) ([]byte, error) {
	var table struct {
		APIVersion        string            `json:"apiVersion"`
		Kind              string            `json:"kind"`
		Metadata          json.RawMessage   `json:"metadata"`
		ColumnDefinitions json.RawMessage   `json:"columnDefinitions"`
		Rows              []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(tableBody, &table); err != nil {
		return nil, err
	}
	for _, row := range table.Rows {
		var r struct {
			Object struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"object"`
		}
		if err := json.Unmarshal(row, &r); err != nil {
			continue
		}
		if r.Object.Metadata.Name == name {
			return json.Marshal(map[string]any{
				"apiVersion":        table.APIVersion,
				"kind":              table.Kind,
				"metadata":          table.Metadata,
				"columnDefinitions": table.ColumnDefinitions,
				"rows":              []json.RawMessage{row},
			})
		}
	}
	return nil, fmt.Errorf("row %q not found in table", name)
}

// buildTable synthesizes a minimal meta.k8s.io/v1 Table from raw list or
// single-object JSON. This is a last-resort fallback only reached for captures
// made before Table responses were stored during capture. New captures always
// serve the real Table response captured from the live cluster, so no
// resource-specific logic is needed here.
func buildTable(body []byte) ([]byte, error) {
	var envelope struct {
		Items []json.RawMessage `json:"items"`
	}
	_ = json.Unmarshal(body, &envelope)
	rawItems := envelope.Items
	if rawItems == nil {
		rawItems = []json.RawMessage{body}
	}

	type colDef struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Format      string `json:"format,omitempty"`
		Description string `json:"description"`
	}

	hasNS := false
	type objMeta struct {
		Metadata struct {
			Name              string `json:"name"`
			Namespace         string `json:"namespace"`
			CreationTimestamp string `json:"creationTimestamp"`
		} `json:"metadata"`
	}
	metas := make([]objMeta, len(rawItems))
	for i, raw := range rawItems {
		_ = json.Unmarshal(raw, &metas[i])
		if metas[i].Metadata.Namespace != "" {
			hasNS = true
		}
	}

	cols := []colDef{{Name: "Name", Type: "string", Format: "name", Description: "Name"}}
	if hasNS {
		cols = append(cols, colDef{Name: "Namespace", Type: "string", Description: "Namespace"})
	}
	cols = append(cols, colDef{Name: "Age", Type: "date", Description: "CreationTimestamp"})

	rows := make([]map[string]any, 0, len(metas))
	for _, m := range metas {
		cells := []any{m.Metadata.Name}
		if hasNS {
			cells = append(cells, m.Metadata.Namespace)
		}
		cells = append(cells, m.Metadata.CreationTimestamp)
		rows = append(rows, map[string]any{
			"cells": cells,
			"object": map[string]any{
				"kind": "PartialObjectMetadata", "apiVersion": "meta.k8s.io/v1",
				"metadata": map[string]any{
					"name": m.Metadata.Name, "namespace": m.Metadata.Namespace,
					"creationTimestamp": m.Metadata.CreationTimestamp,
				},
			},
		})
	}

	return json.Marshal(map[string]any{
		"apiVersion":        "meta.k8s.io/v1",
		"kind":              "Table",
		"metadata":          map[string]string{"resourceVersion": "0"},
		"columnDefinitions": cols,
		"rows":              rows,
	})
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
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Items      []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, 404
	}

	// Derive item kind from list kind (e.g. "PodList" → "Pod").
	itemKind := strings.TrimSuffix(list.Kind, "List")

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
			// Inject apiVersion + kind so kubectl can decode the single object.
			var m map[string]json.RawMessage
			if err := json.Unmarshal(item, &m); err != nil {
				return item, 200
			}
			av, _ := json.Marshal(list.APIVersion)
			kd, _ := json.Marshal(itemKind)
			m["apiVersion"] = av
			m["kind"] = kd
			out, err := json.Marshal(m)
			if err != nil {
				return item, 200
			}
			return out, 200
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

// serveLog serves a pod log sub-resource (e.g. /api/v1/namespaces/<ns>/pods/<name>/log).
// If the log was captured (logs: N in the resource config), it is served as
// plain text. Otherwise a helpful stub message is returned so kubectl logs does
// not error out against the mock server.
func (h *handler) serveLog(w http.ResponseWriter, path string, at time.Time) {
	body, code, err := h.store.Latest(path, at)
	if err == nil && code == 200 {
		// Logs are stored as JSON strings; decode to recover the original plain text.
		var text string
		if json.Unmarshal(body, &text) == nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, text)
			return
		}
		// Fallback: body is already plain text (should not normally happen).
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	// Log was not captured — return a readable stub so kubectl logs exits cleanly.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w,
		"# k8shark capture replay: logs were not captured for this pod.\n"+
			"# To capture logs, add 'logs: 200' (or another line count) to the\n"+
			"# pods entry in your k8shark capture config and re-run the capture.\n")
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
