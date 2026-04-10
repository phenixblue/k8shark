package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

// CaptureStore holds the in-memory index and provides record lookups.
type CaptureStore struct {
	Dir          string
	Metadata     capture.CaptureMetadata
	Index        capture.Index
	resourceInfo map[string]*ResourceInfo
}

// ResourceInfo describes a single captured resource type.
type ResourceInfo struct {
	Group      string
	Version    string
	Resource   string
	Kind       string
	Namespaced bool
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
		resourceInfo: make(map[string]*ResourceInfo),
	}
	s.buildResourceInfo()
	return s, nil
}

// buildResourceInfo derives ResourceInfo for each distinct resource type seen
// in the index keys.
func (s *CaptureStore) buildResourceInfo() {
	for path := range s.Index {
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
	if !at.IsZero() {
		// Walk forward; keep the latest ID whose time is <= at.
		for i, t := range entry.Times {
			if !t.After(at) {
				id = entry.RecordIDs[i]
			}
		}
	}

	data, err := os.ReadFile(filepath.Join(s.Dir, "k8shark-capture", "records", id+".json"))
	if err != nil {
		return nil, 500, fmt.Errorf("reading record %s: %w", id, err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, 500, fmt.Errorf("parsing record %s: %w", id, err)
	}
	return rec.ResponseBody, rec.ResponseCode, nil
}

// parseAPIPath extracts (group, version, resource, namespace) from a REST path.
//
//   /api/v1/pods                                  → ("", "v1", "pods", "")
//   /api/v1/namespaces/default/pods               → ("", "v1", "pods", "default")
//   /apis/apps/v1/deployments                     → ("apps", "v1", "deployments", "")
//   /apis/apps/v1/namespaces/default/deployments  → ("apps", "v1", "deployments", "default")
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
