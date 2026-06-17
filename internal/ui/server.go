package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
	"github.com/phenixblue/k8shark/internal/transitions"
	"gopkg.in/yaml.v3"
)

type OpenOptions struct {
	ArchivePath string
	Port        string
	At          string
	Verbose     bool
}

type Server struct {
	address    string
	httpServer *http.Server
	done       chan struct{}
}

type explorerHandler struct {
	store       *server.CaptureStore
	at          time.Time
	verbose     bool
	archivePath string
	// clusterSpanning is the set of resource names (plural, lowercase) that
	// appear as namespace-scoped index paths in the majority of namespaces.
	// These are OLM-style resources (e.g. packagemanifests) that technically
	// have a namespace scope but return the full cluster list for any namespace.
	// They are excluded from individual namespace drill-downs.
	clusterSpanning map[string]bool
}

type treeResponse struct {
	CapturedAt    time.Time       `json:"captured_at"`
	CapturedUntil time.Time       `json:"captured_until"`
	Namespaces    []namespaceNode `json:"namespaces"`
	ClusterScoped []string        `json:"cluster_scoped"` // resource kind names only; items loaded on demand
	ResourceKinds []string        `json:"resource_kinds"`
}

type timestampsResponse struct {
	CapturedAt    time.Time `json:"captured_at"`
	CapturedUntil time.Time `json:"captured_until"`
	DefaultAt     string    `json:"default_at,omitempty"`
	TotalCount    int       `json:"total_count"`
	Sampled       bool      `json:"sampled"`
	Timestamps    []string  `json:"timestamps"`
}

type namespaceNode struct {
	Name string `json:"name"`
	// Counts is populated for new archives (where IndexEntry.Counts exists)
	// and lets the UI render namespace-card chips on initial load instead of
	// waiting for the user to drill in. Omitted entirely for older archives —
	// the UI then falls back to its current "no chips until visited" behavior.
	Counts *namespaceCardCounts `json:"counts,omitempty"`
}

type namespaceCardCounts struct {
	Workloads int `json:"workloads"`
	Pods      int `json:"pods"`
	Resources int `json:"resources"`
}

type workloadNode struct {
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Status   string            `json:"status,omitempty"`
	Age      string            `json:"age,omitempty"`
	ListPath string            `json:"list_path"`
	Labels   map[string]string `json:"labels,omitempty"`
	Pods     []podNode         `json:"pods,omitempty"`
}

type podNode struct {
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Status     string            `json:"status,omitempty"`
	Age        string            `json:"age,omitempty"`
	ListPath   string            `json:"list_path"`
	Labels     map[string]string `json:"labels,omitempty"`
	OwnerKind  string            `json:"owner_kind,omitempty"`
	OwnerName  string            `json:"owner_name,omitempty"`
	Containers []containerNode   `json:"containers,omitempty"`
}

type containerNode struct {
	Name string `json:"name"`
}

type resourceNode struct {
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Status   string            `json:"status,omitempty"`
	Age      string            `json:"age,omitempty"`
	ListPath string            `json:"list_path"`
	Labels   map[string]string `json:"labels,omitempty"`
}

type namespaceDetailResponse struct {
	Name      string         `json:"name"`
	Workloads []workloadNode `json:"workloads"`
	Pods      []podNode      `json:"pods"`
	Resources []resourceNode `json:"resources"`
}

type listItem struct {
	path string
	item map[string]any
}

func Open(opts OpenOptions) (*Server, error) {
	ar, err := archive.Open(opts.ArchivePath)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}

	store, err := server.LoadStore(ar)
	if err != nil {
		_ = ar.Close()
		return nil, fmt.Errorf("loading capture: %w", err)
	}

	at, err := parseReplayAt(store.Metadata.CapturedAt, store.Metadata.CapturedUntil, opts.At)
	if err != nil {
		_ = ar.Close()
		return nil, err
	}

	port := opts.Port
	if port == "" || port == "0" {
		port = "0"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		_ = ar.Close()
		return nil, fmt.Errorf("listening: %w", err)
	}

	h := &explorerHandler{store: store, at: at, verbose: opts.Verbose, archivePath: opts.ArchivePath}
	h.clusterSpanning = computeClusterSpanningResources(store)
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.serveIndex)
	mux.HandleFunc("/api/ui/tree", h.serveTree)
	mux.HandleFunc("/api/ui/tree/namespace", h.serveTreeNamespace)
	mux.HandleFunc("/api/ui/detail", h.serveDetail)
	mux.HandleFunc("/api/ui/timestamps", h.serveTimestamps)
	mux.HandleFunc("/api/ui/transitions", h.serveTransitions)
	mux.HandleFunc("/api/ui/object-history", h.serveObjectHistory)

	httpSrv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = httpSrv.Serve(ln)
	}()

	addr := fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)
	return &Server{address: addr, httpServer: httpSrv, done: done}, nil
}

func (s *Server) Address() string { return s.address }

func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
	<-s.done
}

func (s *Server) Wait() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-s.done:
	case <-sigCh:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(ctx)
		<-s.done
	}
	return nil
}

func (h *explorerHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (h *explorerHandler) serveTree(w http.ResponseWriter, r *http.Request) {
	// Index-only walk: no record bodies are read.
	// Returns the namespace list, cluster-scoped resource kinds, and all resource kinds.
	// Clients load individual namespace items on demand via /api/ui/tree/namespace.
	nsSet := map[string]struct{}{}
	clusterKindSet := map[string]struct{}{}
	kindSet := map[string]struct{}{}
	for path := range h.store.Index {
		_, _, resource, ns, _ := parseAPIPath(baseAPIPath(path))
		if resource == "" {
			continue
		}
		kind := kindFromResource(resource)
		kindSet[kind] = struct{}{}
		if ns == "" {
			clusterKindSet[kind] = struct{}{}
		} else {
			nsSet[ns] = struct{}{}
		}
	}
	// Also collect namespaces from watch-event paths, which are always
	// namespace-scoped and may reveal namespaces absent from the REST index
	// (e.g. when resources were only captured via cluster-wide list endpoints).
	for path := range h.store.WatchIndex {
		_, _, resource, ns, _ := parseAPIPath(baseAPIPath(path))
		if resource == "" || ns == "" {
			continue
		}
		nsSet[ns] = struct{}{}
	}
	// Also enumerate namespaces from the captured /api/v1/namespaces list.
	// This surfaces namespaces that exist in the cluster but have no per-resource
	// paths in the capture (e.g. empty namespaces or ones not included in the
	// capture config). We do a best-effort read; if the record is absent we
	// gracefully skip.
	if nsListBody, code, err := h.store.ReconstructAt("/api/v1/namespaces", h.at); err == nil && code == 200 {
		var nsList struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if json.Unmarshal(nsListBody, &nsList) == nil {
			for _, item := range nsList.Items {
				if item.Metadata.Name != "" {
					nsSet[item.Metadata.Name] = struct{}{}
				}
			}
		}
	}
	// Pull per-(ns, resource) counts from IndexEntry.Counts (no body reads).
	// Older archives have no Counts in their index entries; nsCounts will
	// simply be empty for those namespaces and the card chips stay blank
	// until the user drills in, matching the prior behavior.
	rawCounts := h.store.NamespaceItemCountsAt(h.at)
	namespaces := make([]namespaceNode, 0, len(nsSet))
	for ns := range nsSet {
		node := namespaceNode{Name: ns}
		if byResource, ok := rawCounts[ns]; ok {
			c := categorizeNamespaceCounts(byResource, h.clusterSpanning)
			node.Counts = &c
		}
		namespaces = append(namespaces, node)
	}
	sort.Slice(namespaces, func(i, j int) bool { return namespaces[i].Name < namespaces[j].Name })

	clusterKinds := make([]string, 0, len(clusterKindSet))
	for k := range clusterKindSet {
		clusterKinds = append(clusterKinds, k)
	}
	sort.Strings(clusterKinds)

	kinds := make([]string, 0, len(kindSet))
	for k := range kindSet {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	writeJSON(w, http.StatusOK, treeResponse{
		CapturedAt:    h.store.Metadata.CapturedAt,
		CapturedUntil: h.store.Metadata.CapturedUntil,
		Namespaces:    namespaces,
		ClusterScoped: clusterKinds,
		ResourceKinds: kinds,
	})
}

// serveTreeNamespace loads all items for a single namespace (or cluster-scoped
// resources when ns="" / ns=":cluster") and returns the full workloads/pods/resources
// breakdown. This is called on demand as the user expands a namespace in the UI.
func (h *explorerHandler) serveTreeNamespace(w http.ResponseWriter, r *http.Request) {
	at, err := h.resolveRequestAt(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ns := r.URL.Query().Get("ns")
	if ns == ":cluster" {
		ns = ""
	}

	node, err := h.buildNamespaceAt(ns, at)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, node)
}

func (h *explorerHandler) serveDetail(w http.ResponseWriter, r *http.Request) {
	at, atErr := h.resolveRequestAt(r)
	if atErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": atErr.Error()})
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing path query parameter"})
		return
	}

	name := r.URL.Query().Get("name")
	var (
		body []byte
		code int
		err  error
	)
	if name != "" {
		body, code, err = h.findResourceBodyAt(path, name, at)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if code == http.StatusNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "item not found in response"})
			return
		}
	} else {
		body, code, err = h.store.Latest(path, at)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if code == http.StatusNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "path not found in capture"})
			return
		}
	}

	normalizedBody, inferred, err := normalizeDetailBody(body, path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "invalid object body"})
		return
	}
	body = normalizedBody

	prettyJSON := body
	var out bytes.Buffer
	if err := json.Indent(&out, body, "", "  "); err == nil {
		prettyJSON = out.Bytes()
	}
	var obj any
	if err := json.Unmarshal(body, &obj); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "invalid json body"})
		return
	}
	yml, _ := yaml.Marshal(obj)

	writeJSON(w, http.StatusOK, map[string]any{
		"path":        path,
		"status_code": code,
		"json":        string(prettyJSON),
		"yaml":        string(yml),
		"inferred":    inferred,
	})
}

