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
