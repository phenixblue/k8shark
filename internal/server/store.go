package server

import (
	"container/list"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// CaptureStore holds the in-memory index and provides record lookups against
// a ZIP+Zstd archive opened without extraction to disk.
type CaptureStore struct {
	ar           *archive.Archive
	Metadata     capture.CaptureMetadata
	Index        capture.Index
	WatchIndex   capture.WatchIndex
	resourceInfo map[string]*ResourceInfo

	// Record LRU cache (bounded by recordCacheMaxBytes total body bytes).
	recordCacheMu    sync.Mutex
	recordCacheMap   map[recordKey]*list.Element
	recordCacheList  *list.List
	recordCacheBytes int64

	// Response cache: (path, at) → marshalled body + code.
	// Entries are valid for responseCacheTTL, bounded by responseCacheMaxBytes.
	responseCacheMu    sync.RWMutex
	responseCacheMap   map[responseCacheKey]*responseCacheEntry
	responseCacheBytes int64
}

// recordCacheMaxBytes caps the total in-memory size of cached record bodies.
// Large cluster captures can have response bodies of hundreds of KB each;
// a count-based cap of 2048 was the primary driver of the v0.2.0 memory regression.
const recordCacheMaxBytes = 128 * 1024 * 1024 // 128 MiB

// responseCacheMaxBytes caps the total in-memory size of cached response bodies.
const responseCacheMaxBytes = 32 * 1024 * 1024 // 32 MiB

type recordKey struct {
	apiPath string
	seq     int
}

type responseCacheKey struct {
	path string
	at   time.Time
}

type responseCacheEntry struct {
	body    []byte
	code    int
	created time.Time
}

const responseCacheTTL = 10 * time.Second

type recordCacheEntry struct {
	key  recordKey
	rec  capture.Record
	size int64 // len(rec.ResponseBody)
}

// ResourceInfo describes a single captured resource type.
type ResourceInfo struct {
	Group        string
	Version      string
	Resource     string
	Kind         string
	Namespaced   bool
	ShortNames   []string
	SingularName string
}

// LoadStore reads metadata and index from the archive and returns a ready
// CaptureStore. The archive must remain open for the lifetime of the store;
// call ar.Close() when done (the server's Shutdown does this).
func LoadStore(ar *archive.Archive) (*CaptureStore, error) {
	var meta capture.CaptureMetadata
	if err := ar.ReadMetadata(&meta); err != nil {
		return nil, fmt.Errorf("reading metadata: %w", err)
	}

	var idx capture.Index
	if err := ar.ReadIndex(&idx); err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}

	s := &CaptureStore{
		ar:               ar,
		Metadata:         meta,
		Index:            idx,
		WatchIndex:       make(capture.WatchIndex),
		resourceInfo:     make(map[string]*ResourceInfo),
		recordCacheMap:   make(map[recordKey]*list.Element),
		recordCacheList:  list.New(),
		responseCacheMap: make(map[responseCacheKey]*responseCacheEntry),
	}

	// Load watch index if present.
	var wi capture.WatchIndex
	if found, err := ar.ReadWatchIndex(&wi); err == nil && found && wi != nil {
		s.WatchIndex = wi
	}

	// Derive ResourceInfo from index keys synchronously (fast — no I/O).
	s.buildResourceInfo()

	// Enrich ResourceInfo from discovery records asynchronously.
	go s.enrichResourceInfoFromDiscovery()

	return s, nil
}

// buildResourceInfo derives ResourceInfo for each distinct resource type in
// the index. It does not read any record data.
func (s *CaptureStore) buildResourceInfo() {
	for path := range s.Index {
		if strings.Contains(path, "?") {
			continue
		}
		g, v, r, ns := parseAPIPath(path)
		if r == "" {
			continue
		}
		key := g + "/" + v + "/" + r
		if existing, ok := s.resourceInfo[key]; ok {
			if ns != "" {
				existing.Namespaced = true
			}
			continue
		}
		s.resourceInfo[key] = &ResourceInfo{
			Group:      g,
			Version:    v,
			Resource:   r,
			Kind:       resourceToKind(r),
			Namespaced: ns != "",
		}
	}
}