func (h *explorerHandler) serveTimestamps(w http.ResponseWriter, r *http.Request) {
	times, totalCount, sampled := collectTimestamps(h.store.Index, 180)
	resp := timestampsResponse{
		CapturedAt:    h.store.Metadata.CapturedAt,
		CapturedUntil: h.store.Metadata.CapturedUntil,
		TotalCount:    totalCount,
		Sampled:       sampled,
		Timestamps:    times,
	}
	if !h.at.IsZero() {
		resp.DefaultAt = h.at.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

// transitionMarker is a lightweight summary of a transition for the timeline.
type transitionMarker struct {
	Time      string `json:"time"`
	EventType string `json:"event_type"`
	Resource  string `json:"resource"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

func (h *explorerHandler) serveTransitions(w http.ResponseWriter, r *http.Request) {
	resource := r.URL.Query().Get("resource")
	namespace := r.URL.Query().Get("namespace")

	all, err := transitions.LoadTransitions(h.archivePath, transitions.FilterOpts{
		Resource:  resource,
		Namespace: namespace,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	markers := make([]transitionMarker, 0, len(all))
	for _, t := range all {
		markers = append(markers, transitionMarker{
			Time:      t.Time.UTC().Format(time.RFC3339),
			EventType: t.EventType,
			Resource:  t.Resource,
			Namespace: t.Namespace,
			Name:      t.Name,
		})
	}
	writeJSON(w, http.StatusOK, markers)
}

// objectHistoryEntry represents one transition for a specific object.
type objectHistoryEntry struct {
	Time      string          `json:"time"`
	EventType string          `json:"event_type"`
	Before    json.RawMessage `json:"before,omitempty"`
	After     json.RawMessage `json:"after,omitempty"`
}

func (h *explorerHandler) serveObjectHistory(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing name query parameter"})
		return
	}
	resource := r.URL.Query().Get("resource")
	namespace := r.URL.Query().Get("namespace")

	all, err := transitions.LoadTransitions(h.archivePath, transitions.FilterOpts{
		Name:      name,
		Resource:  resource,
		Namespace: namespace,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	entries := make([]objectHistoryEntry, 0, len(all))
	for _, t := range all {
		entries = append(entries, objectHistoryEntry{
			Time:      t.Time.UTC().Format(time.RFC3339),
			EventType: t.EventType,
			Before:    t.Before,
			After:     t.After,
		})
	}
	writeJSON(w, http.StatusOK, entries)
}

func (h *explorerHandler) resolveRequestAt(r *http.Request) (time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("at"))
	if raw == "" {
		return h.at, nil
	}
	if strings.EqualFold(raw, "latest") {
		return time.Time{}, nil
	}
	return parseReplayAt(h.store.Metadata.CapturedAt, h.store.Metadata.CapturedUntil, raw)
}

func normalizeDetailBody(body []byte, path string) ([]byte, map[string]bool, error) {
	inferred := map[string]bool{"apiVersion": false, "kind": false}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, inferred, err
	}

	// Keep list/table/detail envelopes as-is.
	if _, hasItems := obj["items"]; hasItems {
		return body, inferred, nil
	}
	if strings.HasSuffix(asString(obj["kind"]), "Table") {
		return body, inferred, nil
	}

	group, version, resource, _, _ := parseAPIPath(path)
	if asString(obj["apiVersion"]) == "" {
		inferred["apiVersion"] = true
		if group == "" {
			obj["apiVersion"] = version
		} else {
			obj["apiVersion"] = group + "/" + version
		}
	}
	if asString(obj["kind"]) == "" {
		inferred["kind"] = true
		obj["kind"] = kindFromResource(resource)
	}

	b, err := json.Marshal(obj)
	if err != nil {
		return nil, inferred, err
	}
	return b, inferred, nil
}

func (h *explorerHandler) buildTree() (*treeResponse, error) {
	return h.buildTreeAt(h.at)
}

// buildTreeAt is kept for tests; it reconstructs the full tree the old way.
// New callers should use the split serveTree + serveTreeNamespace endpoints.
func (h *explorerHandler) buildTreeAt(at time.Time) (*treeResponse, error) {
	nsSet := map[string]struct{}{}
	clusterKindSet := map[string]struct{}{}
	kindSet := map[string]struct{}{}
	for path := range h.store.Index {
		_, _, resource, ns, _ := parseAPIPath(baseAPIPath(path))
		if resource == "" {
			continue
		}
		kind := kindFromResource(resource)
		kindSet[kind] = struct{}{}
		if ns == "" {
			clusterKindSet[kind] = struct{}{}
		} else {
			nsSet[ns] = struct{}{}
		}
	}
	rawCounts := h.store.NamespaceItemCountsAt(at)
	namespaces := make([]namespaceNode, 0, len(nsSet))
	for ns := range nsSet {
		node := namespaceNode{Name: ns}
		if byResource, ok := rawCounts[ns]; ok {
			c := categorizeNamespaceCounts(byResource, h.clusterSpanning)
			node.Counts = &c
		}
		namespaces = append(namespaces, node)
	}
	sort.Slice(namespaces, func(i, j int) bool { return namespaces[i].Name < namespaces[j].Name })

	clusterKinds := make([]string, 0, len(clusterKindSet))
	for k := range clusterKindSet {
		clusterKinds = append(clusterKinds, k)
	}
	sort.Strings(clusterKinds)

	kinds := make([]string, 0, len(kindSet))
	for k := range kindSet {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	return &treeResponse{
		CapturedAt:    h.store.Metadata.CapturedAt,
		CapturedUntil: h.store.Metadata.CapturedUntil,
		Namespaces:    namespaces,
		ClusterScoped: clusterKinds,
		ResourceKinds: kinds,
	}, nil
}

// buildNamespaceAt loads all items for the given namespace (empty = cluster-scoped)
// and returns the full workloads / pods / resources breakdown.
func (h *explorerHandler) buildNamespaceAt(targetNS string, at time.Time) (*namespaceDetailResponse, error) {
	workloadKinds := map[string]bool{
		"deployments": true, "statefulsets": true, "daemonsets": true,
		"jobs": true, "replicasets": true,
	}

	// Collect candidate paths for this namespace.
	// For a named namespace:
	//   - Always include namespace-scoped paths (/api/v1/namespaces/<ns>/pods).
	//   - Include cluster-wide paths (/apis/apps/v1/deployments) ONLY for
	//     resource types that have no namespace-scoped path for this namespace
	//     in the index (e.g. portworx resources captured only at cluster scope).
	//     This prevents truly cluster-scoped resources (ClusterRoles, Nodes…)
	//     from appearing in the namespace drill-down.
	// For the cluster-scoped view (targetNS == ""):
	//   - Only include paths with no /namespaces/ segment.
	byRes := map[string][]string{}
	if targetNS == "" {
		// cluster-scoped: only true cluster-wide paths
		for path := range h.store.Index {
			_, _, resource, ns, _ := parseAPIPath(baseAPIPath(path))
			if resource == "" || ns != "" {
				continue
			}
			byRes[resource] = append(byRes[resource], path)
		}
	} else {
		// named namespace: first pass — collect namespace-scoped paths
		nsScopedResources := map[string]bool{}
		for path := range h.store.Index {
			_, _, resource, ns, _ := parseAPIPath(baseAPIPath(path))
			if resource == "" || ns != targetNS {
				continue
			}
			// Skip OLM-style resources that appear across the majority of
			// namespaces — they belong only in the cluster-scoped card.
			if h.clusterSpanning[resource] {
				continue
			}
			byRes[resource] = append(byRes[resource], path)
			nsScopedResources[resource] = true
		}
		// second pass — add cluster-wide paths only for resources not already
		// found via namespace-scoped paths
		for path := range h.store.Index {
			_, _, resource, ns, _ := parseAPIPath(baseAPIPath(path))
			if resource == "" || ns != "" {
				continue
			}
			if nsScopedResources[resource] {
				continue // already covered by ns-scoped path; skip to avoid cluster-scoped pollution
			}
			byRes[resource] = append(byRes[resource], path)
		}
	}

	node := &namespaceDetailResponse{
		Name:      targetNS,
		Workloads: []workloadNode{},
		Pods:      []podNode{},
		Resources: []resourceNode{},
	}

	for resource, candidates := range byRes {
		items, ok := h.loadResourceItemsAt(candidates, at)
		if !ok {
			continue
		}
		for _, entry := range items {
			itemNS := getMetaNamespace(entry.item)
			// cluster-scoped view: skip any item that belongs to a namespace
			if targetNS == "" && itemNS != "" {
				continue
			}
			// named-namespace view: skip items with no namespace (truly
			// cluster-scoped resources like ClusterRoles that happen to be
			// returned by cluster-wide list endpoints) and items from a
			// different namespace
			if targetNS != "" && itemNS != targetNS {
				continue
			}
			if workloadKinds[resource] {
				node.Workloads = append(node.Workloads, workloadNode{
					Kind:     firstNonEmpty(asString(entry.item["kind"]), kindFromResource(resource)),
					Name:     getMetaName(entry.item),
					Status:   summarizeStatus(entry.item),
					Age:      summarizeAge(entry.item),
					ListPath: entry.path,
					Labels:   getMetaLabels(entry.item),
				})
				continue
			}
			if resource == "pods" {
				node.Pods = append(node.Pods, toPodNode(entry.path, entry.item))
				continue
			}
			node.Resources = append(node.Resources, toResourceNode(resource, entry.path, entry.item))
		}
	}

	attachPodsToWorkloads(node)

	sort.Slice(node.Workloads, func(i, j int) bool {
		if node.Workloads[i].Kind == node.Workloads[j].Kind {
			return node.Workloads[i].Name < node.Workloads[j].Name
		}
		return node.Workloads[i].Kind < node.Workloads[j].Kind
	})
	sort.Slice(node.Pods, func(i, j int) bool { return node.Pods[i].Name < node.Pods[j].Name })
	sort.Slice(node.Resources, func(i, j int) bool {
		if node.Resources[i].Kind == node.Resources[j].Kind {
			return node.Resources[i].Name < node.Resources[j].Name
		}
		return node.Resources[i].Kind < node.Resources[j].Kind
	})
	return node, nil
}

func toResourceNode(resource, listPath string, item map[string]any) resourceNode {
	return resourceNode{
		Kind:     firstNonEmpty(asString(item["kind"]), kindFromResource(resource)),
		Name:     getMetaName(item),
		Status:   summarizeStatus(item),
		Age:      summarizeAge(item),
		ListPath: listPath,
		Labels:   getMetaLabels(item),
	}
}

func toPodNode(listPath string, item map[string]any) podNode {
	containers := make([]containerNode, 0)
	spec, _ := item["spec"].(map[string]any)
	if cList, ok := spec["containers"].([]any); ok {
		for _, c := range cList {
			if cm, ok := c.(map[string]any); ok {
				containers = append(containers, containerNode{Name: asString(cm["name"])})
			}
		}
	}

	ownKind, ownName := getOwner(item)
	return podNode{
		Kind:       firstNonEmpty(asString(item["kind"]), "Pod"),
		Name:       getMetaName(item),
		Status:     summarizeStatus(item),
		Age:        summarizeAge(item),
		ListPath:   listPath,
		Labels:     getMetaLabels(item),
		OwnerKind:  ownKind,
		OwnerName:  ownName,
		Containers: containers,
	}
}

func attachPodsToWorkloads(ns *namespaceDetailResponse) {
	byOwner := map[string]*workloadNode{}
	for i := range ns.Workloads {
		k := ns.Workloads[i].Kind + "/" + ns.Workloads[i].Name
		byOwner[k] = &ns.Workloads[i]
	}

	remaining := make([]podNode, 0)
	for _, p := range ns.Pods {
		if p.OwnerKind != "" && p.OwnerName != "" {
			if w := byOwner[p.OwnerKind+"/"+p.OwnerName]; w != nil {
				w.Pods = append(w.Pods, p)
				continue
			}
		}
		remaining = append(remaining, p)
	}
	ns.Pods = remaining
}

func parseListItems(body []byte) ([]map[string]any, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	itemsAny, ok := raw["items"].([]any)
	if !ok {
		return nil, fmt.Errorf("no items array")
	}
	items := make([]map[string]any, 0, len(itemsAny))
	for _, it := range itemsAny {
		if m, ok := it.(map[string]any); ok {
			items = append(items, m)
		}
	}
	return items, nil
}

func findListItemByName(body []byte, name string) ([]byte, bool) {
	items, err := parseListItems(body)
	if err != nil {
		return nil, false
	}
	for _, item := range items {
		if getMetaName(item) == name {
			b, err := json.Marshal(item)
			return b, err == nil
		}
	}
	return nil, false
}

func matchesObjectName(body []byte, name string) bool {
	var item map[string]any
	if err := json.Unmarshal(body, &item); err != nil {
		return false
	}
	return getMetaName(item) == name
}

func getMetaName(item map[string]any) string {
	meta, _ := item["metadata"].(map[string]any)
	return asString(meta["name"])
}

func getMetaNamespace(item map[string]any) string {
	meta, _ := item["metadata"].(map[string]any)
	return asString(meta["namespace"])
}

func getMetaLabels(item map[string]any) map[string]string {
	meta, _ := item["metadata"].(map[string]any)
	labelsAny, _ := meta["labels"].(map[string]any)
	out := make(map[string]string, len(labelsAny))
	for k, v := range labelsAny {
		out[k] = asString(v)
	}
	return out
}

func summarizeStatus(item map[string]any) string {
	status, _ := item["status"].(map[string]any)
	if phase := asString(status["phase"]); phase != "" {
		return phase
	}
	if conds, ok := status["conditions"].([]any); ok {
		for _, c := range conds {
			if cm, ok := c.(map[string]any); ok {
				if asString(cm["status"]) == "True" {
					return firstNonEmpty(asString(cm["type"]), "Ready")
				}
			}
		}
	}
	return ""
}

func summarizeAge(item map[string]any) string {
	meta, _ := item["metadata"].(map[string]any)
	ts := asString(meta["creationTimestamp"])
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func getOwner(item map[string]any) (string, string) {
	meta, _ := item["metadata"].(map[string]any)
	refs, _ := meta["ownerReferences"].([]any)
	for _, r := range refs {
		if rm, ok := r.(map[string]any); ok {
			return asString(rm["kind"]), asString(rm["name"])
		}
	}
	return "", ""
}

// computeClusterSpanningResources scans the index and returns the set of
// resource names (plural, lowercase) that appear as namespace-scoped paths in
// at least half of all discovered namespaces. These are resources like
// PackageManifests (OLM) that technically have a namespace scope but return
// the full cluster-wide list for any namespace. We exclude them from
// individual namespace drill-downs to avoid every namespace showing the same
// huge list.
func computeClusterSpanningResources(store *server.CaptureStore) map[string]bool {
	resNS := map[string]map[string]struct{}{} // resource → distinct namespaces
	totalNS := map[string]struct{}{}
	for path := range store.Index {
		_, _, resource, ns, _ := parseAPIPath(baseAPIPath(path))
		if resource == "" || ns == "" {
			continue
		}
		if resNS[resource] == nil {
			resNS[resource] = map[string]struct{}{}
		}
		resNS[resource][ns] = struct{}{}
		totalNS[ns] = struct{}{}
	}
	// Use 30% of namespaces as the spanning threshold. 50% was too high: OLM's
	// packagemanifests appear in ~45% of namespaces in typical OpenShift captures
	// (because OLM queries it for every namespace it manages) but not quite 50%.
	threshold := len(totalNS) * 30 / 100
	if threshold < 3 {
		threshold = 3
	}
	out := map[string]bool{}
	for res, nsSet := range resNS {
		if len(nsSet) >= threshold {
			out[res] = true
		}
	}
	return out
}

func kindFromResource(resource string) string {
	known := map[string]string{
		"pods":         "Pod",
		"deployments":  "Deployment",
		"statefulsets": "StatefulSet",
		"daemonsets":   "DaemonSet",
		"jobs":         "Job",
		"replicasets":  "ReplicaSet",
		"services":     "Service",
		"nodes":        "Node",
	}
	if k, ok := known[resource]; ok {
		return k
	}
	s := strings.TrimSuffix(resource, "s")
	if s == "" {
		return resource
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (h *explorerHandler) loadResourceItemsAt(candidates []string, at time.Time) ([]listItem, bool) {
	sorted := append([]string(nil), candidates...)
	sort.Slice(sorted, func(i, j int) bool {
		pi := pathPriority(sorted[i])
		pj := pathPriority(sorted[j])
		if pi == pj {
			return sorted[i] < sorted[j]
		}
		return pi < pj
	})

	var fallbackItems []listItem
	var foundFallback bool
	itemsByKey := make(map[string]listItem)
	var itemOrder []string
	for _, candidate := range sorted {
		bodies, ok := h.responseBodiesAt(candidate, at)
		if !ok {
			continue
		}
		for _, body := range bodies {
			entries, hadItems, ok := parseResponseItems(candidate, body)
			if !ok {
				continue
			}
			if hadItems {
				for _, entry := range entries {
					key := itemIdentityKey(entry.item)
					if _, exists := itemsByKey[key]; exists {
						continue
					}
					itemsByKey[key] = entry
					itemOrder = append(itemOrder, key)
				}
				continue
			}
			if !foundFallback {
				fallbackItems = entries
				foundFallback = true
			}
		}
		// Once we have concrete items from the regular (non-table) path,
		// skip remaining candidates — the ?as=Table path would only duplicate
		// the same items at the cost of additional disk reads.
		if len(itemOrder) > 0 && pathPriority(candidate) == 0 {
			break
		}
	}
	if len(itemOrder) > 0 {
		merged := make([]listItem, 0, len(itemOrder))
		for _, key := range itemOrder {
			merged = append(merged, itemsByKey[key])
		}
		return merged, true
	}
	if foundFallback {
		return fallbackItems, true
	}
	return nil, false
}

func parseResponseItems(path string, body []byte) ([]listItem, bool, bool) {
	if items, err := parseListItems(body); err == nil {
		entries := make([]listItem, 0, len(items))
		for _, item := range items {
			entries = append(entries, listItem{path: path, item: item})
		}
		return entries, len(entries) > 0, true
	}

	if items, ok := parseTableItems(body); ok {
		entries := make([]listItem, 0, len(items))
		for _, item := range items {
			entries = append(entries, listItem{path: path, item: item})
		}
		return entries, len(entries) > 0, true
	}

	var item map[string]any
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, false, false
	}
	meta, _ := item["metadata"].(map[string]any)
	if meta == nil || asString(meta["name"]) == "" {
		return nil, false, false
	}
	return []listItem{{path: path, item: item}}, true, true
}

func parseTableItems(body []byte) ([]map[string]any, bool) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false
	}
	kind := asString(raw["kind"])
	rowsAny, ok := raw["rows"].([]any)
	if !ok {
		return nil, false
	}
	if kind != "Table" && !strings.HasSuffix(kind, "Table") {
		return nil, false
	}

	items := make([]map[string]any, 0, len(rowsAny))
	for _, row := range rowsAny {
		rowMap, ok := row.(map[string]any)
		if !ok {
			continue
		}
		obj, ok := rowMap["object"].(map[string]any)
		if !ok {
			continue
		}
		meta, _ := obj["metadata"].(map[string]any)
		if asString(meta["name"]) == "" {
			continue
		}
		items = append(items, obj)
	}
	return items, true
}

func itemIdentityKey(item map[string]any) string {
	meta, _ := item["metadata"].(map[string]any)
	uid := asString(meta["uid"])
	if uid != "" {
		return uid
	}
	return firstNonEmpty(asString(item["kind"]), "?") + "/" + asString(meta["namespace"]) + "/" + asString(meta["name"])
}

func (h *explorerHandler) findResourceBodyAt(path, name string, at time.Time) ([]byte, int, error) {
	bodies, ok := h.responseBodiesAt(path, at)
	if !ok {
		return nil, http.StatusNotFound, nil
	}
	for i := len(bodies) - 1; i >= 0; i-- {
		body := bodies[i]
		if item, ok := findListItemByName(body, name); ok {
			return item, http.StatusOK, nil
		}
		if matchesObjectName(body, name) {
			return body, http.StatusOK, nil
		}
	}
	return nil, http.StatusNotFound, nil
}

func (h *explorerHandler) responseBodiesAt(path string, at time.Time) ([][]byte, bool) {
	body, code, err := h.store.ReconstructAt(path, at)
	if err == nil && code == http.StatusOK && len(body) > 0 {
		return [][]byte{body}, true
	}

	entry, ok := h.store.Index[path]
	if !ok || len(entry.Seqs) == 0 {
		return nil, false
	}

	bodies := make([][]byte, 0, len(entry.Seqs))
	for _, seq := range entry.Seqs {
		body, ok := h.readRecordBody(path, seq)
		if !ok {
			continue
		}
		bodies = append(bodies, body)
	}
	if len(bodies) == 0 {
		return nil, false
	}
	return bodies, true
}

func collectTimestamps(index capture.Index, limit int) ([]string, int, bool) {
	if len(index) == 0 {
		return nil, 0, false
	}
	uniq := make(map[time.Time]struct{})
	for _, entry := range index {
		for _, t := range entry.Times {
			if t.IsZero() {
				continue
			}
			uniq[t.UTC()] = struct{}{}
		}
	}
	out := make([]time.Time, 0, len(uniq))
	for t := range uniq {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	totalCount := len(out)
	if limit > 0 && len(out) > limit {
		out = sampleTimes(out, limit)
	}
	rfc := make([]string, 0, len(out))
	for _, t := range out {
		rfc = append(rfc, t.Format(time.RFC3339))
	}
	return rfc, totalCount, totalCount > len(rfc)
}

func sampleTimes(times []time.Time, limit int) []time.Time {
	if limit <= 0 || len(times) <= limit {
		return append([]time.Time(nil), times...)
	}
	if limit == 1 {
		return []time.Time{times[len(times)-1]}
	}

	selected := make([]time.Time, 0, limit)
	seen := make(map[time.Time]struct{}, limit)
	lastIdx := len(times) - 1
	for i := 0; i < limit; i++ {
		idx := (i * lastIdx) / (limit - 1)
		t := times[idx]
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		selected = append(selected, t)
	}
	if len(selected) == 0 || !selected[0].Equal(times[0]) {
		selected = append([]time.Time{times[0]}, selected...)
	}
	if !selected[len(selected)-1].Equal(times[len(times)-1]) {
		selected = append(selected, times[len(times)-1])
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Before(selected[j]) })
	if len(selected) > limit {
		selected = selected[len(selected)-limit:]
		sort.Slice(selected, func(i, j int) bool { return selected[i].Before(selected[j]) })
	}
	return selected
}

func (h *explorerHandler) readRecordBody(apiPath string, seq int) ([]byte, bool) {
	rec, err := h.store.ReadRecord(apiPath, seq)
	if err != nil {
		return nil, false
	}
	if rec.ResponseCode != http.StatusOK {
		return nil, false
	}
	return rec.ResponseBody, true
}

// uiWorkloadResources is the set of resource names categorized as
// "workloads" by the namespace-card grid. Mirrors the workloadKinds set in
// buildNamespaceAt so card chips and drill-down counts stay aligned.
var uiWorkloadResources = map[string]bool{
	"deployments":  true,
	"statefulsets": true,
	"daemonsets":   true,
	"jobs":         true,
	"replicasets":  true,
}

// categorizeNamespaceCounts maps raw per-resource counts into the three
// chip categories shown on namespace cards. Pods get their own bucket;
// recognised workload resources are summed; everything else is "resources".
//
// skipResources is the set of resource names to exclude — typically the
// cluster-spanning set (PackageManifests and similar resources that
// technically have namespace scope but return the same cluster-wide list
// for every namespace). The drill-down view applies the same filter, so
// excluding them here keeps the card chip totals in sync with what the
// user sees after navigating in.
func categorizeNamespaceCounts(byResource map[string]int, skipResources map[string]bool) namespaceCardCounts {
	var c namespaceCardCounts
	for resource, n := range byResource {
		if skipResources[resource] {
			continue
		}
		switch {
		case uiWorkloadResources[resource]:
			c.Workloads += n
		case resource == "pods":
			c.Pods += n
		default:
			c.Resources += n
		}
	}
	return c
}

func baseAPIPath(path string) string {
	if i := strings.Index(path, "?"); i >= 0 {
		return path[:i]
	}
	return path
}

func pathPriority(path string) int {
	if strings.Contains(path, "?as=Table") {
		return 2
	}
	if strings.Contains(path, "?") {
		return 1
	}
	return 0
}

func parseAPIPath(path string) (group, version, resource, namespace, name string) {
	path = baseAPIPath(path)
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case len(parts) >= 3 && parts[0] == "api":
		version = parts[1]
		if len(parts) == 3 {
			resource = parts[2]
		} else if len(parts) == 4 {
			resource = parts[2]
			name = parts[3]
		} else if len(parts) == 5 && parts[2] == "namespaces" {
			namespace = parts[3]
			resource = parts[4]
		} else if len(parts) == 6 && parts[2] == "namespaces" {
			namespace = parts[3]
			resource = parts[4]
			name = parts[5]
		}
	case len(parts) >= 4 && parts[0] == "apis":
		group = parts[1]
		version = parts[2]
		if len(parts) == 4 {
			resource = parts[3]
		} else if len(parts) == 5 {
			resource = parts[3]
			name = parts[4]
		} else if len(parts) == 6 && parts[3] == "namespaces" {
			namespace = parts[4]
			resource = parts[5]
		} else if len(parts) == 7 && parts[3] == "namespaces" {
			namespace = parts[4]
			resource = parts[5]
			name = parts[6]
		}
	}
	return
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseReplayAt(start, end time.Time, raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		d, derr := time.ParseDuration(raw)
		if derr != nil {
			return time.Time{}, fmt.Errorf("parsing --at %q: must be RFC3339 or a relative duration like -5m", raw)
		}
		at = end.Add(d)
	}
	if !start.IsZero() && at.Before(start) {
		return time.Time{}, fmt.Errorf("parsing --at %q: requested time %s is before capture start %s", raw, at.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	if !end.IsZero() && at.After(end) {
		return time.Time{}, fmt.Errorf("parsing --at %q: requested time %s is after capture end %s", raw, at.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	return at, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

const indexHTML = `<!doctype html>
<html>
<head>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
  <title>kshrk UI</title>
  <style>
    :root { --bg:#0b0f14; --panel:#111827; --text:#e5e7eb; --muted:#94a3b8; --line:#1f2937; --ok:#16a34a; --warn:#f59e0b; --bad:#dc2626; --accent:#22c55e; }
    body { margin:0; font-family: ui-sans-serif, -apple-system, Segoe UI, sans-serif; background:linear-gradient(145deg,#0b0f14,#0f172a); color:var(--text); }
	header { padding:14px 18px; border-bottom:1px solid var(--line); display:flex; justify-content:space-between; align-items:center; gap:12px; }
	.head-right { display:flex; align-items:center; gap:10px; }
	.snapshot-nav { display:flex; align-items:center; gap:6px; }
	.snapshot-select { min-width:220px; max-width:340px; box-sizing:border-box; padding:6px 8px; border:1px solid #334155; border-radius:8px; background:#0f172a; color:var(--text); }
	.snapshot-label { color:var(--muted); font-size:12px; }
	.snapshot-hint { color:var(--muted); font-size:11px; white-space:nowrap; }
	.snapshot-btn { padding:6px 10px; border-radius:8px; border:1px solid #334155; background:#0f172a; color:#cbd5e1; cursor:pointer; font-size:12px; }
	.snapshot-btn:hover { border-color:#64748b; }
	.snapshot-btn:disabled { opacity:.45; cursor:not-allowed; border-color:#1f2937; }
	.scrub-wrap { display:flex; align-items:center; gap:8px; min-width:240px; }
	.scrub { width:100%; accent-color:#22c55e; }
	.scrub-pos { color:var(--muted); font-size:11px; min-width:72px; text-align:right; }
	body.loading-snapshot .snapshot-select,
	body.loading-snapshot .snapshot-btn,
	body.loading-snapshot .scrub { opacity:.7; }
    .layout { display:grid; grid-template-columns:260px 1fr 40%; min-height:calc(100vh - 56px); }
    .panel { border-right:1px solid var(--line); overflow:auto; }
    .panel:last-child { border-right:none; border-left:1px solid var(--line); }
    .side { padding:12px; background:rgba(17,24,39,.85); }
    .search { width:100%; box-sizing:border-box; padding:8px 10px; border:1px solid #334155; border-radius:8px; background:#0f172a; color:var(--text); }
	.toggle-head { margin-top:10px; display:flex; justify-content:space-between; align-items:center; }
	.toggle-head-title { font-size:11px; color:var(--muted); text-transform:uppercase; letter-spacing:.6px; }
	.toggle-actions button { margin-left:6px; padding:3px 8px; border-radius:6px; border:1px solid #334155; background:#0f172a; color:#cbd5e1; font-size:11px; cursor:pointer; }
	.toggle-actions button:hover { border-color:#64748b; }
    .toggle-list { margin-top:10px; display:grid; gap:6px; }
    .tree { padding:12px; }
	.tree-head { display:flex; justify-content:space-between; align-items:center; margin-bottom:8px; }
	.tree-actions button { margin-left:6px; padding:3px 8px; border-radius:6px; border:1px solid #334155; background:#0f172a; color:#cbd5e1; font-size:11px; cursor:pointer; }
	.tree-actions button:hover { border-color:#64748b; }
    .crumbs { color:var(--muted); font-size:13px; margin-bottom:8px; }
    details { border:1px solid #1f2937; border-radius:8px; margin-bottom:8px; background:rgba(2,6,23,.45); }
    summary { cursor:pointer; padding:8px 10px; }
	.node { display:flex; align-items:center; gap:8px; padding:7px 10px; cursor:pointer; border-radius:6px; margin:3px 8px; border:1px solid transparent; outline:none; }
    .node:hover { border-color:#334155; background:#0f172a; }
	.node:focus-visible { border-color:#22c55e; box-shadow:0 0 0 2px rgba(34,197,94,.2); }
	.node.selected { border-color:#22c55e; background:rgba(34,197,94,.12); }
	.node-name { font-weight:600; letter-spacing:.1px; }
	.kind-chip { display:inline-block; min-width:68px; text-align:center; font-size:10px; font-weight:700; letter-spacing:.4px; text-transform:uppercase; padding:2px 6px; border-radius:999px; border:1px solid transparent; }
	.tone-core { background:rgba(59,130,246,.18); color:#bfdbfe; border-color:rgba(59,130,246,.45); }
	.tone-workload { background:rgba(234,179,8,.18); color:#fde68a; border-color:rgba(234,179,8,.45); }
	.tone-pod { background:rgba(16,185,129,.18); color:#a7f3d0; border-color:rgba(16,185,129,.45); }
	.tone-storage { background:rgba(249,115,22,.18); color:#fdba74; border-color:rgba(249,115,22,.45); }
	.tone-security { background:rgba(244,63,94,.18); color:#fecdd3; border-color:rgba(244,63,94,.45); }
	.tone-meta { background:rgba(148,163,184,.18); color:#e2e8f0; border-color:rgba(148,163,184,.45); }
    .muted { color:var(--muted); font-size:12px; }
    .badge { display:inline-block; font-size:11px; padding:2px 6px; border-radius:999px; margin-left:6px; background:#1f2937; }
    .ok { background:rgba(22,163,74,.2); color:#86efac; }
    .warn { background:rgba(245,158,11,.2); color:#fcd34d; }
    .bad { background:rgba(220,38,38,.2); color:#fca5a5; }
    .detail { padding:12px; background:rgba(17,24,39,.9); }
	.detail-summary { display:flex; flex-wrap:wrap; gap:6px; margin:8px 0 10px; }
	.summary-chip { font-size:11px; line-height:1; padding:5px 8px; border-radius:999px; border:1px solid #334155; color:#cbd5e1; background:#0f172a; }
	.tree-state { margin:8px; padding:10px 12px; border-radius:8px; border:1px solid #334155; background:rgba(15,23,42,.7); }
	.tree-state.error { border-color:rgba(220,38,38,.55); color:#fecaca; background:rgba(127,29,29,.25); }
	.tree-state.empty { border-color:rgba(148,163,184,.45); color:#cbd5e1; }
	.tree-state.loading { border-color:rgba(59,130,246,.45); color:#bfdbfe; }
    pre { white-space:pre-wrap; background:#020617; border:1px solid #1f2937; padding:10px; border-radius:8px; }
	.tabs { display:flex; align-items:center; justify-content:space-between; margin-bottom:6px; }
	.tabs-left button { margin-right:6px; padding:6px 10px; border-radius:7px; border:1px solid #334155; background:#0f172a; color:var(--text); cursor:pointer; }
	.copy-btn { padding:6px 10px; border-radius:7px; border:1px solid #334155; background:#0f172a; color:#cbd5e1; cursor:pointer; font-size:12px; }
	.copy-btn:hover { border-color:#64748b; }
	.detail-actions { display:flex; gap:6px; }
    .tabs button.active { border-color:var(--accent); color:#86efac; }
		.toast-stack { position:fixed; right:14px; bottom:14px; display:grid; gap:8px; z-index:40; pointer-events:none; }
		.toast { pointer-events:auto; max-width:420px; border:1px solid #334155; border-radius:8px; padding:10px 12px; background:rgba(15,23,42,.92); box-shadow:0 8px 28px rgba(2,6,23,.45); font-size:13px; }
		.toast.error { border-color:rgba(239,68,68,.55); color:#fecaca; }
		.toast.info { border-color:rgba(59,130,246,.55); color:#bfdbfe; }
		.ns-pager { margin:6px 10px 10px; display:flex; align-items:center; gap:8px; }
		.ns-pager button { padding:3px 8px; border-radius:6px; border:1px solid #334155; background:#0f172a; color:#cbd5e1; font-size:11px; cursor:pointer; }
		.ns-pager button:hover { border-color:#64748b; }
		.scrub-track { position:relative; flex:1; height:24px; display:flex; align-items:center; }
		.scrub-track .scrub { position:relative; z-index:2; }
		.marker-layer { position:absolute; left:8px; right:8px; top:0; bottom:0; pointer-events:none; z-index:1; }
		.marker-dot { position:absolute; width:6px; height:6px; border-radius:50%; transform:translate(-50%,-50%); top:50%; pointer-events:auto; cursor:pointer; opacity:.85; }
		.marker-dot:hover { opacity:1; transform:translate(-50%,-50%) scale(1.6); }
		.marker-dot.ev-added { background:#22c55e; }
		.marker-dot.ev-modified { background:#f59e0b; }
		.marker-dot.ev-deleted { background:#ef4444; }
		.marker-tooltip { position:absolute; bottom:calc(100% + 6px); left:50%; transform:translateX(-50%); background:#1e293b; border:1px solid #334155; border-radius:6px; padding:4px 8px; font-size:11px; color:#e2e8f0; white-space:nowrap; pointer-events:none; z-index:10; }
		.history-list { margin:0; padding:0; list-style:none; }
		.history-item { padding:8px 10px; border-bottom:1px solid #1f2937; cursor:pointer; display:flex; align-items:center; gap:8px; }
		.history-item:hover { background:#0f172a; }
		.history-item.active { background:rgba(34,197,94,.12); border-left:3px solid #22c55e; }
		.history-event { font-size:11px; font-weight:700; letter-spacing:.4px; text-transform:uppercase; padding:2px 6px; border-radius:999px; }
		.history-event.ev-added { background:rgba(34,197,94,.2); color:#86efac; }
		.history-event.ev-modified { background:rgba(245,158,11,.2); color:#fcd34d; }
		.history-event.ev-deleted { background:rgba(239,68,68,.2); color:#fca5a5; }
		.history-time { color:var(--muted); font-size:12px; }
		.diff-block { white-space:pre-wrap; font-family:ui-monospace,SFMono-Regular,monospace; font-size:12px; line-height:1.6; }
		.diff-line-add { color:#86efac; }
		.diff-line-del { color:#fca5a5; }
		.diff-line-hdr { color:#94a3b8; }
		.history-empty { padding:16px; color:var(--muted); font-size:13px; }

	/* --- namespace card grid --- */
	.ns-grid { display:grid; grid-template-columns:repeat(auto-fill,minmax(200px,1fr)); gap:12px; padding:14px; }
	.ns-card { background:rgba(15,23,42,.75); border:1px solid #1f2937; border-radius:10px; padding:14px 16px; cursor:pointer; transition:border-color .15s,box-shadow .15s; user-select:none; }
	.ns-card:hover { border-color:#334155; box-shadow:0 2px 12px rgba(0,0,0,.35); }
	.ns-card:focus-visible { outline:none; border-color:#22c55e; box-shadow:0 0 0 2px rgba(34,197,94,.2); }
	.ns-card.cluster { border-color:rgba(59,130,246,.3); background:rgba(15,23,42,.9); }
	.ns-card-name { font-weight:700; font-size:14px; margin-bottom:8px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
	.ns-card-chips { display:flex; flex-wrap:wrap; gap:4px; }
	.ns-card-chip { font-size:10px; padding:2px 6px; border-radius:999px; background:#1f2937; color:var(--muted); }
	.ns-back { display:inline-flex; align-items:center; gap:6px; padding:5px 10px; border-radius:7px; border:1px solid #334155; background:#0f172a; color:#cbd5e1; cursor:pointer; font-size:12px; margin:10px 12px 2px; }
	.ns-back:hover { border-color:#64748b; }
	.ns-drilldown { padding-bottom:12px; }
	.ns-section { margin:0 12px 8px; }
	.ns-section-header { font-size:11px; color:var(--muted); text-transform:uppercase; letter-spacing:.6px; padding:6px 0 4px; border-bottom:1px solid #1f2937; margin-bottom:4px; }
  </style>
</head>
<body>
  <header>
    <div><strong>kshrk capture explorer</strong></div>
		<div class="head-right">
			<span class="snapshot-label">Snapshot</span>
			<div class="snapshot-nav">
				<button id="snapshotPrev" class="snapshot-btn" type="button" title="Jump to older snapshot">Prev</button>
				<select id="snapshotAt" class="snapshot-select" title="Select capture timestamp"></select>
				<button id="snapshotNext" class="snapshot-btn" type="button" title="Jump to newer snapshot">Next</button>
			</div>
			<div class="scrub-wrap">
				<div class="scrub-track">
					<div id="markerLayer" class="marker-layer"></div>
					<input id="timelineScrub" class="scrub" type="range" min="0" max="0" step="1" value="0"/>
				</div>
				<span id="timelinePosition" class="scrub-pos"></span>
			</div>
			<div id="snapshotHint" class="snapshot-hint"></div>
			<div id="meta" class="muted"></div>
		</div>
  </header>
  <div class="layout">
    <aside class="panel side">
      <input class="search" id="search" placeholder="Search name or label..."/>
			<div class="toggle-head">
				<span class="toggle-head-title">Kinds</span>
				<span class="toggle-actions">
					<button id="toggleAll" type="button">All</button>
					<button id="toggleNone" type="button">None</button>
				</span>
			</div>
      <div class="toggle-list" id="toggles"></div>
    </aside>
    <main class="panel tree">
			<div class="tree-head">
				<span class="toggle-head-title" id="treeHeadLabel">Namespaces</span>
			</div>
      <div class="crumbs" id="crumbs">Cluster</div>
      <div id="tree"></div>
    </main>
    <section class="detail">
      <h3 id="detailTitle">Select a resource</h3>
			<div id="detailSummary" class="detail-summary"></div>
      <div class="tabs">
				<span class="tabs-left">
					<button id="tabJson" class="active">JSON</button>
					<button id="tabYaml">YAML</button>
					<button id="tabHistory">History</button>
					<button id="tabDiff">Diff</button>
				</span>
				<span class="detail-actions">
					<button id="copyDetail" class="copy-btn" type="button">Copy JSON</button>
					<button id="downloadDetail" class="copy-btn" type="button">Download JSON</button>
				</span>
      </div>
      <pre id="detailBody">Click a node in the tree to inspect details.</pre>
      <div id="historyPane" style="display:none"></div>
      <div id="diffPane" style="display:none"></div>
    </section>
  </div>
	<div id="toasts" class="toast-stack" aria-live="polite"></div>
  <script>
    let treeData = null;
    let nsCache = {};
    const nsFetchConcurrency = 3;
    let nsFetchActive = 0;
    const nsFetchQueue = [];
    function loadNsDetail(nsName) {
      if (nsCache[nsName] && !nsCache[nsName]._loading) return;
      if (nsCache[nsName] && nsCache[nsName]._loading) return; // already queued/in-flight
      nsCache[nsName] = { _loading: true };
      nsFetchQueue.push(nsName);
      nsDrainQueue();
    }
    function nsDrainQueue() {
      while (nsFetchActive < nsFetchConcurrency && nsFetchQueue.length > 0) {
        const nsName = nsFetchQueue.shift();
        nsFetchActive++;
        nsFetchOne(nsName).finally(() => {
          nsFetchActive--;
          nsDrainQueue();
        });
      }
    }
    async function nsFetchOne(nsName) {
      try {
        const q = withAtQuery(new URLSearchParams());
        q.set('ns', nsName === ':cluster' ? ':cluster' : nsName);
        const res = await fetch('/api/ui/tree/namespace?' + q.toString());
        const data = await res.json();
        nsCache[nsName] = data;
      } catch (_) {
        delete nsCache[nsName];
        return;
      }
      render();
    }
    let activeKinds = new Set();
	let activeKindsInitialized = false;
    let selected = null;
    let activeTab = 'json';
	let visibleNodes = [];
	let activeNodeIdx = -1;
	const storageKindsKey = 'kshrk.ui.activeKinds.v1';
	let currentAt = '';
	let currentNS = null; // null = card grid, ':cluster' = cluster-scoped, string = namespace name
	let timelinePoints = [];
	let scrubDebounce = null;
	let lastLoadedAt = '';
	let refreshToken = 0;
	let allTransitions = [];
	let objectHistory = [];

    const el = {
      tree: document.getElementById('tree'),
      search: document.getElementById('search'),
      toggles: document.getElementById('toggles'),
      crumbs: document.getElementById('crumbs'),
      meta: document.getElementById('meta'),
      detailTitle: document.getElementById('detailTitle'),
	detailSummary: document.getElementById('detailSummary'),
      detailBody: document.getElementById('detailBody'),
      tabJson: document.getElementById('tabJson'),
			tabYaml: document.getElementById('tabYaml'),
			copyDetail: document.getElementById('copyDetail'),
			downloadDetail: document.getElementById('downloadDetail'),
			toggleAll: document.getElementById('toggleAll'),
			toggleNone: document.getElementById('toggleNone'),
			treeHeadLabel: document.getElementById('treeHeadLabel'),
			snapshotPrev: document.getElementById('snapshotPrev'),
			snapshotAt: document.getElementById('snapshotAt'),
			snapshotNext: document.getElementById('snapshotNext'),
			timelineScrub: document.getElementById('timelineScrub'),
			timelinePosition: document.getElementById('timelinePosition'),
			snapshotHint: document.getElementById('snapshotHint'),
			toasts: document.getElementById('toasts'),
			tabHistory: document.getElementById('tabHistory'),
			tabDiff: document.getElementById('tabDiff'),
			historyPane: document.getElementById('historyPane'),
			diffPane: document.getElementById('diffPane'),
			markerLayer: document.getElementById('markerLayer')
    };

    el.tabJson.onclick = () => setTab('json');
    el.tabYaml.onclick = () => setTab('yaml');
	el.tabHistory.onclick = () => setTab('history');
	el.tabDiff.onclick = () => setTab('diff');
	el.copyDetail.onclick = () => copyActiveDetail();
	el.downloadDetail.onclick = () => downloadActiveDetail();
    el.search.oninput = render;
		el.toggleAll.onclick = () => setAllKinds(true);
		el.toggleNone.onclick = () => setAllKinds(false);
	el.snapshotPrev.onclick = () => stepSnapshot(1);
	el.snapshotAt.onchange = () => {
		currentAt = el.snapshotAt.value || '';
		syncSnapshotButtons();
		syncScrubber();
		syncAtInURL();
		refreshTree();
	};
	el.snapshotNext.onclick = () => stepSnapshot(-1);
	el.timelineScrub.oninput = () => {
		syncScrubberLabel();
		if (scrubDebounce) window.clearTimeout(scrubDebounce);
		scrubDebounce = window.setTimeout(() => {
			applyScrubberSelection(false);
		}, 180);
	};
	el.timelineScrub.onchange = () => {
		if (scrubDebounce) {
			window.clearTimeout(scrubDebounce);
			scrubDebounce = null;
		}
		applyScrubberSelection(true);
	};
	document.addEventListener('keydown', onTreeKeyDown);

	function syncAtInURL() {
		const u = new URL(window.location.href);
		if (currentAt) {
			u.searchParams.set('at', currentAt);
		} else {
			u.searchParams.delete('at');
		}
		window.history.replaceState({}, '', u.toString());
	}

	function initAtFromURL() {
		const u = new URL(window.location.href);
		const at = (u.searchParams.get('at') || '').trim();
		if (at) {
			currentAt = at;
		}
	}

	function withAtQuery(params) {
		if (currentAt) {
			params.set('at', currentAt);
		}
		return params;
	}

	function formatAtLabel(v) {
		if (!v || v === 'latest') return 'Latest available';
		const d = new Date(v);
		if (Number.isNaN(d.getTime())) return v;
		return d.toISOString();
	}

	function snapshotSequence() {
		return Array.from(el.snapshotAt.options).map((opt) => opt.value);
	}

	function syncSnapshotButtons() {
		const sequence = snapshotSequence();
		const idx = sequence.indexOf(currentAt || 'latest');
		el.snapshotPrev.disabled = idx < 0 || idx >= sequence.length - 1;
		el.snapshotNext.disabled = idx <= 0;
	}

	function syncScrubberLabel() {
		if (!timelinePoints.length) {
			el.timelinePosition.textContent = '';
			return;
		}
		const idx = Number(el.timelineScrub.value || 0);
		const pos = Math.max(0, Math.min(idx, timelinePoints.length - 1));
		const ts = timelinePoints[pos] || '';
		el.timelinePosition.textContent = (pos + 1) + '/' + timelinePoints.length + (ts ? (' · ' + formatAtLabel(ts)) : '');
	}

	function syncScrubber() {
		if (!timelinePoints.length) {
			el.timelineScrub.min = '0';
			el.timelineScrub.max = '0';
			el.timelineScrub.value = '0';
			el.timelineScrub.disabled = true;
			syncScrubberLabel();
			return;
		}
		el.timelineScrub.disabled = false;
		el.timelineScrub.min = '0';
		el.timelineScrub.max = String(timelinePoints.length - 1);
		let target = timelinePoints.length - 1;
		if (currentAt && currentAt !== 'latest') {
			const idx = timelinePoints.indexOf(currentAt);
			if (idx >= 0) target = idx;
		}
		el.timelineScrub.value = String(target);
		syncScrubberLabel();
	}

	function applyScrubberSelection(forceRefresh) {
		if (!timelinePoints.length) return;
		const idx = Number(el.timelineScrub.value || 0);
		const pos = Math.max(0, Math.min(idx, timelinePoints.length - 1));
		const ts = timelinePoints[pos];
		const newest = timelinePoints[timelinePoints.length - 1];
		const nextAt = ts === newest ? 'latest' : ts;
		if (nextAt === currentAt && !forceRefresh) {
			syncScrubberLabel();
			return;
		}
		currentAt = nextAt;
		el.snapshotAt.value = currentAt;
		syncSnapshotButtons();
		syncScrubber();
		syncAtInURL();
		refreshTree(!!forceRefresh);
	}

	function stepSnapshot(delta) {
		const sequence = snapshotSequence();
		if (sequence.length === 0) {
			return;
		}
		const current = currentAt || 'latest';
		const idx = sequence.indexOf(current);
		if (idx < 0) {
			return;
		}
		const nextIdx = Math.max(0, Math.min(sequence.length - 1, idx + delta));
		if (nextIdx === idx) {
			return;
		}
		currentAt = sequence[nextIdx];
		el.snapshotAt.value = currentAt;
		syncSnapshotButtons();
		syncScrubber();
		syncAtInURL();
		refreshTree(true);
	}

	function normalizedAt() {
		return currentAt && currentAt !== 'latest' ? currentAt : 'latest';
	}

	function updateMetaLine() {
		if (!treeData) {
			el.meta.textContent = 'capture unavailable';
			return;
		}
		const start = new Date(treeData.captured_at).toISOString();
		const end = new Date(treeData.captured_until).toISOString();
		const atLabel = formatAtLabel(currentAt);
		el.meta.textContent = 'capture ' + start + ' to ' + end + ' | view: ' + atLabel;
	}

	function renderSnapshotSelector(times, defaultAt, totalCount, sampled) {
		const asc = Array.isArray(times) ? times.slice() : [];
		timelinePoints = asc.slice();
		const opts = asc.slice().reverse();
		const unique = [];
		const seen = new Set();
		for (const ts of opts) {
			if (!ts || seen.has(ts)) continue;
			seen.add(ts);
			unique.push(ts);
		}

		el.snapshotAt.innerHTML = '';
		const latest = document.createElement('option');
		latest.value = 'latest';
		latest.textContent = 'Latest available';
		el.snapshotAt.appendChild(latest);

		for (const ts of unique) {
			const opt = document.createElement('option');
			opt.value = ts;
			opt.textContent = sampled ? (formatAtLabel(ts) + ' (sampled)') : formatAtLabel(ts);
			el.snapshotAt.appendChild(opt);
		}

		if (!currentAt && defaultAt) {
			currentAt = defaultAt;
		}
		if (!currentAt) {
			currentAt = 'latest';
		}
		if (currentAt !== 'latest' && !seen.has(currentAt)) {
			const opt = document.createElement('option');
			opt.value = currentAt;
			opt.textContent = formatAtLabel(currentAt) + ' (custom)';
			el.snapshotAt.appendChild(opt);
		}
		el.snapshotAt.value = currentAt;
		syncScrubber();
		if (sampled && totalCount > opts.length) {
			el.snapshotHint.textContent = 'showing ' + opts.length + ' sampled of ' + totalCount;
		} else if (totalCount > 0) {
			el.snapshotHint.textContent = totalCount + ' snapshot' + (totalCount === 1 ? '' : 's');
		} else {
			el.snapshotHint.textContent = '';
		}
		syncSnapshotButtons();
	}

    function setTab(tab) {
      activeTab = tab;
      el.tabJson.classList.toggle('active', tab === 'json');
      el.tabYaml.classList.toggle('active', tab === 'yaml');
      el.tabHistory.classList.toggle('active', tab === 'history');
      el.tabDiff.classList.toggle('active', tab === 'diff');
      el.detailBody.style.display = (tab === 'json' || tab === 'yaml') ? '' : 'none';
      el.historyPane.style.display = tab === 'history' ? '' : 'none';
      el.diffPane.style.display = tab === 'diff' ? '' : 'none';
			el.copyDetail.style.display = (tab === 'json' || tab === 'yaml') ? '' : 'none';
			el.downloadDetail.style.display = (tab === 'json' || tab === 'yaml') ? '' : 'none';
			if (tab === 'json' || tab === 'yaml') {
				el.copyDetail.textContent = tab === 'yaml' ? 'Copy YAML' : 'Copy JSON';
				el.downloadDetail.textContent = tab === 'yaml' ? 'Download YAML' : 'Download JSON';
			}
      if ((tab === 'json' || tab === 'yaml') && selected && selected.detail) {
        el.detailBody.textContent = selected.detail[tab] || '';
      }
      if (tab === 'history' && selected && selected.node) {
        loadObjectHistory(selected.node);
      }
      if (tab === 'diff') {
        renderDiffPane();
      }
    }

		function sanitizeFilenamePart(v) {
			return String(v || 'resource').replace(/[^a-zA-Z0-9._-]+/g, '-');
		}

		function downloadActiveDetail() {
			if (!selected || !selected.detail) {
				showToast('info', 'Select a resource first.');
				return;
			}
			const text = selected.detail[activeTab] || '';
			if (!text) {
				showToast('info', 'No content available to download.');
				return;
			}
			const ext = activeTab === 'yaml' ? 'yaml' : 'json';
			const mime = activeTab === 'yaml' ? 'application/yaml' : 'application/json';
			const name = sanitizeFilenamePart(selected.node && selected.node.name) + '.' + ext;
			const blob = new Blob([text], { type: mime });
			const url = URL.createObjectURL(blob);
			const a = document.createElement('a');
			a.href = url;
			a.download = name;
			document.body.appendChild(a);
			a.click();
			a.remove();
			URL.revokeObjectURL(url);
			showToast('info', (activeTab === 'yaml' ? 'YAML' : 'JSON') + ' downloaded as ' + name + '.');
		}

		async function copyActiveDetail() {
			if (!selected || !selected.detail) {
				showToast('info', 'Select a resource first.');
				return;
			}
			const text = selected.detail[activeTab] || '';
			if (!text) {
				showToast('info', 'No content available to copy.');
				return;
			}
			try {
				if (navigator.clipboard && navigator.clipboard.writeText) {
					await navigator.clipboard.writeText(text);
				} else {
					const tmp = document.createElement('textarea');
					tmp.value = text;
					document.body.appendChild(tmp);
					tmp.select();
					document.execCommand('copy');
					tmp.remove();
				}
				showToast('info', (activeTab === 'yaml' ? 'YAML' : 'JSON') + ' copied to clipboard.');
			} catch (err) {
				showToast('error', 'Copy failed: ' + (err && err.message ? err.message : String(err)));
			}
		}

		function setTreeState(type, message) {
			el.tree.innerHTML = '';
			const box = document.createElement('div');
			box.className = 'tree-state ' + type;
			box.textContent = message;
			el.tree.appendChild(box);
		}

		function showToast(type, message, timeoutMs) {
			const toast = document.createElement('div');
			toast.className = 'toast ' + type;
			toast.textContent = message;
			el.toasts.appendChild(toast);
			const ttl = timeoutMs || (type === 'error' ? 7000 : 3000);
			window.setTimeout(() => toast.remove(), ttl);
		}

		function setSnapshotLoading(loading) {
			document.body.classList.toggle('loading-snapshot', !!loading);
			el.snapshotAt.disabled = !!loading;
			el.timelineScrub.disabled = !!loading || timelinePoints.length === 0;
			if (loading) {
				el.snapshotPrev.disabled = true;
				el.snapshotNext.disabled = true;
			} else {
				syncSnapshotButtons();
			}
			if (loading) {
				el.snapshotHint.textContent = 'loading snapshot...';
			}
		}

		function updateDetailSummary(data, node) {
			el.detailSummary.innerHTML = '';
			if (!data || !data.json) return;
			let obj = null;
			try {
				obj = JSON.parse(data.json);
			} catch (_) {
				return;
			}
			const meta = obj.metadata || {};
			const refs = meta.ownerReferences || [];
			const owner = refs.length > 0 ? (refs[0].kind + '/' + refs[0].name) : '';
			const labels = meta.labels ? Object.keys(meta.labels).length : 0;
			const inferred = data.inferred || {};
			const completeness = (inferred.apiVersion || inferred.kind)
				? ('inferred: ' + [inferred.apiVersion ? 'apiVersion' : '', inferred.kind ? 'kind' : ''].filter(Boolean).join(', '))
				: 'inferred: none';
			const chips = [
				['kind', obj.kind || node.kind || ''],
				['name', meta.name || node.name || ''],
				['ns', meta.namespace || '-'],
				['labels', String(labels)],
				['owner', owner || '-'],
				['raw', completeness]
			];
			for (const [k, v] of chips) {
				const chip = document.createElement('span');
				chip.className = 'summary-chip';
				chip.textContent = k + ': ' + v;
				el.detailSummary.appendChild(chip);
			}
		}

		function loadPreferences() {
			try {
				const kindsRaw = window.localStorage.getItem(storageKindsKey);
				if (kindsRaw) {
					const parsed = JSON.parse(kindsRaw);
					if (Array.isArray(parsed)) {
						activeKinds = new Set(parsed);
						activeKindsInitialized = true;
					}
				}
			} catch (_) {}
		}

		function persistKinds() {
			try {
				window.localStorage.setItem(storageKindsKey, JSON.stringify(Array.from(activeKinds)));
			} catch (_) {}
		}

    function statusClass(status) {
      const s = (status || '').toLowerCase();
      if (s.includes('running') || s.includes('ready') || s.includes('active')) return 'ok';
      if (s.includes('pending') || s.includes('containercreating')) return 'warn';
      if (s.includes('failed') || s.includes('error') || s.includes('crash')) return 'bad';
      return '';
    }

    function nodeMatches(node, q, namespaceName) {
      if (!q) return true;
      const hay = [
			node.name,
			node.kind,
			node.status,
			node.age,
			namespaceName || '',
			node.list_path || '',
			...(Object.entries(node.labels || {}).map(([k,v]) => k + '=' + v))
		].join(' ').toLowerCase();
      return hay.includes(q);
    }

    function kindEnabled(kind) {
		return !activeKindsInitialized || activeKinds.has(kind);
    }

		function kindTone(kind) {
			const k = (kind || '').toLowerCase();
			if (k === 'pod' || k === 'container') return 'tone-pod';
			if (k.includes('deploy') || k.includes('stateful') || k.includes('daemon') || k.includes('job') || k.includes('replicaset')) return 'tone-workload';
			if (k.includes('secret') || k.includes('serviceaccount') || k.includes('role')) return 'tone-security';
			if (k.includes('persistent') || k.includes('storage') || k.includes('volume')) return 'tone-storage';
			if (k === 'node' || k === 'namespace' || k === 'service') return 'tone-core';
			return 'tone-meta';
		}

		function mkNode(node, crumbs, depth, namePrefix) {
      const d = document.createElement('div');
      d.className = 'node';
			d.tabIndex = 0;
			d.setAttribute('role', 'button');
			d.style.paddingLeft = (10 + (depth * 16)) + 'px';
      d.title = [node.kind, node.status, node.age, Object.entries(node.labels || {}).slice(0,5).map(([k,v]) => k + '=' + v).join(', ')].filter(Boolean).join(' | ');
      const badge = node.status ? '<span class="badge ' + statusClass(node.status) + '">' + node.status + '</span>' : '';
			const kindChip = '<span class="kind-chip ' + kindTone(node.kind) + '">' + (node.kind || 'Resource') + '</span>';
			const nodeName = '<span class="node-name">' + (namePrefix ? namePrefix + node.name : node.name) + '</span>';
			d.innerHTML = kindChip + nodeName + badge + (node.age ? ' <span class="muted">' + node.age + '</span>' : '');
			d._node = node;
			d._crumbs = crumbs;
			d._nodeKey = nodeIdentity(node);
      d.onclick = () => activateNodeElement(d, true);
			d.onkeydown = (ev) => {
				if (ev.key === 'Enter' || ev.key === ' ') {
					ev.preventDefault();
					activateNodeElement(d, false);
				}
			};
      return d;
    }

		function clearNodeSelection() {
			for (const n of visibleNodes) n.classList.remove('selected');
		}

		function selectNodeAt(index, focusNode) {
			if (visibleNodes.length === 0) {
				activeNodeIdx = -1;
				return;
			}
			activeNodeIdx = Math.max(0, Math.min(index, visibleNodes.length - 1));
			clearNodeSelection();
			const nodeEl = visibleNodes[activeNodeIdx];
			nodeEl.classList.add('selected');
			if (focusNode) nodeEl.focus();
			nodeEl.scrollIntoView({ block: 'nearest' });
		}

		function activateNodeElement(nodeEl, keepFocus) {
			const idx = visibleNodes.indexOf(nodeEl);
			if (idx >= 0) selectNodeAt(idx, keepFocus);
			showDetail(nodeEl._node, nodeEl._crumbs);
		}

		function normalizeListPath(path) {
			const raw = String(path || '');
			return raw.split('?')[0];
		}

		function namespaceFromPath(path) {
			const match = normalizeListPath(path).match(/\/namespaces\/([^/]+)\//);
			return match ? match[1] : '';
		}

		function nodeIdentity(node) {
			if (!node) return '';
			if ((node.kind || '') === 'Container') {
				return ['container', normalizeListPath(node.list_path), node.name || ''].join('|');
			}
			return [namespaceFromPath(node.list_path), node.kind || '', node.name || ''].join('|');
		}

		function restoreSelectionByKey(nodeKey) {
			if (!nodeKey || visibleNodes.length === 0) return false;
			const idx = visibleNodes.findIndex((n) => n._nodeKey === nodeKey);
			if (idx < 0) return false;
			selectNodeAt(idx, false);
			return true;
		}

		function onTreeKeyDown(ev) {
			const tag = (document.activeElement && document.activeElement.tagName) || '';
			if (tag === 'INPUT' || tag === 'TEXTAREA' || (document.activeElement && document.activeElement.isContentEditable)) return;
			if (ev.altKey && ev.key === 'ArrowLeft') {
				ev.preventDefault();
				stepSnapshot(1);
				return;
			}
			if (ev.altKey && ev.key === 'ArrowRight') {
				ev.preventDefault();
				stepSnapshot(-1);
				return;
			}
			if (visibleNodes.length === 0) return;
			if (ev.key === 'ArrowDown') {
				ev.preventDefault();
				selectNodeAt(activeNodeIdx < 0 ? 0 : activeNodeIdx + 1, true);
				return;
			}
			if (ev.key === 'ArrowUp') {
				ev.preventDefault();
				selectNodeAt(activeNodeIdx < 0 ? 0 : activeNodeIdx - 1, true);
				return;
			}
			if ((ev.key === 'Enter' || ev.key === ' ') && activeNodeIdx >= 0) {
				ev.preventDefault();
				activateNodeElement(visibleNodes[activeNodeIdx], false);
			}
		}

    async function showDetail(node, crumbs) {
			const previousKey = selected && selected.node ? nodeIdentity(selected.node) : '';
			const nextKey = nodeIdentity(node);
      selected = { node, crumbs };
			if (previousKey !== nextKey) {
				selectedHistoryEntry = null;
			}
      el.crumbs.textContent = crumbs.join(' / ');
      el.detailTitle.textContent = node.kind + ' ' + node.name;
			el.detailBody.textContent = 'Loading detail...';
			try {
				const q = withAtQuery(new URLSearchParams({ path: node.list_path, name: node.name }));
				const res = await fetch('/api/ui/detail?' + q.toString());
				const data = await res.json();
				if (!res.ok) {
					const msg = (data && data.error) ? data.error : ('request failed with status ' + res.status);
					throw new Error(msg);
				}
				selected.detail = data;
				updateDetailSummary(data, node);
				if (activeTab === 'json' || activeTab === 'yaml') {
					el.detailBody.textContent = data[activeTab] || JSON.stringify(data, null, 2);
				}
			} catch (err) {
				selected.detail = null;
				el.detailSummary.innerHTML = '';
				const msg = (err && err.message ? err.message : String(err));
				el.detailBody.textContent = 'Failed to load detail: ' + msg;
				showToast('error', 'Detail request failed: ' + msg);
			}
			if (activeTab === 'history') loadObjectHistory(node);
			if (activeTab === 'diff') renderDiffPane();
    }

    function render() {
			if (!treeData) {
				setTreeState('loading', 'Loading captured resources...');
				return;
			}
			el.tree.innerHTML = '';
			if (currentNS === null) {
				renderCardGrid();
			} else {
				renderNsDrilldown();
			}
		}

		// ── card grid ────────────────────────────────────────────────────────────
		function renderCardGrid() {
			el.treeHeadLabel.textContent = 'Namespaces';
			el.crumbs.textContent = 'Cluster';
			const q = el.search.value.trim().toLowerCase();

			const grid = document.createElement('div');
			grid.className = 'ns-grid';

			// cluster-scoped card
			const allNS = [{ name: ':cluster', _cluster: true }].concat(treeData.namespaces || []);
			let shown = 0;
			for (const ns of allNS) {
				const label = ns._cluster ? 'Cluster-scoped' : ns.name;
				if (q && !label.toLowerCase().includes(q)) continue;
				const detail = nsCache[ns.name] || {};
				// If the user has drilled into this ns, the live arrays beat any
				// precomputed counts (handles snapshot scrubbing edge cases).
				// Otherwise fall back to the index-derived counts shipped on the
				// namespace node, so cards show chips before any drill-down.
				let workloads, pods, resources, loaded;
				if (nsCache[ns.name] && !detail._loading) {
					workloads = (detail.workloads || []).length;
					pods = (detail.pods || []).length;
					resources = (detail.resources || []).length;
					loaded = true;
				} else if (ns.counts) {
					workloads = ns.counts.workloads || 0;
					pods = ns.counts.pods || 0;
					resources = ns.counts.resources || 0;
					loaded = true;
				} else {
					workloads = 0;
					pods = 0;
					resources = 0;
					loaded = false;
				}

				const card = document.createElement('div');
				card.className = 'ns-card' + (ns._cluster ? ' cluster' : '');
				card.tabIndex = 0;
				card.setAttribute('role', 'button');
				card.title = 'Open ' + label;

				const nameEl = document.createElement('div');
				nameEl.className = 'ns-card-name';
				nameEl.textContent = label;
				card.appendChild(nameEl);

				const chips = document.createElement('div');
				chips.className = 'ns-card-chips';
				if (!loaded) {
					// show nothing until the user opens this namespace
				} else {
					if (workloads) { const c = document.createElement('span'); c.className = 'ns-card-chip'; c.textContent = workloads + ' workload' + (workloads !== 1 ? 's' : ''); chips.appendChild(c); }
					if (pods)      { const c = document.createElement('span'); c.className = 'ns-card-chip'; c.textContent = pods + ' pod' + (pods !== 1 ? 's' : '');      chips.appendChild(c); }
					if (resources) { const c = document.createElement('span'); c.className = 'ns-card-chip'; c.textContent = resources + ' resource' + (resources !== 1 ? 's' : ''); chips.appendChild(c); }
					if (!workloads && !pods && !resources) {
						const c = document.createElement('span'); c.className = 'ns-card-chip'; c.textContent = 'empty'; chips.appendChild(c);
					}
				}
				card.appendChild(chips);

				const openNS = () => navigateToNS(ns.name);
				card.onclick = openNS;
				card.onkeydown = (ev) => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); openNS(); } };
				grid.appendChild(card);
				shown++;
			}

			if (shown === 0) {
				setTreeState('empty', q ? 'No namespaces match "' + q + '".' : 'No namespaces found in this capture.');
				visibleNodes = [];
				activeNodeIdx = -1;
				return;
			}
			el.tree.appendChild(grid);
			visibleNodes = [];
			activeNodeIdx = -1;
		}

		function navigateToNS(nsName) {
			currentNS = nsName;
			render();
		}

		function navigateBack() {
			currentNS = null;
			render();
		}

		// ── ns drill-down ─────────────────────────────────────────────────────────
		function renderNsDrilldown() {
			const nsLabel = currentNS === ':cluster' ? 'Cluster-scoped' : currentNS;
			el.treeHeadLabel.textContent = nsLabel;
			el.crumbs.textContent = 'Cluster / ' + nsLabel;

			// Build the back button once; we re-append it whenever a state
			// transition (loading / empty) clears the tree, so the user can
			// always return to the namespace list — including when the
			// drill-down has nothing to show.
			const back = document.createElement('button');
			back.className = 'ns-back';
			back.type = 'button';
			back.innerHTML = '&#8592; All namespaces';
			back.onclick = navigateBack;
			el.tree.appendChild(back);

			// Local helper: setTreeState wipes el.tree, so wrap it to preserve
			// the back button in loading/empty states.
			const setStateWithBack = (type, message) => {
				setTreeState(type, message);
				el.tree.insertBefore(back, el.tree.firstChild);
			};

			const detail = nsCache[currentNS];
			if (!detail || detail._loading) {
				loadNsDetail(currentNS);
				setStateWithBack('loading', 'Loading ' + nsLabel + '…');
				visibleNodes = [];
				activeNodeIdx = -1;
				return;
			}

			const q = el.search.value.trim().toLowerCase();
			const desiredKey = selected && selected.node ? nodeIdentity(selected.node) : '';
			const drilldown = document.createElement('div');
			drilldown.className = 'ns-drilldown';

			const workloads = detail.workloads || [];
			const pods = detail.pods || [];
			const resources = detail.resources || [];
			let renderedNodes = 0;

			// helper: build a section for a group of nodes
			function appendSection(title, items) {
				if (items.length === 0) return;
				const sec = document.createElement('div');
				sec.className = 'ns-section';
				const hdr = document.createElement('div');
				hdr.className = 'ns-section-header';
				hdr.textContent = title + ' (' + items.length + ')';
				sec.appendChild(hdr);
				for (const el of items) {
					sec.appendChild(el);
					renderedNodes++;
				}
				drilldown.appendChild(sec);
			}

			// workloads (with nested pods)
			const workloadNodes = [];
			for (const w of workloads) {
				const visible = kindEnabled(w.kind) && nodeMatches(w, q, currentNS === ':cluster' ? '' : currentNS);
				if (visible) workloadNodes.push(mkNode(w, ['Cluster', nsLabel, w.kind, w.name], 0, ''));
				for (const p of (w.pods || [])) {
					if (!kindEnabled(p.kind) || !nodeMatches(p, q, currentNS === ':cluster' ? '' : currentNS)) continue;
					const podCrumbs = visible
						? ['Cluster', nsLabel, w.kind, w.name, 'Pod', p.name]
						: ['Cluster', nsLabel, 'Pod', p.name];
					workloadNodes.push(mkNode(p, podCrumbs, visible ? 1 : 0, ''));
					for (const c of (p.containers || [])) {
						const fake = { kind:'Container', name:c.name, status:'', age:'', labels:{}, list_path:p.list_path };
						if (!kindEnabled('Container') || !nodeMatches(fake, q, currentNS === ':cluster' ? '' : currentNS)) continue;
						const depth = visible ? 2 : 1;
						const cc = visible ? ['Cluster', nsLabel, w.kind, w.name, 'Pod', p.name, 'Container', c.name] : ['Cluster', nsLabel, 'Pod', p.name, 'Container', c.name];
						workloadNodes.push(mkNode(fake, cc, depth, ''));
					}
				}
			}
			appendSection('Workloads', workloadNodes);

			// standalone pods
			const podNodes = [];
			for (const p of pods) {
				if (!kindEnabled(p.kind) || !nodeMatches(p, q, currentNS === ':cluster' ? '' : currentNS)) continue;
				podNodes.push(mkNode(p, ['Cluster', nsLabel, 'Pod', p.name], 0, ''));
				for (const c of (p.containers || [])) {
					const fake = { kind:'Container', name:c.name, status:'', age:'', labels:{}, list_path:p.list_path };
					if (!kindEnabled('Container') || !nodeMatches(fake, q, currentNS === ':cluster' ? '' : currentNS)) continue;
					podNodes.push(mkNode(fake, ['Cluster', nsLabel, 'Pod', p.name, 'Container', c.name], 1, ''));
				}
			}
			appendSection('Pods', podNodes);

			// remaining resources grouped by kind
			const byKind = {};
			for (const r of resources) {
				if (!kindEnabled(r.kind) || !nodeMatches(r, q, currentNS === ':cluster' ? '' : currentNS)) continue;
				(byKind[r.kind] = byKind[r.kind] || []).push(mkNode(r, ['Cluster', nsLabel, r.kind, r.name], 0, ''));
			}
			for (const kind of Object.keys(byKind).sort()) {
				appendSection(kind, byKind[kind]);
			}

			if (renderedNodes === 0) {
				const msg = q ? 'No resources match "' + q + '" in ' + nsLabel + '.' : nsLabel + ' has no resources at this snapshot.';
				setStateWithBack('empty', msg);
				visibleNodes = [];
				activeNodeIdx = -1;
				return;
			}

			el.tree.appendChild(drilldown);
			visibleNodes = Array.from(el.tree.querySelectorAll('.node'));
			if (visibleNodes.length === 0) { activeNodeIdx = -1; return; }
			if (activeNodeIdx < 0 || activeNodeIdx >= visibleNodes.length) activeNodeIdx = 0;
			if (restoreSelectionByKey(desiredKey)) return;
			selectNodeAt(activeNodeIdx, false);
		}

    function renderToggles(kinds) {
			if (!activeKindsInitialized) {
				activeKinds = new Set(kinds);
				activeKindsInitialized = true;
			} else {
				const allowed = new Set(kinds);
				// Remove stale kinds; add newly-seen kinds as enabled by default.
				activeKinds = new Set(Array.from(activeKinds).filter((k) => allowed.has(k)));
				for (const k of kinds) {
					if (!activeKinds.has(k)) activeKinds.add(k);
				}
			}
      el.toggles.innerHTML = '';
      for (const kind of kinds) {
        const row = document.createElement('label');
        row.className = 'muted';
        const cb = document.createElement('input');
        cb.type = 'checkbox';
				cb.dataset.kind = kind;
				cb.checked = activeKinds.has(kind);
        cb.onchange = () => {
          if (cb.checked) activeKinds.add(kind); else activeKinds.delete(kind);
				persistKinds();
          render();
        };
        row.appendChild(cb);
        row.appendChild(document.createTextNode(' ' + kind));
        el.toggles.appendChild(row);
      }
    }

		function setAllKinds(enabled) {
			const inputs = el.toggles.querySelectorAll('input[type="checkbox"]');
			for (const input of inputs) input.checked = enabled;
			activeKindsInitialized = true;
			if (enabled) {
				const kinds = Array.from(inputs).map((input) => input.dataset.kind || '').filter(Boolean);
				activeKinds = new Set(kinds);
			} else {
				activeKinds = new Set();
			}
			persistKinds();
			render();
		}

		async function loadTimestamps() {
			const q = withAtQuery(new URLSearchParams());
			const url = q.toString() ? ('/api/ui/timestamps?' + q.toString()) : '/api/ui/timestamps';
			const res = await fetch(url);
			const data = await res.json();
			if (!res.ok) {
				const msg = (data && data.error) ? data.error : ('request failed with status ' + res.status);
				throw new Error(msg);
			}
			renderSnapshotSelector(data.timestamps || [], data.default_at || '', data.total_count || 0, !!data.sampled);
			syncAtInURL();
		}

		async function loadTransitions() {
			try {
				const res = await fetch('/api/ui/transitions');
				if (res.ok) {
					allTransitions = await res.json();
				}
			} catch (_) {
				allTransitions = [];
			}
		}

		function renderTimelineMarkers() {
			el.markerLayer.innerHTML = '';
			if (!allTransitions.length || !timelinePoints.length) return;
			const first = new Date(timelinePoints[0]).getTime();
			const last = new Date(timelinePoints[timelinePoints.length - 1]).getTime();
			const span = last - first;
			if (span <= 0) return;
			for (const t of allTransitions) {
				const ms = new Date(t.time).getTime();
				if (ms < first || ms > last) continue;
				const pct = ((ms - first) / span) * 100;
				const dot = document.createElement('div');
				dot.className = 'marker-dot ev-' + t.event_type.toLowerCase();
				dot.style.left = pct + '%';
				dot.title = t.time + ' ' + t.event_type + ' ' + t.resource + '/' + (t.namespace ? t.namespace + '/' : '') + t.name;
				dot.onclick = (e) => {
					e.stopPropagation();
					seekToTime(t.time);
				};
				el.markerLayer.appendChild(dot);
			}
		}

		function seekToTime(ts) {
			if (!timelinePoints.length) return;
			let best = 0;
			for (let i = 0; i < timelinePoints.length; i++) {
				if (timelinePoints[i] <= ts) best = i;
			}
			el.timelineScrub.value = String(best);
			applyScrubberSelection(true);
		}

		async function loadObjectHistory(node) {
			el.historyPane.innerHTML = '<div class="history-empty">Loading history...</div>';
			try {
				const q = new URLSearchParams({name: node.name});
				const ns = namespaceFromPath(node.list_path);
				if (ns) q.set('namespace', ns);
				if (node.kind) {
					const res2kind = {Pod:'pods',Deployment:'deployments',StatefulSet:'statefulsets',DaemonSet:'daemonsets',Job:'jobs',ReplicaSet:'replicasets',Service:'services',Node:'nodes',ConfigMap:'configmaps',Secret:'secrets'};
					const r = Object.entries(res2kind).find(([k]) => k === node.kind);
					if (r) q.set('resource', r[1]);
				}
				const res = await fetch('/api/ui/object-history?' + q.toString());
				if (!res.ok) throw new Error('status ' + res.status);
				objectHistory = await res.json();
			} catch (err) {
				objectHistory = [];
				el.historyPane.innerHTML = '<div class="history-empty">Failed to load history: ' + (err.message || err) + '</div>';
				return;
			}
			renderHistoryList();
		}

		function renderHistoryList() {
			el.historyPane.innerHTML = '';
			if (!objectHistory.length) {
				el.historyPane.innerHTML = '<div class="history-empty">No transitions detected for this object.</div>';
				return;
			}
			const ul = document.createElement('ul');
			ul.className = 'history-list';
			for (let i = 0; i < objectHistory.length; i++) {
				const entry = objectHistory[i];
				const li = document.createElement('li');
				li.className = 'history-item';
				li.innerHTML = '<span class="history-event ev-' + entry.event_type.toLowerCase() + '">' + entry.event_type + '</span>' +
					'<span class="history-time">' + entry.time + '</span>';
				li.onclick = () => {
					for (const item of ul.children) item.classList.remove('active');
					li.classList.add('active');
					seekToTime(entry.time);
					showHistoryDiff(entry);
				};
				ul.appendChild(li);
			}
			el.historyPane.appendChild(ul);
		}

		function showHistoryDiff(entry) {
			selectedHistoryEntry = entry;
			if (activeTab === 'diff') renderDiffPane();
		}

		let selectedHistoryEntry = null;

		function renderDiffPane() {
			el.diffPane.innerHTML = '';
			if (!selectedHistoryEntry) {
				el.diffPane.innerHTML = '<div class="history-empty">Select an event in the History tab to view its diff.</div>';
				return;
			}
			const entry = selectedHistoryEntry;
			const header = document.createElement('div');
			header.style.cssText = 'margin-bottom:8px;font-size:13px;color:#cbd5e1;';
			header.innerHTML = '<span class="history-event ev-' + entry.event_type.toLowerCase() + '">' + entry.event_type + '</span> at ' + entry.time;
			el.diffPane.appendChild(header);

			if (entry.event_type === 'ADDED') {
				const block = document.createElement('pre');
				block.className = 'diff-block';
				block.innerHTML = colorizeJSON(entry.after, 'diff-line-add');
				el.diffPane.appendChild(block);
			} else if (entry.event_type === 'DELETED') {
				const block = document.createElement('pre');
				block.className = 'diff-block';
				block.innerHTML = colorizeJSON(entry.before, 'diff-line-del');
				el.diffPane.appendChild(block);
			} else if (entry.event_type === 'MODIFIED') {
				const beforeLines = prettyJSONLines(entry.before);
				const afterLines = prettyJSONLines(entry.after);
				const diffLines = simpleDiff(beforeLines, afterLines);
				const block = document.createElement('pre');
				block.className = 'diff-block';
				block.innerHTML = diffLines.map(renderDiffLine).join('\n');
				el.diffPane.appendChild(block);
			}
		}

		function prettyJSONLines(raw) {
			if (!raw) return [];
			try {
				return JSON.stringify(JSON.parse(typeof raw === 'string' ? raw : JSON.stringify(raw)), null, 2).split('\n');
			} catch (_) {
				return String(raw).split('\n');
			}
		}

		function colorizeJSON(raw, cls) {
			const lines = prettyJSONLines(raw);
			return lines.map((l) => '<span class="' + cls + '">' + escapeHTML(l) + '</span>').join('\n');
		}

		function escapeHTML(s) {
			return s.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
		}

		function simpleDiff(a, b) {
			const out = [];
			const max = Math.max(a.length, b.length);
			let ai = 0, bi = 0;
			while (ai < a.length || bi < b.length) {
				if (ai < a.length && bi < b.length && a[ai] === b[bi]) {
					out.push({type:'ctx', text:a[ai]});
					ai++; bi++;
				} else if (ai < a.length && (bi >= b.length || !b.includes(a[ai]))) {
					out.push({type:'del', text:a[ai]});
					ai++;
				} else if (bi < b.length && (ai >= a.length || !a.includes(b[bi]))) {
					out.push({type:'add', text:b[bi]});
					bi++;
				} else {
					out.push({type:'del', text:a[ai]});
					out.push({type:'add', text:b[bi]});
					ai++; bi++;
				}
			}
			return out;
		}

		function renderDiffLine(line) {
			const prefix = line.type === 'add' ? '+' : line.type === 'del' ? '-' : ' ';
			const cls = line.type === 'add' ? 'diff-line-add' : line.type === 'del' ? 'diff-line-del' : '';
			return '<span class="' + cls + '">' + escapeHTML(prefix + ' ' + line.text) + '</span>';
		}

		async function refreshTree(forceRefresh) {
			const previousSelectionKey = selected && selected.node ? nodeIdentity(selected.node) : '';
			if (!forceRefresh && normalizedAt() === lastLoadedAt) {
				return;
			}
			const token = ++refreshToken;
			setTreeState('loading', 'Loading captured resources...');
			setSnapshotLoading(true);
			const q = withAtQuery(new URLSearchParams());
			const url = q.toString() ? ('/api/ui/tree?' + q.toString()) : '/api/ui/tree';
			try {
				const res = await fetch(url);
				const data = await res.json();
				if (token !== refreshToken) {
					return;
				}
				if (!res.ok) {
					const msg = (data && data.error) ? data.error : ('request failed with status ' + res.status);
					throw new Error(msg);
				}
				treeData = data;
				nsCache = {};
				nsFetchQueue.length = 0;
				currentNS = null;
				lastLoadedAt = normalizedAt();
				updateMetaLine();
				renderToggles((treeData.resource_kinds || []).concat(['Container']));
				render();
				if (restoreSelectionByKey(previousSelectionKey)) {
					const nodeEl = visibleNodes[activeNodeIdx];
					if (nodeEl) {
						showDetail(nodeEl._node, nodeEl._crumbs || ['Cluster']);
						return;
					}
				}
				if ((activeTab === 'history' || activeTab === 'diff') && selectedHistoryEntry) {
					// Keep the selected transition context even when the object is
					// absent at this exact timeline position (e.g. deleted events).
					if (activeTab === 'diff') {
						renderDiffPane();
					}
					return;
				}
				selected = null;
				selectedHistoryEntry = null;
				el.crumbs.textContent = '';
				el.detailTitle.textContent = 'Select a resource';
				el.detailSummary.innerHTML = '';
				el.detailBody.textContent = 'No resource is available at the selected time.';
				el.historyPane.innerHTML = '<div class="history-empty">No resource is selected.</div>';
				el.diffPane.innerHTML = '<div class="history-empty">Select an event in the History tab to view its diff.</div>';
			} finally {
				if (token === refreshToken) {
					setSnapshotLoading(false);
					syncSnapshotButtons();
				}
			}
		}

		async function init() {
			setTreeState('loading', 'Loading captured resources...');
			try {
				loadPreferences();
				initAtFromURL();
				await loadTimestamps();
				await loadTransitions();
				await refreshTree(true);
				renderTimelineMarkers();
			} catch (err) {
				const msg = (err && err.message ? err.message : String(err));
				setTreeState('error', 'Failed to load capture tree: ' + msg);
				showToast('error', 'Capture tree failed to load: ' + msg);
				el.meta.textContent = 'capture unavailable';
			}
    }
    init();
  </script>
</body>
</html>`
