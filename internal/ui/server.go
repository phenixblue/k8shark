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
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
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
	tmpDir     string
	httpServer *http.Server
	done       chan struct{}
}

type explorerHandler struct {
	store   *server.CaptureStore
	at      time.Time
	verbose bool
}

type treeResponse struct {
	CapturedAt    time.Time       `json:"captured_at"`
	CapturedUntil time.Time       `json:"captured_until"`
	Namespaces    []namespaceNode `json:"namespaces"`
	ClusterScoped []resourceNode  `json:"cluster_scoped"`
	ResourceKinds []string        `json:"resource_kinds"`
}

type namespaceNode struct {
	Name      string         `json:"name"`
	Workloads []workloadNode `json:"workloads"`
	Pods      []podNode      `json:"pods"`
	Resources []resourceNode `json:"resources"`
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

type listItem struct {
	path string
	item map[string]any
}

func Open(opts OpenOptions) (*Server, error) {
	tmpDir, err := os.MkdirTemp("", "k8shark-ui-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}

	if err := archive.Open(opts.ArchivePath, tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("extracting archive: %w", err)
	}

	store, err := server.LoadStore(tmpDir)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("loading capture: %w", err)
	}

	at, err := parseReplayAt(store.Metadata.CapturedAt, store.Metadata.CapturedUntil, opts.At)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}

	port := opts.Port
	if port == "" || port == "0" {
		port = "0"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("listening: %w", err)
	}

	h := &explorerHandler{store: store, at: at, verbose: opts.Verbose}
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.serveIndex)
	mux.HandleFunc("/api/ui/tree", h.serveTree)
	mux.HandleFunc("/api/ui/detail", h.serveDetail)

	httpSrv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = httpSrv.Serve(ln)
	}()

	addr := fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)
	return &Server{address: addr, tmpDir: tmpDir, httpServer: httpSrv, done: done}, nil
}

func (s *Server) Address() string { return s.address }