// enrichResourceInfoFromDiscovery reads captured APIResourceList bodies from
// the archive and back-fills Kind, ShortNames, SingularName into resourceInfo.
// Runs in a background goroutine; store is usable before this completes.
func (s *CaptureStore) enrichResourceInfoFromDiscovery() {
	type apiResourceEntry struct {
		Name         string   `json:"name"`
		SingularName string   `json:"singularName"`
		Kind         string   `json:"kind"`
		ShortNames   []string `json:"shortNames"`
	}
	type apiResourceList struct {
		Kind      string             `json:"kind"`
		Resources []apiResourceEntry `json:"resources"`
	}

	for path, entry := range s.Index {
		if strings.Contains(path, "?") {
			continue
		}
		var g, v string
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		switch {
		case path == "/api/v1":
			g, v = "", "v1"
		case len(parts) == 3 && parts[0] == "apis":
			g, v = parts[1], parts[2]
		default:
			continue
		}
		if len(entry.Seqs) == 0 {
			continue
		}
		// Use the latest record.
		var body []byte
		for i := len(entry.Seqs) - 1; i >= 0; i-- {
			rec, err := s.readRecord(path, entry.Seqs[i])
			if err != nil || rec.ResponseCode != 200 {
				continue
			}
			body = rec.ResponseBody
			break
		}
		if body == nil {
			continue
		}
		var resList apiResourceList
		if err := json.Unmarshal(body, &resList); err != nil || resList.Kind != "APIResourceList" {
			continue
		}
		for _, res := range resList.Resources {
			if strings.Contains(res.Name, "/") {
				continue
			}
			key := g + "/" + v + "/" + res.Name
			ri, ok := s.resourceInfo[key]
			if !ok {
				continue
			}
			if res.Kind != "" {
				ri.Kind = res.Kind
			}
			if res.SingularName != "" {
				ri.SingularName = res.SingularName
			}
			if len(res.ShortNames) > 0 {
				ri.ShortNames = res.ShortNames
			}
		}
	}
}

// Resources returns all distinct ResourceInfo entries.
func (s *CaptureStore) Resources() []*ResourceInfo {
	out := make([]*ResourceInfo, 0, len(s.resourceInfo))
	for _, ri := range s.resourceInfo {
		out = append(out, ri)
	}
	return out
}

// Latest returns the ResponseBody of the most recent record for apiPath.
// If at is non-zero, it returns the latest record whose timestamp is <= at.
func (s *CaptureStore) Latest(apiPath string, at time.Time) ([]byte, int, error) {
	entry, ok := s.Index[apiPath]
	if !ok || len(entry.Seqs) == 0 {
		return nil, 404, nil
	}

	idx := len(entry.Seqs) - 1
	if !at.IsZero() && len(entry.Times) == len(entry.Seqs) {
		pos := sort.Search(len(entry.Times), func(i int) bool {
			return entry.Times[i].After(at)
		})
		if pos == 0 {
			return nil, 404, nil
		}
		idx = pos - 1
	}

	rec, err := s.readRecord(apiPath, entry.Seqs[idx])
	if err != nil {
		return nil, 500, err
	}
	return rec.ResponseBody, rec.ResponseCode, nil
}

