package inspect

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// Report summarises the contents of a capture archive.
type Report struct {
	CaptureID         string            `json:"capture_id"`
	CapturedAt        time.Time         `json:"captured_at"`
	CapturedUntil     time.Time         `json:"captured_until"`
	KubernetesVersion string            `json:"kubernetes_version"`
	ServerAddress     string            `json:"server_address"`
	RecordCount       int               `json:"record_count"`
	ArchivePath       string            `json:"archive_path"`
	ArchiveSize       int64             `json:"archive_size_bytes"`
	Resources         []ResourceSummary `json:"resources"`
}

// ResourceSummary describes a single captured resource type.
type ResourceSummary struct {
	Group      string   `json:"group"`
	Version    string   `json:"version"`
	Resource   string   `json:"resource"`
	Namespaced bool     `json:"namespaced"`
	Namespaces []string `json:"namespaces,omitempty"`
	Records    int      `json:"record_count"`
	Items      int      `json:"item_count"`
}

// Run opens archivePath and returns a Report without extracting to disk.
func Run(archivePath string) (*Report, error) {
	ar, err := archive.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	defer ar.Close()

	var meta capture.CaptureMetadata
	if err := ar.ReadMetadata(&meta); err != nil {
		return nil, fmt.Errorf("reading metadata: %w", err)
	}

	var idx capture.Index
	if err := ar.ReadIndex(&idx); err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}

	resources := summariseResources(ar, idx)

	return &Report{
		CaptureID:         meta.CaptureID,
		CapturedAt:        meta.CapturedAt,
		CapturedUntil:     meta.CapturedUntil,
		KubernetesVersion: meta.KubernetesVersion,
		ServerAddress:     meta.ServerAddress,
		RecordCount:       meta.RecordCount,
		ArchivePath:       archivePath,
		ArchiveSize:       ar.Size(),
		Resources:         resources,
	}, nil
}

// summariseResources aggregates per-resource information from the index.
// Item counts are read from the latest record for each path directly from the archive.
func summariseResources(ar *archive.Archive, idx capture.Index) []ResourceSummary {
	type key struct{ group, version, resource string }
	type accum struct {
		namespaced bool
		nsSeen     map[string]bool
		records    int
		items      int
	}
	byKey := map[key]*accum{}

	for path, entry := range idx {
		// Skip discovery and OpenAPI paths, and Table variants.
		if strings.Contains(path, "?") {
			continue
		}
		g, v, r, ns := parseAPIPath(path)
		if r == "" || v == "" {
			continue
		}
		k := key{g, v, r}
		a, ok := byKey[k]
		if !ok {
			a = &accum{nsSeen: map[string]bool{}}
			byKey[k] = a
		}
		if ns != "" {
			a.namespaced = true
			a.nsSeen[ns] = true
		}
		a.records += len(entry.Seqs)

		// Count items in the latest record for this path.
		if len(entry.Seqs) > 0 {
			latestSeq := entry.Seqs[len(entry.Seqs)-1]
			if data, err := ar.ReadRecord(path, latestSeq); err == nil {
				var rec capture.Record
				if jerr := json.Unmarshal(data, &rec); jerr == nil && rec.ResponseCode == 200 {
					var list struct {
						Items []json.RawMessage `json:"items"`
					}
					if jerr2 := json.Unmarshal(rec.ResponseBody, &list); jerr2 == nil {
						a.items += len(list.Items)
					}
				}
			}
		}
	}

	summaries := make([]ResourceSummary, 0, len(byKey))
	for k, a := range byKey {
		nsList := make([]string, 0, len(a.nsSeen))
		for ns := range a.nsSeen {
			nsList = append(nsList, ns)
		}
		sort.Strings(nsList)
		summaries = append(summaries, ResourceSummary{
			Group:      k.group,
			Version:    k.version,
			Resource:   k.resource,
			Namespaced: a.namespaced,
			Namespaces: nsList,
			Records:    a.records,
			Items:      a.items,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		si := summaries[i].Group + "/" + summaries[i].Version + "/" + summaries[i].Resource
		sj := summaries[j].Group + "/" + summaries[j].Version + "/" + summaries[j].Resource
		return si < sj
	})
	return summaries
}

// parseAPIPath is a local copy of the equivalent function in internal/server/store.go.
// Duplicated here to avoid an import cycle — the inspect package must not depend on
// the server package.
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
