package v2

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ObjectDetail is the response from /v2/api/object — a single captured object
// rendered as both pretty JSON and YAML, for the generic object view.
type ObjectDetail struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind,omitempty"`
	At        string `json:"at,omitempty"`
	Found     bool   `json:"found"`
	JSON      string `json:"json"`
	YAML      string `json:"yaml"`
}

// serveObject returns one captured object (by list path + name) as JSON+YAML.
// When name is empty the whole list body is returned, which lets the view show
// cluster/list-scoped resources too.
func (h *Handler) serveObject(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	path := r.URL.Query().Get("path")
	name := r.URL.Query().Get("name")
	if path == "" {
		writeError(w, http.StatusBadRequest, "missing path query parameter")
		return
	}
	at := h.resolveAt(r)
	body, code, err := h.Store.ReconstructAt(path, at)
	resp := ObjectDetail{Path: path, Name: name}
	if !at.IsZero() {
		resp.At = at.UTC().Format(time.RFC3339)
	}
	if err != nil || code != http.StatusOK || len(body) == 0 {
		writeJSON(w, http.StatusOK, resp) // Found=false
		return
	}

	raw := body
	if name != "" {
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		found := false
		if err := json.Unmarshal(body, &list); err == nil {
			for _, it := range list.Items {
				if getName(it) == name {
					raw = it
					found = true
					break
				}
			}
		}
		if !found {
			writeJSON(w, http.StatusOK, resp) // Found=false
			return
		}
	}

	resp.Found = true
	resp.Kind, resp.Namespace = kindAndNamespace(raw)
	resp.JSON = prettyJSON(raw)
	resp.YAML = toYAML(raw)
	writeJSON(w, http.StatusOK, resp)
}

// ResourceObjectRow is a single object in a generic resource-type list.
type ResourceObjectRow struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Age       string `json:"age,omitempty"`
	Link      string `json:"link"`
}

// ResourceList is the response from /v2/api/resource — every object of a given
// resource type (optionally scoped to a namespace) at the resolved snapshot.
type ResourceList struct {
	Resource string              `json:"resource"`
	Kind     string              `json:"kind"`
	Namespace string             `json:"namespace,omitempty"`
	At       string              `json:"at,omitempty"`
	Total    int                 `json:"total"`
	Items    []ResourceObjectRow `json:"items"`
}

// serveResourceList lists every object of a resource type across the capture.
// The resource is identified by its plural name (e.g. "configmaps"); the
// handler discovers the matching API paths from the index, so the caller does
// not need to know the group/version. Optional ns scopes to one namespace.
func (h *Handler) serveResourceList(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	resource := r.URL.Query().Get("resource")
	nsFilter := r.URL.Query().Get("ns")
	if resource == "" {
		writeError(w, http.StatusBadRequest, "missing resource query parameter")
		return
	}
	at := h.resolveAt(r)

	out := ResourceList{Resource: resource, Kind: kindFromResource(resource), Namespace: nsFilter}
	if !at.IsZero() {
		out.At = at.UTC().Format(time.RFC3339)
	}

	for path, entry := range h.Store.Index {
		if entry == nil || len(entry.Seqs) == 0 {
			continue
		}
		if strings.Contains(path, "?") {
			continue
		}
		_, _, res, ns := parseAPIPath(path)
		if res != resource {
			continue
		}
		if nsFilter != "" && ns != nsFilter {
			continue
		}
		body, code, err := h.Store.ReconstructAt(path, at)
		if err != nil || code != http.StatusOK || len(body) == 0 {
			continue
		}
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			continue
		}
		for _, it := range list.Items {
			name := getName(it)
			if name == "" {
				continue
			}
			out.Items = append(out.Items, ResourceObjectRow{
				Namespace: ns,
				Name:      name,
				Age:       humanAge(getCreationTimestamp(it), at),
				Link:      objectLink(path, name),
			})
		}
	}
	sort.SliceStable(out.Items, func(i, j int) bool {
		if out.Items[i].Namespace != out.Items[j].Namespace {
			return out.Items[i].Namespace < out.Items[j].Namespace
		}
		return out.Items[i].Name < out.Items[j].Name
	})
	out.Total = len(out.Items)
	writeJSON(w, http.StatusOK, out)
}

// ResourceCatalogRow describes one captured resource type (API kind).
type ResourceCatalogRow struct {
	Group      string `json:"group"`
	Version    string `json:"version"`
	Resource   string `json:"resource"`
	Kind       string `json:"kind"`
	Namespaced bool   `json:"namespaced"`
	Count      int    `json:"count"`
	Link       string `json:"link"`
}

