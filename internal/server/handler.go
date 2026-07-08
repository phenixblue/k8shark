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
	// clock, when non-nil, puts the handler in replay mode: LIST/GET reconstruct
	// state as-of the clock's advancing position and watches stream captured
	// events over time (see streamReplayWatch). nil for plain `open`/`ui`.
	clock *ReplayClock
}

func newHandler(store *CaptureStore, at time.Time, verbose bool) *handler {
	return &handler{store: store, at: at, verbose: verbose}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// In replay mode the effective time is the clock's current position;
	// otherwise it's the server's fixed --at.
	replayAt := h.at
	if h.clock != nil {
		replayAt = h.clock.Now()
	}
	// Per-request timestamp override via header (UI time travel).
	if v := r.Header.Get("X-K8shark-At"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			replayAt = t
		}
	}

	path := r.URL.Path
	if h.verbose {
		fmt.Printf("  --> %s %s\n", r.Method, path)
	}

	// Replay transport controls live under a reserved prefix that can't collide
	// with the Kubernetes API (which is served under /api, /apis, …). They accept
	// POST, so intercept before the read-only method check below.
	if h.clock != nil && strings.HasPrefix(path, replayControlPrefix) {
		h.handleReplayControl(w, r, path)
		return
	}

	// Intercept interactive sub-resources that can never work against a capture
	// replay. Doing this before the method check ensures we return a specific,
	// actionable error instead of the generic "write operations are not
	// supported" message — and we don't hang waiting for a protocol upgrade.
	//   kubectl exec / kubectl cp  → POST .../pods/<name>/exec
	//   kubectl port-forward       → POST .../pods/<name>/portforward
	//   kubectl attach             → POST .../pods/<name>/attach
	//   istioctl proxy-status      → GET/POST .../pods/<name>/proxy/...
	//                                GET/POST .../services/<name>/proxy/...
	if strings.HasSuffix(path, "/exec") ||
		strings.HasSuffix(path, "/portforward") ||
		strings.HasSuffix(path, "/attach") ||
		strings.HasSuffix(path, "/proxy") ||
		strings.Contains(path, "/proxy/") {
		w.Header().Set("Allow", "")
		h.writeStatus(w, http.StatusMethodNotAllowed,
			"k8shark capture replay: exec, cp, port-forward, and proxy are not supported — "+
				"this mock server replays a captured snapshot and cannot run commands "+
				"or proxy connections to pods/services")
		return
	}

	// Client compatibility: tools like k9s POST authorization review resources
	// to determine what actions are available. These requests are read-only
	// capability checks, not mutating operations, so we synthesize permissive
	// read-only responses to avoid breaking navigation workflows.
	if r.Method == http.MethodPost {
		switch path {
		case "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
			writeJSON(w, http.StatusCreated, map[string]any{
				"apiVersion": "authorization.k8s.io/v1",
				"kind":       "SelfSubjectAccessReview",
				"status": map[string]any{
					"allowed": true,
					"denied":  false,
					"reason":  "k8shark replay server: read-only access checks allowed for client compatibility",
				},
			})
			return
		case "/apis/authorization.k8s.io/v1/selfsubjectrulesreviews":
			writeJSON(w, http.StatusCreated, map[string]any{
				"apiVersion": "authorization.k8s.io/v1",
				"kind":       "SelfSubjectRulesReview",
				"status": map[string]any{
					"incomplete": false,
					"resourceRules": []map[string]any{
						{
							"verbs":     []string{"get", "list", "watch"},
							"apiGroups": []string{"*"},
							"resources": []string{"*"},
						},
					},
					"nonResourceRules": []map[string]any{
						{
							"verbs":           []string{"get"},
							"nonResourceURLs": []string{"*"},
						},
					},
				},
			})
			return
		}
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

	// Watch requests get a synthetic event stream. In replay mode the stream is
	// paced by the clock; otherwise it's the snapshot-burst-then-idle behavior.
	if r.URL.Query().Get("watch") == "1" || r.URL.Query().Get("watch") == "true" {
		if h.clock != nil {
			h.streamReplayWatch(w, r, path)
		} else {
			h.handleWatch(w, r, path, replayAt)
		}
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
		h.serveLog(w, r, path, replayAt)
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
		// Prefer short names from the captured discovery document; fall back to
		// the built-in static map for well-known Kubernetes types.
		sn := ri.ShortNames
		if len(sn) == 0 {
			sn = shortNamesFor(ri.Resource)
		}
		if len(sn) > 0 {
			entry["shortNames"] = sn
		}
		if ri.SingularName != "" {
			entry["singularName"] = ri.SingularName
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
	body, code, err := h.store.ReconstructAt(path, at)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, statusObj(500, err.Error()))
		return
	}

	if code == 404 {
		// Try single-item GET by looking up the parent list and filtering by name.
		body, code = h.trySingleItemGet(path, at)
	}

	if code == 404 {
		// Try all-namespaces aggregation: kubectl -A issues cluster-wide paths
		// like /api/v1/pods or /apis/apps/v1/deployments. Only fire for paths
		// with no namespace segment; namespace-scoped 404s fall through to the
		// cluster-scoped fallback below, which correctly filters by namespace.
		if _, _, _, reqNS := parseAPIPath(path); reqNS == "" {
			body, code, err = h.store.AggregateAcrossNamespaces(path, at)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, statusObj(500, err.Error()))
				return
			}
		}
	}

	if code == 404 {
		// Cluster-scoped fallback: if the resource was captured at the cluster
		// path (e.g. pods with no namespaces: in config, or allNotFound fallback)
		// but the request targets a specific namespace, try the cluster path and
		// filter items by metadata.namespace. This makes kubectl get pods -n <ns>
		// work even when only /api/v1/pods (not per-namespace paths) was captured.
		g, v, resource, ns := parseAPIPath(path)
		if ns != "" && resource != "" {
			var clusterPath string
			if g == "" {
				clusterPath = "/api/" + v + "/" + resource
			} else {
				clusterPath = "/apis/" + g + "/" + v + "/" + resource
			}
			clusterBody, clusterCode, cerr := h.store.ReconstructAt(clusterPath, at)
			if cerr != nil {
				writeJSON(w, http.StatusInternalServerError, statusObj(500, cerr.Error()))
				return
			}
			if clusterCode == 200 {
				filtered, ferr := applySelectors(clusterBody, "", "metadata.namespace="+ns)
				if ferr == nil {
					body, code = filtered, 200
				}
			}
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
		// Aggregated Table across namespaces (for -A / cluster-scoped paths only).
		if _, _, _, reqNS := parseAPIPath(path); reqNS == "" {
			if tb, tbCode, _ := h.store.AggregateTableAcrossNamespaces(path, at); tbCode == 200 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = w.Write(tableFiltered(tb))
				return
			}
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

	body, code, err := h.store.ReconstructAt(parentPath, at)
	if err != nil || code != 200 {
		// Namespace-scoped single-item GET whose per-namespace list was not
		// captured: fall back to the cluster-scoped list and filter by namespace.
		g, v, resource, ns := parseAPIPath(parentPath)
		if ns != "" && resource != "" {
			var clusterParent string
			if g == "" {
				clusterParent = "/api/" + v + "/" + resource
			} else {
				clusterParent = "/apis/" + g + "/" + v + "/" + resource
			}
			clusterBody, clusterCode, cerr := h.store.ReconstructAt(clusterParent, at)
			if cerr == nil && clusterCode == 200 {
				// Filter to the requested namespace before doing the name lookup.
				filtered, ferr := applySelectors(clusterBody, "", "metadata.namespace="+ns)
				if ferr == nil {
					body, code = filtered, 200
				}
			}
		}
		if code != 200 {
			return nil, 404
		}
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
	watchPath := strings.TrimSuffix(path, "/")
	list, ok, err := h.resolveWatchList(watchPath, at, r.URL.Query().Get("labelSelector"), r.URL.Query().Get("fieldSelector"))
	if err != nil {
		h.writeStatus(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		h.writeStatus(w, http.StatusNotFound, fmt.Sprintf("%q not found in capture", path))
		return
	}

	// Honor ?timeoutSeconds: nil channel blocks forever (no timeout).
	timer, stopTimer := watchTimeout(r.URL.Query().Get("timeoutSeconds"))
	defer stopTimer()

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

	// Use resourceVersion from the list metadata; fall back to a capture time.
	// Treat "0" as unspecified too — aggregated/synthesized empty lists carry
	// RV "0", but watch clients expect a non-zero BOOKMARK resourceVersion.
	rv := list.ResourceVersion
	if rv == "" || rv == "0" {
		// Lead with the list-as-of time so the BOOKMARK RV aligns with --at / UI
		// time travel; fall back to the capture bounds when `at` is unset.
		rv = bookmarkResourceVersion(at, h.store.Metadata.CapturedAt, h.store.Metadata.CapturedUntil)
	}

	// BOOKMARK signals end of initial list; kubectl -w then waits for new events.
	// The BOOKMARK object must have the same kind as the watched resource
	// (not "Status"), otherwise client-go reflectors log unexpected type errors.
	bookmarkKind := strings.TrimSuffix(list.Kind, "List")
	bookmarkAPIVersion := list.APIVersion
	if bookmarkKind == "" {
		bookmarkKind = "Status"
	}
	if bookmarkAPIVersion == "" {
		bookmarkAPIVersion = "v1"
	}
	bookmark := map[string]any{
		"type": "BOOKMARK",
		"object": map[string]any{
			"apiVersion": bookmarkAPIVersion,
			"kind":       bookmarkKind,
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
// Recognized query parameters:
//   - container=<c>  — request a specific container's log
//   - previous=true  — request the previous-container log (kubectl logs --previous)
//
// Lookup order:
//  1. If both container and previous are set, look up the record under
//     path + "?container=<c>&previous=true".
//  2. If only container is set, look up path + "?container=<c>".
//  3. Legacy archives stored a single record at the bare path — try that.
//  4. If no container was specified, fall back to the first per-container
//     record we have for this pod (covers single-container pods, where
//     kubectl sends no ?container= param).
//  5. Return a readable stub explaining how to enable log capture.
func (h *handler) serveLog(w http.ResponseWriter, r *http.Request, path string, at time.Time) {
	q := r.URL.Query()
	container := q.Get("container")
	previous := q.Get("previous") == "true"

	if container != "" {
		key := path + "?container=" + container
		if previous {
			key += "&previous=true"
		}
		if h.tryServeLogRecord(w, key, at) {
			return
		}
	}
	if h.tryServeLogRecord(w, path, at) {
		return
	}
	if container == "" {
		prefix := path + "?container="
		suffix := ""
		if previous {
			suffix = "&previous=true"
		}
		for indexKey := range h.store.Index {
			if !strings.HasPrefix(indexKey, prefix) {
				continue
			}
			if suffix != "" && !strings.HasSuffix(indexKey, suffix) {
				continue
			}
			if suffix == "" && strings.Contains(indexKey, "&previous=true") {
				// When the client didn't ask for previous logs, don't accidentally
				// serve a previous-log record as the default.
				continue
			}
			if h.tryServeLogRecord(w, indexKey, at) {
				return
			}
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w,
		"# k8shark capture replay: logs were not captured for this pod.\n"+
			"# To capture logs, add 'logs: 200' (or another line count) to the\n"+
			"# pods entry in your k8shark capture config and re-run the capture.\n")
}

// tryServeLogRecord writes the captured log for indexKey if one exists.
// Returns true on success so the caller stops trying further fallbacks.
func (h *handler) tryServeLogRecord(w http.ResponseWriter, indexKey string, at time.Time) bool {
	body, code, err := h.store.Latest(indexKey, at)
	if err != nil || code != 200 {
		return false
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// Logs are stored as JSON strings; decode to recover the original plain text.
	var text string
	if json.Unmarshal(body, &text) == nil {
		_, _ = fmt.Fprint(w, text)
		return true
	}
	// Fallback: body is already plain text (should not normally happen).
	_, _ = w.Write(body)
	return true
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
