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
	// The parent List envelope carries the correctly-cased kind
	// (e.g. "MutatingWebhookConfigurationList") and the real apiVersion, which
	// individual items inside the list omit. Capture them as hints so we can
	// restore them on the item without guessing casing from the resource name.
	var apiVersionHint, kindHint string
	if name != "" {
		var list struct {
			Kind       string            `json:"kind"`
			APIVersion string            `json:"apiVersion"`
			Items      []json.RawMessage `json:"items"`
		}
		found := false
		if err := json.Unmarshal(body, &list); err == nil {
			apiVersionHint = list.APIVersion
			kindHint = strings.TrimSuffix(list.Kind, "List")
			if kindHint == list.Kind { // didn't end in "List" — not a usable hint
				kindHint = ""
			}
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
	raw = normalizeObjectBody(raw, path, apiVersionHint, kindHint)
	resp.Kind, resp.Namespace = kindAndNamespace(raw)
	resp.JSON = prettyJSON(raw)
	resp.YAML = toYAML(raw)
	writeJSON(w, http.StatusOK, resp)
}

// ResourceObjectRow is a single object in a generic resource-type list.
type ResourceObjectRow struct {
	Namespace string            `json:"namespace,omitempty"`
	Name      string            `json:"name"`
	Age       string            `json:"age,omitempty"`
	Link      string            `json:"link"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// ResourceList is the response from /v2/api/resource — every object of a given
// resource type (optionally scoped to a namespace) at the resolved snapshot.
type ResourceList struct {
	Resource  string              `json:"resource"`
	Kind      string              `json:"kind"`
	Namespace string              `json:"namespace,omitempty"`
	At        string              `json:"at,omitempty"`
	Total     int                 `json:"total"`
	Items     []ResourceObjectRow `json:"items"`
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
				Labels:    getLabels(it),
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
	Group      string   `json:"group"`
	Version    string   `json:"version"`
	Resource   string   `json:"resource"`
	Kind       string   `json:"kind"`
	Singular   string   `json:"singular,omitempty"`
	ShortNames []string `json:"short_names,omitempty"`
	Namespaced bool     `json:"namespaced"`
	Count      int      `json:"count"`
	Link       string   `json:"link"`
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

	meta := h.discoveryResourceMeta()
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
			dm, ok := meta[g+"/"+v+"/"+res]
			if !ok {
				dm = meta[res]
			}
			kind := dm.Kind
			if kind == "" {
				kind = kindFromResource(res)
			}
			row = &ResourceCatalogRow{Group: g, Version: v, Resource: res, Kind: kind, Singular: dm.Singular, ShortNames: dm.Short, Link: resourceLink(res, "")}
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

// discMeta is the per-resource discovery info used by the Resources catalog.
type discMeta struct {
	Kind     string
	Singular string
	Short    []string
}

// discoveryResourceMeta builds an authoritative resource→{kind, singular,
// short names} map from the captured API discovery documents (/api/v1 and
// /apis/<group>/<version>). This gives correct kinds (incl. CRDs / irregular
// plurals) and the kubectl short names for searching. Keyed by
// "group/version/resource" and, as a fallback, by bare "resource".
func (h *Handler) discoveryResourceMeta() map[string]discMeta {
	m := map[string]discMeta{}
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
				Name         string   `json:"name"`
				SingularName string   `json:"singularName"`
				Kind         string   `json:"kind"`
				ShortNames   []string `json:"shortNames"`
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
			dm := discMeta{Kind: res.Kind, Singular: res.SingularName, Short: res.ShortNames}
			m[group+"/"+version+"/"+res.Name] = dm
			if _, ok := m[res.Name]; !ok {
				m[res.Name] = dm
			}
		}
	}
	return m
}

// apiListPath builds the Kubernetes list path for a group/version/resource,
// scoped to a namespace when ns is non-empty.
func apiListPath(group, version, resource, ns string) string {
	base := "/api/" + version
	if group != "" {
		base = "/apis/" + group + "/" + version
	}
	if ns == "" {
		return base + "/" + resource
	}
	return base + "/namespaces/" + ns + "/" + resource
}

// ownerLink builds an object-view link for an ownerReference, guessing the
// resource as lowercase(kind)+"s" (correct for the common workload kinds).
func ownerLink(o ownerRef, ns string) string {
	group, version := "", o.APIVersion
	if i := strings.Index(o.APIVersion, "/"); i >= 0 {
		group, version = o.APIVersion[:i], o.APIVersion[i+1:]
	}
	if version == "" {
		version = "v1"
	}
	resource := strings.ToLower(o.Kind) + "s"
	return objectLink(apiListPath(group, version, resource, ns), o.Name)
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

// getLabels reads metadata.labels from a raw object.
func getLabels(raw json.RawMessage) map[string]string {
	var m struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m.Metadata.Labels
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

// normalizeObjectBody restores the top-level apiVersion and kind fields that
// Kubernetes omits from objects nested inside a List response. It prefers the
// hints derived from the parent List envelope (which preserve correct casing,
// e.g. "MutatingWebhookConfiguration") and falls back to inferring them from
// the capture path's group/version/resource. List and Table envelopes are left
// untouched. The fix mirrors the legacy v1 UI's normalizeDetailBody.
func normalizeObjectBody(raw json.RawMessage, path, apiVersionHint, kindHint string) json.RawMessage {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	// Leave list/table envelopes as-is — they carry their own kind.
	if _, hasItems := obj["items"]; hasItems {
		return raw
	}
	if k, _ := obj["kind"].(string); strings.HasSuffix(k, "Table") {
		return raw
	}

	group, version, resource, _ := parseAPIPath(path)
	changed := false
	if s, _ := obj["apiVersion"].(string); s == "" {
		switch {
		case apiVersionHint != "":
			obj["apiVersion"] = apiVersionHint
			changed = true
		case version != "" && group == "":
			obj["apiVersion"] = version
			changed = true
		case version != "":
			obj["apiVersion"] = group + "/" + version
			changed = true
		}
	}
	if s, _ := obj["kind"].(string); s == "" {
		switch {
		case kindHint != "":
			obj["kind"] = kindHint
			changed = true
		case resource != "":
			obj["kind"] = kindFromResource(resource)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	// Re-marshaling sorts keys alphabetically, which happens to match the
	// canonical apiVersion/kind/metadata/spec/status ordering.
	b, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return b
}

// prettyJSON re-indents a raw JSON body. Returns the original string on error.
func prettyJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}
