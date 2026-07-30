package capture

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/phenixblue/k8shark/internal/config"
)

// extractNamespaceFromObject reads metadata.namespace from a Kubernetes
// object body returned in a watch event. Returns "" if the body can't be
// parsed or the field is absent.
func extractNamespaceFromObject(obj []byte) string {
	var x struct {
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(obj, &x); err != nil {
		return ""
	}
	return x.Metadata.Namespace
}

// demuxClusterPlainList parses a standard <Kind>List response, groups items
// by metadata.namespace, and writes a per-namespace list record for each.
func (e *Engine) demuxClusterPlainList(res config.Resource, clusterPath string, body []byte, statusCode int, dedupEnabled bool) {
	var env struct {
		APIVersion string            `json:"apiVersion,omitempty"`
		Kind       string            `json:"kind,omitempty"`
		Metadata   json.RawMessage   `json:"metadata,omitempty"`
		Items      []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] cluster-wide demux %s: parse error: %v\n", clusterPath, err)
		}
		return
	}

	groups := make(map[string][]json.RawMessage)
	for _, raw := range env.Items {
		ns := extractNamespaceFromObject(raw)
		if ns == "" {
			continue
		}
		groups[ns] = append(groups[ns], raw)
	}

	e.writeDemuxedListGroups(res, clusterPath, statusCode, dedupEnabled, false, groups,
		func(items []json.RawMessage) []byte {
			return marshalDemuxedList(env.APIVersion, env.Kind, env.Metadata, items)
		})
}

// demuxClusterTableList parses a meta.k8s.io/v1 Table response, groups rows
// by their embedded object's metadata.namespace, and writes a per-namespace
// Table record for each.
func (e *Engine) demuxClusterTableList(res config.Resource, clusterPath string, body []byte, statusCode int, dedupEnabled bool) {
	var env struct {
		APIVersion        string            `json:"apiVersion,omitempty"`
		Kind              string            `json:"kind,omitempty"`
		Metadata          json.RawMessage   `json:"metadata,omitempty"`
		ColumnDefinitions json.RawMessage   `json:"columnDefinitions,omitempty"`
		Rows              []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] cluster-wide table demux %s: parse error: %v\n", clusterPath, err)
		}
		return
	}

	groups := make(map[string][]json.RawMessage)
	for _, rowRaw := range env.Rows {
		var rowObj struct {
			Object json.RawMessage `json:"object"`
		}
		if err := json.Unmarshal(rowRaw, &rowObj); err != nil {
			continue
		}
		ns := extractNamespaceFromObject(rowObj.Object)
		if ns == "" {
			continue
		}
		groups[ns] = append(groups[ns], rowRaw)
	}

	e.writeDemuxedListGroups(res, clusterPath, statusCode, dedupEnabled, true, groups,
		func(rows []json.RawMessage) []byte {
			return marshalDemuxedTable(env.APIVersion, env.Kind, env.Metadata, env.ColumnDefinitions, rows)
		})
}

// writeDemuxedListGroups writes one record per namespace group, plus an
// empty-list record for any namespace that previously had items but has
// none in this cycle — so the replay's "latest" view converges with the
// source instead of returning stale items forever.
func (e *Engine) writeDemuxedListGroups(
	res config.Resource,
	clusterPath string,
	statusCode int,
	dedupEnabled bool,
	isTable bool,
	groups map[string][]json.RawMessage,
	buildBody func([]json.RawMessage) []byte,
) {
	cacheKey := clusterPath
	if isTable {
		cacheKey += tableIndexKeySuffix
	}

	e.mu.Lock()
	if e.clusterListNamespacesSeen == nil {
		e.clusterListNamespacesSeen = make(map[string]map[string]bool)
	}
	seenPrev := e.clusterListNamespacesSeen[cacheKey]
	if seenPrev == nil {
		seenPrev = make(map[string]bool)
		e.clusterListNamespacesSeen[cacheKey] = seenPrev
	}
	var emptied []string
	for ns := range groups {
		seenPrev[ns] = true
	}
	for ns := range seenPrev {
		if _, ok := groups[ns]; !ok {
			emptied = append(emptied, ns)
		}
	}
	e.mu.Unlock()

	for ns, items := range groups {
		indexKey := buildAPIPath(res.Group, res.Version, res.Resource, ns)
		if isTable {
			indexKey += tableIndexKeySuffix
		}
		e.storeRecord(indexKey, buildBody(items), statusCode, dedupEnabled)
	}
	for _, ns := range emptied {
		indexKey := buildAPIPath(res.Group, res.Version, res.Resource, ns)
		if isTable {
			indexKey += tableIndexKeySuffix
		}
		e.storeRecord(indexKey, buildBody(nil), statusCode, dedupEnabled)
	}
}

// marshalDemuxedList builds a <Kind>List JSON body containing only items.
// Empty items render as "items":[] so kubectl/clients see a valid empty list.
func marshalDemuxedList(apiVersion, kind string, metadata json.RawMessage, items []json.RawMessage) []byte {
	if items == nil {
		items = []json.RawMessage{}
	}
	out := map[string]any{"items": items}
	if apiVersion != "" {
		out["apiVersion"] = apiVersion
	}
	if kind != "" {
		out["kind"] = kind
	}
	if len(metadata) > 0 {
		out["metadata"] = metadata
	}
	body, _ := json.Marshal(out)
	return body
}

// marshalDemuxedTable builds a meta.k8s.io/v1 Table JSON body containing only rows.
func marshalDemuxedTable(apiVersion, kind string, metadata, columnDefs json.RawMessage, rows []json.RawMessage) []byte {
	if rows == nil {
		rows = []json.RawMessage{}
	}
	out := map[string]any{"rows": rows}
	if apiVersion != "" {
		out["apiVersion"] = apiVersion
	}
	if kind != "" {
		out["kind"] = kind
	}
	if len(metadata) > 0 {
		out["metadata"] = metadata
	}
	if len(columnDefs) > 0 {
		out["columnDefinitions"] = columnDefs
	}
	body, _ := json.Marshal(out)
	return body
}