// ResourceCatalog is the response from /v2/api/resources — every resource type
// seen in the capture, with item counts, for the Resources catalog page.
type ResourceCatalog struct {
	At        string               `json:"at,omitempty"`
	Capture   CaptureMeta          `json:"capture"`
	Total     int                  `json:"total"`
	Resources []ResourceCatalogRow `json:"resources"`
}

// serveResourceCatalog enumerates every resource type captured (one row per
// group/version/resource) with a summed item count at the resolved snapshot.
// Counts come straight from the index (no body reads).
func (h *Handler) serveResourceCatalog(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	at := h.resolveAt(r)

	kinds := h.discoveryKindMap()
	type key struct{ g, v, res string }
	agg := map[key]*ResourceCatalogRow{}
	for path, entry := range h.Store.Index {
		if entry == nil || len(entry.Seqs) == 0 {
			continue
		}
		if strings.Contains(path, "?") {
			continue
		}
		g, v, res, ns := parseAPIPath(path)
		if res == "" {
			continue
		}
		k := key{g, v, res}
		row := agg[k]
		if row == nil {
			kind := kinds[g+"/"+v+"/"+res]
			if kind == "" {
				kind = kinds[res]
			}
			if kind == "" {
				kind = kindFromResource(res)
			}
			row = &ResourceCatalogRow{Group: g, Version: v, Resource: res, Kind: kind, Link: resourceLink(res, "")}
			agg[k] = row
		}
		if ns != "" {
			row.Namespaced = true
		}
		if i := latestIndex(entry, at); i >= 0 && i < len(entry.Counts) && entry.Counts[i] > 0 {
			row.Count += entry.Counts[i]
		}
	}

	out := ResourceCatalog{Capture: h.captureMeta()}
	if !at.IsZero() {
		out.At = at.UTC().Format(time.RFC3339)
	}
	for _, row := range agg {
		out.Resources = append(out.Resources, *row)
	}
	sort.SliceStable(out.Resources, func(i, j int) bool {
		a, b := out.Resources[i], out.Resources[j]
		if a.Group != b.Group {
			return a.Group < b.Group // core ("") group sorts first
		}
		return a.Resource < b.Resource
	})
	out.Total = len(out.Resources)
	writeJSON(w, http.StatusOK, out)
}

// discoveryKindMap builds an authoritative resource→kind map from the captured
// API discovery documents (/api/v1 and /apis/<group>/<version>), which carry
// the real Kind for every resource — including CRDs and irregular plurals that
// kindFromResource can only guess at. Keyed by "group/version/resource" and,
// as a fallback, by bare "resource".
func (h *Handler) discoveryKindMap() map[string]string {
	m := map[string]string{}
	for path, entry := range h.Store.Index {
		if entry == nil || len(entry.Seqs) == 0 {
			continue
		}
		if strings.Contains(path, "?") {
			continue
		}
		// Discovery docs are /api/v1 (core) or /apis/<group>/<version>.
		if path != "/api/v1" && !(strings.HasPrefix(path, "/apis/") && strings.Count(path, "/") == 3) {
			continue
		}
		body, code, err := h.Store.Latest(path, time.Time{})
		if err != nil || code != http.StatusOK || len(body) == 0 {
			continue
		}
		var rl struct {
			GroupVersion string `json:"groupVersion"`
			Resources    []struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"resources"`
		}
		if json.Unmarshal(body, &rl) != nil {
			continue
		}
		group, version := "", rl.GroupVersion
		if i := strings.Index(rl.GroupVersion, "/"); i >= 0 {
			group, version = rl.GroupVersion[:i], rl.GroupVersion[i+1:]
		}
		for _, res := range rl.Resources {
			if res.Kind == "" || strings.Contains(res.Name, "/") { // skip subresources
				continue
			}
			m[group+"/"+version+"/"+res.Name] = res.Kind
			if _, ok := m[res.Name]; !ok {
				m[res.Name] = res.Kind
			}
		}
	}
	return m
}

// objectLink builds a hash link to the generic object view for a list path +
// object name. Both are URL-query-encoded by the frontend; here we only need a
// stable, parseable string.
func objectLink(path, name string) string {
	q := url.Values{}
	q.Set("path", path)
	q.Set("name", name)
	return "#/object?" + q.Encode()
}

// kindAndNamespace pulls apiVersion/kind and metadata.namespace from a raw
// object so the object view can show a readable header.
func kindAndNamespace(raw json.RawMessage) (kind, namespace string) {
	var m struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", ""
	}
	return m.Kind, m.Metadata.Namespace
}

// prettyJSON re-indents a raw JSON body. Returns the original string on error.
func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
