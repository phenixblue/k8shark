package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

// CaptureStore holds the in-memory index and provides record lookups.
type CaptureStore struct {
	Dir          string
	Metadata     capture.CaptureMetadata
	Index        capture.Index
	WatchIndex   capture.WatchIndex
	resourceInfo map[string]*ResourceInfo
	recordCache  sync.Map // key: record ID string → capture.Record
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

// LoadStore reads metadata.json and index.json from an extracted archive
// directory and returns an in-memory CaptureStore.
func LoadStore(dir string) (*CaptureStore, error) {
	metaData, err := os.ReadFile(filepath.Join(dir, "k8shark-capture", "metadata.json"))
	if err != nil {
		return nil, fmt.Errorf("reading metadata: %w", err)
	}
	var meta capture.CaptureMetadata
	if err := json.Unmarshal(metaData, &meta); err != nil {
		return nil, fmt.Errorf("parsing metadata: %w", err)
	}

	idxData, err := os.ReadFile(filepath.Join(dir, "k8shark-capture", "index.json"))
	if err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}
	var idx capture.Index
	if err := json.Unmarshal(idxData, &idx); err != nil {
		return nil, fmt.Errorf("parsing index: %w", err)
	}

	s := &CaptureStore{
		Dir:          dir,
		Metadata:     meta,
		Index:        idx,
		WatchIndex:   make(capture.WatchIndex),
		resourceInfo: make(map[string]*ResourceInfo),
	}

	// Load watch-index.json if present. Older archives without it are treated
	// as snapshot-only — this is the primary backward-compatibility mechanism.
	if wiData, rerr := os.ReadFile(filepath.Join(dir, "k8shark-capture", "watch-index.json")); rerr == nil {
		var wi capture.WatchIndex
		if jerr := json.Unmarshal(wiData, &wi); jerr == nil && wi != nil {
			s.WatchIndex = wi
		}
	}

	s.buildResourceInfo()
	return s, nil
}