// readRecord reads and parses a single capture.Record from the archive,
// using a bounded LRU cache to avoid re-reading hot records.
func (s *CaptureStore) readRecord(apiPath string, seq int) (capture.Record, error) {
	k := recordKey{apiPath, seq}

	s.recordCacheMu.Lock()
	if el, ok := s.recordCacheMap[k]; ok {
		s.recordCacheList.MoveToFront(el)
		rec := el.Value.(*recordCacheEntry).rec
		s.recordCacheMu.Unlock()
		return rec, nil
	}
	s.recordCacheMu.Unlock()

	data, err := s.ar.ReadRecord(apiPath, seq)
	if err != nil {
		return capture.Record{}, fmt.Errorf("reading record path=%s seq=%d: %w", apiPath, seq, err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return capture.Record{}, fmt.Errorf("parsing record path=%s seq=%d: %w", apiPath, seq, err)
	}

	entry := &recordCacheEntry{key: k, rec: rec, size: int64(len(rec.ResponseBody))}

	s.recordCacheMu.Lock()
	// Evict LRU entries until the new entry fits within the byte budget.
	for s.recordCacheBytes+entry.size > recordCacheMaxBytes {
		back := s.recordCacheList.Back()
		if back == nil {
			break
		}
		evicted := back.Value.(*recordCacheEntry)
		s.recordCacheList.Remove(back)
		delete(s.recordCacheMap, evicted.key)
		s.recordCacheBytes -= evicted.size
	}
	el := s.recordCacheList.PushFront(entry)
	s.recordCacheMap[k] = el
	s.recordCacheBytes += entry.size
	s.recordCacheMu.Unlock()

	return rec, nil
}

// ReadRecord reads a single record by apiPath and seq, exposed for callers
// outside this package that need raw record access.
func (s *CaptureStore) ReadRecord(apiPath string, seq int) (capture.Record, error) {
	return s.readRecord(apiPath, seq)
}

// SnapshotAt returns the body, HTTP code, and capture time of the most recent
// snapshot record for apiPath at or before at.
func (s *CaptureStore) SnapshotAt(apiPath string, at time.Time) ([]byte, int, time.Time, error) {
	entry, ok := s.Index[apiPath]
	if !ok || len(entry.Seqs) == 0 {
		return nil, 404, time.Time{}, nil
	}

	idx := len(entry.Seqs) - 1
	snapTime := entry.Times[idx]
	if !at.IsZero() && len(entry.Times) == len(entry.Seqs) {
		pos := sort.Search(len(entry.Times), func(i int) bool {
			return entry.Times[i].After(at)
		})
		if pos == 0 {
			return nil, 404, time.Time{}, nil
		}
		idx = pos - 1
		snapTime = entry.Times[idx]
	}

	rec, err := s.readRecord(apiPath, entry.Seqs[idx])
	if err != nil {
		return nil, 500, time.Time{}, err
	}
	return rec.ResponseBody, rec.ResponseCode, snapTime, nil
}

// objectKey returns the stable identity key for a Kubernetes object JSON blob.
func objectKey(raw json.RawMessage) string {
	var meta struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ""
	}
	if meta.Metadata.Namespace != "" {
		return meta.Metadata.Namespace + "/" + meta.Metadata.Name
	}
	return meta.Metadata.Name
}

// ReconstructAt returns the reconstructed list body for apiPath at time T.
// Watch events are applied on top of the snapshot in parallel to reduce latency.
// Falls back to Latest for paths without watch events.
func (s *CaptureStore) ReconstructAt(apiPath string, at time.Time) ([]byte, int, error) {
	// Check response cache first.
	cacheKey := responseCacheKey{apiPath, at}
	s.responseCacheMu.RLock()
	if e, ok := s.responseCacheMap[cacheKey]; ok && time.Since(e.created) < responseCacheTTL {
		body, code := e.body, e.code
		s.responseCacheMu.RUnlock()
		return body, code, nil
	}
	s.responseCacheMu.RUnlock()

	body, code, err := s.reconstructAt(apiPath, at)
	if err != nil {
		return nil, code, err
	}

	// Cache the result.
	s.responseCacheMu.Lock()
	s.responseCacheMap[cacheKey] = &responseCacheEntry{body: body, code: code, created: time.Now()}
	s.responseCacheBytes += int64(len(body))
	// Evict expired entries when over the byte budget.
	if s.responseCacheBytes > responseCacheMaxBytes {
		now := time.Now()
		for k, v := range s.responseCacheMap {
			if now.Sub(v.created) > responseCacheTTL {
				s.responseCacheBytes -= int64(len(v.body))
				delete(s.responseCacheMap, k)
			}
		}
	}
	s.responseCacheMu.Unlock()

	return body, code, nil
}