func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
	<-s.done
	_ = os.RemoveAll(s.tmpDir)
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
	_ = os.RemoveAll(s.tmpDir)
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
	resp, err := h.buildTree()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *explorerHandler) serveDetail(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing path query parameter"})
		return
	}

	name := r.URL.Query().Get("name")
	var (
		body []byte
		code = http.StatusOK
		err  error
	)
	if name != "" {
		body, code, err = h.findResourceBody(path, name)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if code == http.StatusNotFound {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "item not found in response"})
			return
		}
	} else {
		body, code, err = h.store.Latest(path, h.at)
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
		prettyJSON = []byte(out.String())
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
	nsMap := map[string]*namespaceNode{}
	clusterScoped := make([]resourceNode, 0)
	kindSet := map[string]bool{}
	workloadKinds := map[string]bool{
		"deployments":  true,
		"statefulsets": true,
		"daemonsets":   true,
		"jobs":         true,
		"replicasets":  true,
	}
	// Map of (namespace, resource) -> all candidate index paths
	byNSRes := map[string]map[string][]string{}
	for path := range h.store.Index {
		group, version, resource, ns, _ := parseAPIPath(baseAPIPath(path))
		if resource == "" {
			continue
		}
		if byNSRes[ns] == nil {
			byNSRes[ns] = map[string][]string{}
		}
		byNSRes[ns][resource] = append(byNSRes[ns][resource], path)
		_ = group
		_ = version
	}
	for ns, resMap := range byNSRes {
		for resource, candidates := range resMap {
			items, ok := h.loadResourceItems(candidates)
			if !ok {
				continue
			}
			if ns == "" {
				for _, entry := range items {
					node := toResourceNode(resource, entry.path, entry.item)
					clusterScoped = append(clusterScoped, node)
					kindSet[node.Kind] = true
				}
				continue
			}
			node, ok := nsMap[ns]
			if !ok {
				node = &namespaceNode{
					Name:      ns,
					Workloads: []workloadNode{},
					Pods:      []podNode{},
					Resources: []resourceNode{},
				}
				nsMap[ns] = node
			}
			if workloadKinds[resource] {
				for _, entry := range items {
					w := workloadNode{
						Kind:     firstNonEmpty(asString(entry.item["kind"]), kindFromResource(resource)),
						Name:     getMetaName(entry.item),
						Status:   summarizeStatus(entry.item),
						Age:      summarizeAge(entry.item),
						ListPath: entry.path,
						Labels:   getMetaLabels(entry.item),
					}
					node.Workloads = append(node.Workloads, w)
					kindSet[w.Kind] = true
				}
				continue
			}
			if resource == "pods" {
				for _, entry := range items {
					p := toPodNode(entry.path, entry.item)
					node.Pods = append(node.Pods, p)
					kindSet[p.Kind] = true
				}
				continue
			}
			for _, entry := range items {
				rn := toResourceNode(resource, entry.path, entry.item)
				node.Resources = append(node.Resources, rn)
				kindSet[rn.Kind] = true
			}
		}
	}
	for ns := range nsMap {
		attachPodsToWorkloads(nsMap[ns])
	}
	namespaces := make([]namespaceNode, 0, len(nsMap))
	for _, ns := range nsMap {
		sort.Slice(ns.Workloads, func(i, j int) bool {
			if ns.Workloads[i].Kind == ns.Workloads[j].Kind {
				return ns.Workloads[i].Name < ns.Workloads[j].Name
			}
			return ns.Workloads[i].Kind < ns.Workloads[j].Kind
		})
		sort.Slice(ns.Pods, func(i, j int) bool { return ns.Pods[i].Name < ns.Pods[j].Name })
		sort.Slice(ns.Resources, func(i, j int) bool {
			if ns.Resources[i].Kind == ns.Resources[j].Kind {
				return ns.Resources[i].Name < ns.Resources[j].Name
			}
			return ns.Resources[i].Kind < ns.Resources[j].Kind
		})
		namespaces = append(namespaces, *ns)
	}
	sort.Slice(namespaces, func(i, j int) bool { return namespaces[i].Name < namespaces[j].Name })
	sort.Slice(clusterScoped, func(i, j int) bool {
		if clusterScoped[i].Kind == clusterScoped[j].Kind {
			return clusterScoped[i].Name < clusterScoped[j].Name
		}
		return clusterScoped[i].Kind < clusterScoped[j].Kind
	})
	kinds := make([]string, 0, len(kindSet))
	for k := range kindSet {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return &treeResponse{
		CapturedAt:    h.store.Metadata.CapturedAt,
		CapturedUntil: h.store.Metadata.CapturedUntil,
		Namespaces:    namespaces,
		ClusterScoped: clusterScoped,
		ResourceKinds: kinds,
	}, nil
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

func attachPodsToWorkloads(ns *namespaceNode) {
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

func buildPathCandidates(index map[string]*capture.IndexEntry) map[string][]string {
	pathsByBase := make(map[string][]string)
	for path := range index {
		base := baseAPIPath(path)
		if _, _, resource, _, _ := parseAPIPath(base); resource == "" {
			continue
		}
		pathsByBase[base] = append(pathsByBase[base], path)
	}
	for base := range pathsByBase {
		sort.Slice(pathsByBase[base], func(i, j int) bool {
			pi := pathPriority(pathsByBase[base][i])
			pj := pathPriority(pathsByBase[base][j])
			if pi == pj {
				return pathsByBase[base][i] < pathsByBase[base][j]
			}
			return pi < pj
		})
	}
	return pathsByBase
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (h *explorerHandler) loadResourceItems(candidates []string) ([]listItem, bool) {
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
		bodies, ok := h.responseBodies(candidate)
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

func (h *explorerHandler) findResourceBody(path, name string) ([]byte, int, error) {
	bodies, ok := h.responseBodies(path)
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

func (h *explorerHandler) responseBodies(path string) ([][]byte, bool) {
	entry, ok := h.store.Index[path]
	if !ok || len(entry.RecordIDs) == 0 {
		return nil, false
	}

	if !h.at.IsZero() {
		index := -1
		for i, t := range entry.Times {
			if !t.After(h.at) {
				index = i
			}
		}
		if index < 0 {
			return nil, false
		}
		body, ok := h.readRecordBody(entry.RecordIDs[index])
		if !ok {
			return nil, false
		}
		return [][]byte{body}, true
	}

	bodies := make([][]byte, 0, len(entry.RecordIDs))
	for _, id := range entry.RecordIDs {
		body, ok := h.readRecordBody(id)
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

func (h *explorerHandler) readRecordBody(id string) ([]byte, bool) {
	data, err := os.ReadFile(filepath.Join(h.store.Dir, "k8shark-capture", "records", id+".json"))
	if err != nil {
		return nil, false
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, false
	}
	if rec.ResponseCode != http.StatusOK {
		return nil, false
	}
	return rec.ResponseBody, true
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
    header { padding:14px 18px; border-bottom:1px solid var(--line); display:flex; justify-content:space-between; align-items:center; }
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
  </style>
</head>
<body>
  <header>
    <div><strong>kshrk capture explorer</strong></div>
    <div id="meta" class="muted"></div>
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
				<span class="toggle-head-title">Tree</span>
				<span class="tree-actions">
					<button id="expandAll" type="button">Expand All</button>
					<button id="collapseAll" type="button">Collapse All</button>
				</span>
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
				</span>
				<span class="detail-actions">
					<button id="copyDetail" class="copy-btn" type="button">Copy JSON</button>
					<button id="downloadDetail" class="copy-btn" type="button">Download JSON</button>
				</span>
      </div>
      <pre id="detailBody">Click a node in the tree to inspect details.</pre>
    </section>
  </div>
	<div id="toasts" class="toast-stack" aria-live="polite"></div>
  <script>
    let treeData = null;
    let activeKinds = new Set();
    let selected = null;
    let activeTab = 'json';
	let visibleNodes = [];
	let activeNodeIdx = -1;
	const namespacePageSize = 120;
	let namespaceVisibleLimit = {};
	const storageKindsKey = 'kshrk.ui.activeKinds.v1';
	const storageExpandedKey = 'kshrk.ui.expandedSections.v1';
	let expandedSections = {};

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
			expandAll: document.getElementById('expandAll'),
			collapseAll: document.getElementById('collapseAll'),
			toasts: document.getElementById('toasts')
    };

    el.tabJson.onclick = () => setTab('json');
    el.tabYaml.onclick = () => setTab('yaml');
	el.copyDetail.onclick = () => copyActiveDetail();
	el.downloadDetail.onclick = () => downloadActiveDetail();
    el.search.oninput = render;
		el.toggleAll.onclick = () => setAllKinds(true);
		el.toggleNone.onclick = () => setAllKinds(false);
	el.expandAll.onclick = () => setAllTreeDetails(true);
	el.collapseAll.onclick = () => setAllTreeDetails(false);
	document.addEventListener('keydown', onTreeKeyDown);

    function setTab(tab) {
      activeTab = tab;
      el.tabJson.classList.toggle('active', tab === 'json');
      el.tabYaml.classList.toggle('active', tab === 'yaml');
			el.copyDetail.textContent = tab === 'yaml' ? 'Copy YAML' : 'Copy JSON';
			el.downloadDetail.textContent = tab === 'yaml' ? 'Download YAML' : 'Download JSON';
      if (selected && selected.detail) {
        el.detailBody.textContent = selected.detail[activeTab] || '';
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

		function namespaceLimit(name) {
			if (!namespaceVisibleLimit[name]) namespaceVisibleLimit[name] = namespacePageSize;
			return namespaceVisibleLimit[name];
		}

		function showMoreNamespace(name) {
			namespaceVisibleLimit[name] = namespaceLimit(name) + namespacePageSize;
			render();
		}

		function showToast(type, message, timeoutMs) {
			const toast = document.createElement('div');
			toast.className = 'toast ' + type;
			toast.textContent = message;
			el.toasts.appendChild(toast);
			const ttl = timeoutMs || (type === 'error' ? 7000 : 3000);
			window.setTimeout(() => toast.remove(), ttl);
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
					if (Array.isArray(parsed)) activeKinds = new Set(parsed);
				}
			} catch (_) {}
			try {
				const expandedRaw = window.localStorage.getItem(storageExpandedKey);
				if (expandedRaw) {
					const parsed = JSON.parse(expandedRaw);
					if (parsed && typeof parsed === 'object') expandedSections = parsed;
				}
			} catch (_) {}
		}

		function persistKinds() {
			try {
				window.localStorage.setItem(storageKindsKey, JSON.stringify(Array.from(activeKinds)));
			} catch (_) {}
		}

		function persistExpandedSections() {
			try {
				window.localStorage.setItem(storageExpandedKey, JSON.stringify(expandedSections));
			} catch (_) {}
		}

		function sectionOpen(sectionKey, fallbackOpen) {
			if (Object.prototype.hasOwnProperty.call(expandedSections, sectionKey)) {
				return !!expandedSections[sectionKey];
			}
			return fallbackOpen;
		}

		function bindSectionState(details, sectionKey) {
			details.onToggle = null;
			details.addEventListener('toggle', () => {
				expandedSections[sectionKey] = details.open;
				persistExpandedSections();
			});
		}

    function statusClass(status) {
      const s = (status || '').toLowerCase();
      if (s.includes('running') || s.includes('ready') || s.includes('active')) return 'ok';
      if (s.includes('pending') || s.includes('containercreating')) return 'warn';
      if (s.includes('failed') || s.includes('error') || s.includes('crash')) return 'bad';
      return '';
    }

    function nodeMatches(node, q) {
      if (!q) return true;
      const hay = [node.name, ...(Object.entries(node.labels || {}).map(([k,v]) => k + '=' + v))].join(' ').toLowerCase();
      return hay.includes(q);
    }

    function kindEnabled(kind) {
      return activeKinds.size === 0 || activeKinds.has(kind);
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

		function onTreeKeyDown(ev) {
			if (visibleNodes.length === 0) return;
			const tag = (document.activeElement && document.activeElement.tagName) || '';
			if (tag === 'INPUT' || tag === 'TEXTAREA' || (document.activeElement && document.activeElement.isContentEditable)) return;
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
      selected = { node, crumbs };
      el.crumbs.textContent = crumbs.join(' / ');
      el.detailTitle.textContent = node.kind + ' ' + node.name;
			el.detailBody.textContent = 'Loading detail...';
			try {
				const q = new URLSearchParams({ path: node.list_path, name: node.name });
				const res = await fetch('/api/ui/detail?' + q.toString());
				const data = await res.json();
				if (!res.ok) {
					const msg = (data && data.error) ? data.error : ('request failed with status ' + res.status);
					throw new Error(msg);
				}
				selected.detail = data;
				updateDetailSummary(data, node);
				el.detailBody.textContent = data[activeTab] || JSON.stringify(data, null, 2);
			} catch (err) {
				selected.detail = null;
				el.detailSummary.innerHTML = '';
				const msg = (err && err.message ? err.message : String(err));
				el.detailBody.textContent = 'Failed to load detail: ' + msg;
				showToast('error', 'Detail request failed: ' + msg);
			}
    }

    function render() {
			if (!treeData) {
				setTreeState('loading', 'Loading captured resources...');
				return;
			}
      el.tree.innerHTML = '';
      const q = el.search.value.trim().toLowerCase();
			let renderedNodes = 0;

      const cs = document.createElement('details');
			cs.open = sectionOpen('cluster-scoped', true);
			cs.innerHTML = '<summary>Cluster-scoped resources <span class="muted">(' + treeData.cluster_scoped.length + ')</span></summary>';
			bindSectionState(cs, 'cluster-scoped');
      for (const node of treeData.cluster_scoped) {
        if (!kindEnabled(node.kind) || !nodeMatches(node, q)) continue;
				cs.appendChild(mkNode(node, ['Cluster', node.kind, node.name], 0, ''));
				renderedNodes++;
      }
      el.tree.appendChild(cs);

      for (const ns of treeData.namespaces) {
        const ds = document.createElement('details');
				ds.open = sectionOpen('ns:' + ns.name, true);
				const nsCount = (ns.workloads || []).length + (ns.pods || []).length + (ns.resources || []).length;
				ds.innerHTML = '<summary>Namespace: <strong>' + ns.name + '</strong> <span class="muted">(' + nsCount + ')</span></summary>';
				bindSectionState(ds, 'ns:' + ns.name);

				const nsNodes = [];

			for (const w of (ns.workloads || [])) {
          if (!kindEnabled(w.kind) || !nodeMatches(w, q)) continue;
					nsNodes.push(mkNode(w, ['Cluster', ns.name, w.kind, w.name], 0, ''));
          for (const p of (w.pods || [])) {
            if (!kindEnabled(p.kind) || !nodeMatches(p, q)) continue;
						nsNodes.push(mkNode(p, ['Cluster', ns.name, w.kind, w.name, 'Pod', p.name], 1, ''));
            for (const c of (p.containers || [])) {
              const fake = {kind:'Container',name:c.name,status:'',age:'',labels:{},list_path:p.list_path};
              if (!kindEnabled('Container') || !nodeMatches(fake, q)) continue;
							nsNodes.push(mkNode(fake, ['Cluster', ns.name, w.kind, w.name, 'Pod', p.name, 'Container', c.name], 2, ''));
            }
          }
        }

			for (const p of (ns.pods || [])) {
          if (!kindEnabled(p.kind) || !nodeMatches(p, q)) continue;
					nsNodes.push(mkNode(p, ['Cluster', ns.name, 'Pod', p.name], 0, ''));
          for (const c of (p.containers || [])) {
            const fake = {kind:'Container',name:c.name,status:'',age:'',labels:{},list_path:p.list_path};
            if (!kindEnabled('Container') || !nodeMatches(fake, q)) continue;
						nsNodes.push(mkNode(fake, ['Cluster', ns.name, 'Pod', p.name, 'Container', c.name], 1, ''));
          }
        }

			for (const r of (ns.resources || [])) {
          if (!kindEnabled(r.kind) || !nodeMatches(r, q)) continue;
					nsNodes.push(mkNode(r, ['Cluster', ns.name, r.kind, r.name], 0, ''));
        }

				const limit = q ? nsNodes.length : namespaceLimit(ns.name);
				const shown = nsNodes.slice(0, limit);
				for (const nodeEl of shown) {
					ds.appendChild(nodeEl);
					renderedNodes++;
				}
				if (!q && nsNodes.length > shown.length) {
					const pager = document.createElement('div');
					pager.className = 'ns-pager';
					const hidden = nsNodes.length - shown.length;
					const meta = document.createElement('span');
					meta.className = 'muted';
					meta.textContent = hidden + ' more hidden';
					const btn = document.createElement('button');
					btn.type = 'button';
					btn.textContent = 'Show more';
					btn.onclick = () => showMoreNamespace(ns.name);
					pager.appendChild(meta);
					pager.appendChild(btn);
					ds.appendChild(pager);
				}
        el.tree.appendChild(ds);
      }

			if (renderedNodes === 0) {
				const msg = q
					? 'No resources match the current search/filter selection.'
					: 'No resources were found in this capture at the selected time.';
				setTreeState('empty', msg);
				visibleNodes = [];
				activeNodeIdx = -1;
				return;
			}

			visibleNodes = Array.from(el.tree.querySelectorAll('.node'));
			if (visibleNodes.length === 0) {
				activeNodeIdx = -1;
				return;
			}
			if (activeNodeIdx < 0 || activeNodeIdx >= visibleNodes.length) {
				activeNodeIdx = 0;
			}
			selectNodeAt(activeNodeIdx, false);
    }

    function renderToggles(kinds) {
			if (activeKinds.size === 0) {
				activeKinds = new Set(kinds);
			} else {
				const allowed = new Set(kinds);
				activeKinds = new Set(Array.from(activeKinds).filter((k) => allowed.has(k)));
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
			if (enabled) {
				const kinds = Array.from(inputs).map((input) => input.dataset.kind || '').filter(Boolean);
				activeKinds = new Set(kinds);
			} else {
				activeKinds = new Set();
			}
			persistKinds();
			render();
		}

		function setAllTreeDetails(open) {
			for (const details of el.tree.querySelectorAll('details')) {
				details.open = open;
				const summary = details.querySelector('summary');
				if (summary && summary.textContent.startsWith('Namespace:')) {
					const strong = summary.querySelector('strong');
					if (strong) expandedSections['ns:' + strong.textContent] = open;
				} else {
					expandedSections['cluster-scoped'] = open;
				}
			}
			persistExpandedSections();
		}

    async function init() {
			setTreeState('loading', 'Loading captured resources...');
			try {
				loadPreferences();
				const res = await fetch('/api/ui/tree');
				const data = await res.json();
				if (!res.ok) {
					const msg = (data && data.error) ? data.error : ('request failed with status ' + res.status);
					throw new Error(msg);
				}
				treeData = data;
				el.meta.textContent = 'capture ' + new Date(treeData.captured_at).toISOString() + ' to ' + new Date(treeData.captured_until).toISOString();
				renderToggles((treeData.resource_kinds || []).concat(['Container']));
				render();
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