// buildResourceInfo derives ResourceInfo for each distinct resource type seen
// in the index keys.
func (s *CaptureStore) buildResourceInfo() {
	for path := range s.Index {
		// Skip Table-format index keys (they use "?as=Table" suffix which is
		// not a valid path component and would produce bogus ResourceInfo entries).
		if strings.Contains(path, "?") {
			continue
		}
		g, v, r, ns := parseAPIPath(path)
		if r == "" {
			continue
		}
		key := g + "/" + v + "/" + r
		if existing, ok := s.resourceInfo[key]; ok {
			// Mark namespaced if we see any namespace-scoped path for this resource.
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
	// Second pass: enrich ResourceInfo from captured /apis/<group>/<version>
	// discovery records. This gives us real shortNames, singularName, and kind
	// for CRD-backed resources that the static maps in handler.go don't cover.
	s.enrichResourceInfoFromDiscovery()
}

// enrichResourceInfoFromDiscovery reads captured APIResourceList bodies from
// the index (paths matching /apis/<g>/<v> and /api/v1) and updates
// ShortNames, SingularName, and Kind for each known ResourceInfo entry.
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
		// Only process group-version discovery paths, not resource paths.
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

		if len(entry.RecordIDs) == 0 {
			continue
		}
		// Use the latest record that we can read.
		var body []byte
		for i := len(entry.RecordIDs) - 1; i >= 0; i-- {
			rec, err := s.readRecord(entry.RecordIDs[i])
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
		if err := json.Unmarshal(body, &resList); err != nil {
			continue
		}
		if resList.Kind != "APIResourceList" {
			continue
		}

		for _, res := range resList.Resources {
			// Skip sub-resources (they have '/' in the name).
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
// Returns (nil, 404, nil) when the path is not in the index.
func (s *CaptureStore) Latest(apiPath string, at time.Time) ([]byte, int, error) {
	entry, ok := s.Index[apiPath]
	if !ok || len(entry.RecordIDs) == 0 {
		return nil, 404, nil
	}

	// Default to the most recent record.
	id := entry.RecordIDs[len(entry.RecordIDs)-1]
	if !at.IsZero() && len(entry.Times) == len(entry.RecordIDs) && len(entry.Times) > 0 {
		idx := sort.Search(len(entry.Times), func(i int) bool {
			return entry.Times[i].After(at)
		})
		if idx == 0 {
			return nil, 404, nil
		}
		id = entry.RecordIDs[idx-1]
	}

	rec, err := s.readRecord(id)
	if err != nil {
		return nil, 500, err
	}
	return rec.ResponseBody, rec.ResponseCode, nil
}

// readRecord reads and parses a single capture.Record by ID from the archive.
// Results are cached in memory so repeated reads of the same record are free.
func (s *CaptureStore) readRecord(id string) (capture.Record, error) {
	if v, ok := s.recordCache.Load(id); ok {
		return v.(capture.Record), nil
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, "k8shark-capture", "records", id+".json"))
	if err != nil {
		return capture.Record{}, fmt.Errorf("reading record %s: %w", id, err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return capture.Record{}, fmt.Errorf("parsing record %s: %w", id, err)
	}
	s.recordCache.Store(id, rec)
	return rec, nil
}

// SnapshotAt returns the body, HTTP code, and capture time of the most recent
// snapshot record (i.e. non-watch-event record) for apiPath at or before at.
// Returns (nil, 404, time.Time{}, nil) when no snapshot exists.
func (s *CaptureStore) SnapshotAt(apiPath string, at time.Time) ([]byte, int, time.Time, error) {
	entry, ok := s.Index[apiPath]
	if !ok || len(entry.RecordIDs) == 0 {
		return nil, 404, time.Time{}, nil
	}

	id := entry.RecordIDs[len(entry.RecordIDs)-1]
	snapTime := entry.Times[len(entry.Times)-1]
	if !at.IsZero() && len(entry.Times) == len(entry.RecordIDs) && len(entry.Times) > 0 {
		idx := sort.Search(len(entry.Times), func(i int) bool {
			return entry.Times[i].After(at)
		})
		if idx == 0 {
			return nil, 404, time.Time{}, nil
		}
		id = entry.RecordIDs[idx-1]
		snapTime = entry.Times[idx-1]
	}

	rec, err := s.readRecord(id)
	if err != nil {
		return nil, 500, time.Time{}, err
	}
	return rec.ResponseBody, rec.ResponseCode, snapTime, nil
}

// objectKey returns the stable identity key for a Kubernetes object JSON blob.
// Namespaced objects use "namespace/name"; cluster-scoped use "name".
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
// If a WatchIndex entry exists for the path, it applies ADDED/MODIFIED/DELETED
// watch events from (snapshot_time, T] on top of the latest snapshot.
// Falls back to Latest for paths without watch events.
func (s *CaptureStore) ReconstructAt(apiPath string, at time.Time) ([]byte, int, error) {
	wi, hasWatch := s.WatchIndex[apiPath]
	if !hasWatch || len(wi.RecordIDs) == 0 {
		// No watch events for this path — fall back to snapshot lookup.
		body, code, err := s.Latest(apiPath, at)
		return body, code, err
	}

	// Get the latest snapshot at or before T.
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
		// Parse snapshot into an ordered item map (preserves insertion order).
		if err := json.Unmarshal(snapBody, &snapList); err != nil {
			// Snapshot not a list body (e.g. single object) — fall through unchanged.
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

	// Build ordered key list and object map from snapshot items.
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

	// Collect watch events from (snapTime, at] in index order.
	for i, id := range wi.RecordIDs {
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

		eventType := wi.EventTypes[i]
		rec, rerr := s.readRecord(id)
		if rerr != nil {
			continue
		}
		k := objectKey(rec.ResponseBody)
		if k == "" {
			continue
		}

		switch eventType {
		case "ADDED":
			if _, exists := items[k]; !exists {
				itemOrder = append(itemOrder, k)
			}
			items[k] = rec.ResponseBody
		case "MODIFIED":
			if _, exists := items[k]; !exists {
				itemOrder = append(itemOrder, k)
			}
			items[k] = rec.ResponseBody
		case "DELETED":
			delete(items, k)
		}
	}

	// Reconstruct ordered items list (skip deleted).
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

// AggregateAcrossNamespaces aggregates list items from all namespaced paths
// for the given cluster-scoped path (e.g. /api/v1/pods). Returns a merged
// list JSON, the list kind/apiVersion taken from the first found namespace
// list, and 404 if nothing is found.
func (s *CaptureStore) AggregateAcrossNamespaces(clusterPath string, at time.Time) ([]byte, int, error) {
	g, v, resource, _ := parseAPIPath(clusterPath)

	// Build the namespace-scoped path prefix to match: /api/v1/namespaces/*/resource
	// or /apis/<g>/<v>/namespaces/*/resource
	var pathPrefix string
	if g == "" {
		pathPrefix = "/api/" + v + "/namespaces/"
	} else {
		pathPrefix = "/apis/" + g + "/" + v + "/namespaces/"
	}
	suffix := "/" + resource

	var (
		allItems   []json.RawMessage
		listKind   string
		apiVersion string
		found      bool
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
		allItems = append(allItems, list.Items...)
	}

	if !found {
		return nil, 404, nil
	}
	if allItems == nil {
		allItems = []json.RawMessage{}
	}

	// Build list kind from resource if not captured (fallback).
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

// AggregateTableAcrossNamespaces merges per-namespace Table responses (stored
// under "path?as=Table" keys) for a cluster-scoped path. It preserves the real
// columnDefinitions from the first namespace's response and concatenates all
// rows — so kubectl gets the full live-cluster column set for every resource
// type with no resource-specific logic required.
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
//
//	/api/v1/pods                                  → ("", "v1", "pods", "")
//	/api/v1/namespaces/default/pods               → ("", "v1", "pods", "default")
//	/apis/apps/v1/deployments                     → ("apps", "v1", "deployments", "")
//	/apis/apps/v1/namespaces/default/deployments  → ("apps", "v1", "deployments", "default")
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
	// Fallback: strip trailing 's' and title-case.
	s := strings.TrimSuffix(resource, "s")
	if s == "" {
		return resource
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