// reconstructAt is the uncached implementation of ReconstructAt.
func (s *CaptureStore) reconstructAt(apiPath string, at time.Time) ([]byte, int, error) {
	wi, hasWatch := s.WatchIndex[apiPath]
	if !hasWatch || len(wi.Seqs) == 0 {
		return s.Latest(apiPath, at)
	}

	snapBody, snapCode, snapTime, err := s.SnapshotAt(apiPath, at)
	if err != nil {
		return nil, 500, err
	}
	var snapList struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Metadata   json.RawMessage   `json:"metadata"`
		Items      []json.RawMessage `json:"items"`
	}
	if snapCode == 200 {
		if err := json.Unmarshal(snapBody, &snapList); err != nil {
			return snapBody, snapCode, nil
		}
	} else {
		group, version, resource, _ := parseAPIPath(apiPath)
		if resource == "" {
			return nil, 404, nil
		}
		if group == "" {
			snapList.APIVersion = version
		} else {
			snapList.APIVersion = group + "/" + version
		}
		snapList.Kind = resourceToKind(resource) + "List"
		snapList.Metadata = json.RawMessage(`{"resourceVersion":"0"}`)
	}

	// Determine which watch events fall in (snapTime, at].
	type watchEvent struct {
		idx  int
		body json.RawMessage
	}

	// Collect in-range event indices first (no I/O).
	var inRange []int
	for i := range wi.Seqs {
		if i >= len(wi.Times) || i >= len(wi.EventTypes) {
			break
		}
		evTime := wi.Times[i]
		if evTime.IsZero() || !evTime.After(snapTime) {
			continue
		}
		if !at.IsZero() && evTime.After(at) {
			break
		}
		inRange = append(inRange, i)
	}

	// Read watch event records in parallel.
	events := make([]watchEvent, len(inRange))
	var wg sync.WaitGroup
	for pos, i := range inRange {
		wg.Add(1)
		go func(pos, i int) {
			defer wg.Done()
			rec, rerr := s.readRecord(apiPath, wi.Seqs[i])
			if rerr == nil {
				events[pos] = watchEvent{idx: i, body: rec.ResponseBody}
			}
		}(pos, i)
	}
	wg.Wait()

	// Apply events in order.
	itemOrder := make([]string, 0, len(snapList.Items))
	items := make(map[string]json.RawMessage, len(snapList.Items))
	for _, item := range snapList.Items {
		k := objectKey(item)
		if k == "" {
			continue
		}
		items[k] = item
		itemOrder = append(itemOrder, k)
	}

	for pos, i := range inRange {
		ev := events[pos]
		if ev.body == nil {
			continue
		}
		k := objectKey(ev.body)
		if k == "" {
			continue
		}
		switch wi.EventTypes[i] {
		case "ADDED", "MODIFIED":
			if _, exists := items[k]; !exists {
				itemOrder = append(itemOrder, k)
			}
			items[k] = ev.body
		case "DELETED":
			delete(items, k)
		}
	}

	reconstructed := make([]json.RawMessage, 0, len(itemOrder))
	seen := make(map[string]bool, len(itemOrder))
	for _, k := range itemOrder {
		if seen[k] {
			continue
		}
		seen[k] = true
		if raw, ok := items[k]; ok {
			reconstructed = append(reconstructed, raw)
		}
	}

	out, err := json.Marshal(map[string]any{
		"apiVersion": snapList.APIVersion,
		"kind":       snapList.Kind,
		"metadata":   snapList.Metadata,
		"items":      reconstructed,
	})
	if err != nil {
		return nil, 500, err
	}
	return out, 200, nil
}

