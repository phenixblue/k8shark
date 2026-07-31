package inspect

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/k8spath"
)

// SchemaVersion is the version of the `kshrk inspect -o json` output shape.
// Distinct from Report.ArchiveFormatVersion, which reports the version of the
// .kshrk archive being inspected — the two evolve independently.
const SchemaVersion = 1

// Report summarizes the contents of a capture archive.
type Report struct {
	SchemaVersion int `json:"schema_version"`
	// ArchiveFormatVersion is the .kshrk format version of the inspected
	// archive (capture.CurrentFormatVersion), not the version of this output.
	ArchiveFormatVersion int               `json:"archive_format_version"`
	CaptureID            string            `json:"capture_id"`
	CapturedAt           time.Time         `json:"captured_at"`
	CapturedUntil        time.Time         `json:"captured_until"`
	KubernetesVersion    string            `json:"kubernetes_version"`
	ServerAddress        string            `json:"server_address"`
	RecordCount          int               `json:"record_count"`
	ArchivePath          string            `json:"archive_path"`
	ArchiveSize          int64             `json:"archive_size_bytes"`
	Resources            []ResourceSummary `json:"resources"`
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
// identities decrypts an encrypted archive; it is ignored for plaintext
// archives, so callers may pass nil.
func Run(archivePath string, identities []age.Identity) (*Report, error) {
	ar, err := archive.OpenWithIdentities(archivePath, identities)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	defer ar.Close()

	meta, err := ar.ReadMetadata()
	if err != nil {
		return nil, fmt.Errorf("reading metadata: %w", err)
	}

	idx, err := ar.ReadIndex()
	if err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}

	resources := summarizeResources(ar, idx)

	// Pre-versioning archives (0) are treated as version 1.
	formatVersion := meta.FormatVersion
	if formatVersion == 0 {
		formatVersion = 1
	}

	return &Report{
		SchemaVersion:        SchemaVersion,
		ArchiveFormatVersion: formatVersion,

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

// summarizeResources aggregates per-resource information from the index.
// Item counts are read from the latest record for each path directly from the archive.
func summarizeResources(ar *archive.Archive, idx capture.Index) []ResourceSummary {
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
		a.records += len(entry.Seqs)

		// Count items in the latest record for this path.
		if len(entry.Seqs) > 0 {
			latestSeq := entry.Seqs[len(entry.Seqs)-1]
			if data, err := ar.ReadRecord(path, latestSeq); err == nil {
				var rec capture.Record
				if jerr := json.Unmarshal(data, &rec); jerr == nil && rec.ResponseCode == 200 {
					// A /namespaces/<ns>/ segment only means something on a
					// successful response. When a cluster-scoped resource is
					// configured with `namespaces:` by mistake, the engine
					// probes the namespaced endpoints, gets 404s, and falls
					// back to the cluster-scoped path (fetchResource in
					// internal/capture/poll.go) — but those 404 records stay in
					// the archive. Reading the namespace out of them reported
					// nodes as namespaced=true with bogus namespaces entries.
					if ns != "" {
						a.namespaced = true
						a.nsSeen[ns] = true
					}
					var list struct {
						Items []struct {
							Metadata struct {
								Namespace string `json:"namespace"`
							} `json:"metadata"`
						} `json:"items"`
					}
					if jerr2 := json.Unmarshal(rec.ResponseBody, &list); jerr2 == nil {
						a.items += len(list.Items)
						// Namespacedness has to come from the items, not the
						// request path. A namespaced resource fetched
						// cluster-wide (/api/v1/pods rather than
						// /api/v1/namespaces/x/pods) has no namespace segment
						// to read, and that is the *recommended* way to capture
						// a large cluster — it costs one LIST instead of one
						// per namespace. Deriving it from the path alone
						// reported namespaced=false for every namespaced
						// resource in exactly that case.
						for _, it := range list.Items {
							if it.Metadata.Namespace != "" {
								a.namespaced = true
								a.nsSeen[it.Metadata.Namespace] = true
							}
						}
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

// parseAPIPath extracts (group, version, resource, namespace) from a REST path.
func parseAPIPath(path string) (group, version, resource, namespace string) {
	return k8spath.Parse(path)
}