// itemDedupeKey returns a short string that uniquely identifies a raw JSON
// item by uid (preferred) or name-only fallback. Used to deduplicate items
// when aggregating across namespaces.
//
// Name-only (not namespace/name) is intentional for the no-UID case: OLM
// resources like PackageManifests have no uid and are stamped with the
// requested namespace on every namespace-scoped response, so the same package
// appears as "adani/prometheus-operator", "kube-system/prometheus-operator",
// etc. Using just the name correctly collapses these duplicates. Resources that
// have genuinely distinct same-named items in different namespaces (pods,
// services…) always carry a uid and therefore never hit this fallback.
func itemDedupeKey(raw json.RawMessage) string {
	var obj struct {
		Metadata struct {
			UID  string `json:"uid"`
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Metadata.UID == "" {
		return obj.Metadata.Name
	}
	return obj.Metadata.UID
}

// AggregateAcrossNamespaces aggregates list items from all namespaced paths.
func (s *CaptureStore) AggregateAcrossNamespaces(clusterPath string, at time.Time) ([]byte, int, error) {
	g, v, resource, _ := parseAPIPath(clusterPath)
	var pathPrefix string
	if g == "" {
		pathPrefix = "/api/" + v + "/namespaces/"
	} else {
		pathPrefix = "/apis/" + g + "/" + v + "/namespaces/"
	}
	suffix := "/" + resource

	// Safety cap: resources like packagemanifests return the full cluster list
	// for every namespace (OLM behaviour). Aggregating across all namespaces
	// would multiply the item count by the number of namespaces and materialise
	// hundreds of millions of items. Cap the aggregate at 10 000 unique items
	// (keyed by uid or name) to prevent unbounded memory use.
	const aggregateItemCap = 10_000

	var (
		allItems   []json.RawMessage
		listKind   string
		apiVersion string
		found      bool
		seen       = make(map[string]struct{})
		capped     bool
	)

	for indexPath := range s.Index {
		if !strings.HasPrefix(indexPath, pathPrefix) || !strings.HasSuffix(indexPath, suffix) {
			continue
		}
		body, code, err := s.ReconstructAt(indexPath, at)
		if err != nil || code != 200 {
			continue
		}
		var list struct {
			APIVersion string            `json:"apiVersion"`
			Kind       string            `json:"kind"`
			Items      []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			continue
		}
		if !found {
			listKind = list.Kind
			apiVersion = list.APIVersion
			found = true
		}
		for _, raw := range list.Items {
			if capped {
				break
			}
			// Deduplicate by uid or name so OLM-style resources that return the
			// full cluster list for every namespace don't multiply item count.
			key := itemDedupeKey(raw)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			allItems = append(allItems, raw)
			if len(allItems) >= aggregateItemCap {
				capped = true
			}
		}
		if capped {
			break
		}
	}

	if !found {
		return nil, 404, nil
	}
	if allItems == nil {
		allItems = []json.RawMessage{}
	}

	if listKind == "" {
		listKind = resourceToKind(resource) + "List"
	}
	if apiVersion == "" {
		if g == "" {
			apiVersion = v
		} else {
			apiVersion = g + "/" + v
		}
	}

	merged := map[string]any{
		"apiVersion": apiVersion,
		"kind":       listKind,
		"metadata":   map[string]string{"resourceVersion": "0"},
		"items":      allItems,
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, 500, err
	}
	return out, 200, nil
}

// AggregateTableAcrossNamespaces merges per-namespace Table responses.
func (s *CaptureStore) AggregateTableAcrossNamespaces(clusterPath string, at time.Time) ([]byte, int, error) {
	g, v, resource, _ := parseAPIPath(clusterPath)
	var pathPrefix string
	if g == "" {
		pathPrefix = "/api/" + v + "/namespaces/"
	} else {
		pathPrefix = "/apis/" + g + "/" + v + "/namespaces/"
	}
	tableKeySuffix := "/" + resource + "?as=Table"

	var (
		allRows []json.RawMessage
		colDefs json.RawMessage
		found   bool
	)

	for indexPath := range s.Index {
		if !strings.HasPrefix(indexPath, pathPrefix) || !strings.HasSuffix(indexPath, tableKeySuffix) {
			continue
		}
		body, code, err := s.Latest(indexPath, at)
		if err != nil || code != 200 {
			continue
		}
		var table struct {
			ColumnDefinitions json.RawMessage   `json:"columnDefinitions"`
			Rows              []json.RawMessage `json:"rows"`
		}
		if err := json.Unmarshal(body, &table); err != nil {
			continue
		}
		if !found {
			colDefs = table.ColumnDefinitions
			found = true
		}
		allRows = append(allRows, table.Rows...)
	}

	if !found {
		return nil, 404, nil
	}
	if allRows == nil {
		allRows = []json.RawMessage{}
	}

	merged := map[string]any{
		"apiVersion":        "meta.k8s.io/v1",
		"kind":              "Table",
		"metadata":          map[string]string{"resourceVersion": "0"},
		"columnDefinitions": colDefs,
		"rows":              allRows,
	}
	out, err := json.Marshal(merged)
	if err != nil {
		return nil, 500, err
	}
	return out, 200, nil
}

// parseAPIPath extracts (group, version, resource, namespace) from a REST path.
func parseAPIPath(path string) (group, version, resource, namespace string) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case len(parts) >= 3 && parts[0] == "api":
		version = parts[1]
		if len(parts) == 3 {
			resource = parts[2]
		} else if len(parts) == 5 && parts[2] == "namespaces" {
			namespace = parts[3]
			resource = parts[4]
		}
	case len(parts) >= 4 && parts[0] == "apis":
		group = parts[1]
		version = parts[2]
		if len(parts) == 4 {
			resource = parts[3]
		} else if len(parts) == 6 && parts[3] == "namespaces" {
			namespace = parts[4]
			resource = parts[5]
		}
	}
	return
}

// resourceToKind maps a plural resource name to its Kind string.
func resourceToKind(resource string) string {
	known := map[string]string{
		"bindings":                  "Binding",
		"componentstatuses":         "ComponentStatus",
		"configmaps":                "ConfigMap",
		"endpoints":                 "Endpoints",
		"events":                    "Event",
		"limitranges":               "LimitRange",
		"namespaces":                "Namespace",
		"nodes":                     "Node",
		"persistentvolumeclaims":    "PersistentVolumeClaim",
		"persistentvolumes":         "PersistentVolume",
		"pods":                      "Pod",
		"podtemplates":              "PodTemplate",
		"replicationcontrollers":    "ReplicationController",
		"resourcequotas":            "ResourceQuota",
		"secrets":                   "Secret",
		"serviceaccounts":           "ServiceAccount",
		"services":                  "Service",
		"controllerrevisions":       "ControllerRevision",
		"daemonsets":                "DaemonSet",
		"deployments":               "Deployment",
		"replicasets":               "ReplicaSet",
		"statefulsets":              "StatefulSet",
		"horizontalpodautoscalers":  "HorizontalPodAutoscaler",
		"cronjobs":                  "CronJob",
		"jobs":                      "Job",
		"ingresses":                 "Ingress",
		"ingressclasses":            "IngressClass",
		"networkpolicies":           "NetworkPolicy",
		"poddisruptionbudgets":      "PodDisruptionBudget",
		"clusterrolebindings":       "ClusterRoleBinding",
		"clusterroles":              "ClusterRole",
		"rolebindings":              "RoleBinding",
		"roles":                     "Role",
		"storageclasses":            "StorageClass",
		"volumeattachments":         "VolumeAttachment",
		"customresourcedefinitions": "CustomResourceDefinition",
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
