package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	apimachineryversion "k8s.io/apimachinery/pkg/version"

	kstore "github.com/phenixblue/k8shark/internal/store"
)

// tryServeFromStore writes the stored response for path and returns true if
// a successful record was found. Used to serve captured discovery responses.
func (h *handler) tryServeFromStore(w http.ResponseWriter, path string, at time.Time) bool {
	body, code, err := h.store.Latest(path, at)
	if err != nil || code != 200 {
		return false
	}
	writeRawJSON(w, code, body)
	return true
}

// writeRawJSON writes body verbatim (unlike writeJSON, which marshals a Go
// value) — used to serve a captured response's exact bytes.
func writeRawJSON(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// mergeJSONArrayField parses body as a JSON object, decodes the array under
// fieldName (defaulting to an empty array if the field is absent), and
// returns it alongside the full decoded object — so a caller can inspect what
// the array already contains, append raw additions, and re-marshal only that
// one field while every other key (and every existing array element) passes
// through completely untouched. ok is false when body isn't a JSON object
// (including a literal JSON "null", which unmarshals into a nil map without
// error) or fieldName isn't a JSON array (including a literal "null" value,
// which unmarshals into a nil slice without error), signaling the caller
// should give up and serve body verbatim rather than risk mutating a nil map
// or corrupting a differently-shaped document.
func mergeJSONArrayField(body []byte, fieldName string) (doc map[string]json.RawMessage, arr []json.RawMessage, ok bool) {
	if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
		return nil, nil, false
	}
	raw, present := doc[fieldName]
	if !present {
		return doc, nil, true
	}
	if err := json.Unmarshal(raw, &arr); err != nil || arr == nil {
		return nil, nil, false
	}
	return doc, arr, true
}

// setJSONArrayField re-marshals doc with fieldName set to arr, returning
// ok=false on any marshal error (the map value type is json.RawMessage, so
// this can only fail if arr itself contains something unmarshalable, which
// none of this file's callers ever produce — but propagating rather than
// panicking keeps every caller's existing verbatim-fallback path working).
func setJSONArrayField(doc map[string]json.RawMessage, fieldName string, arr []json.RawMessage) ([]byte, bool) {
	merged, err := json.Marshal(arr)
	if err != nil {
		return nil, false
	}
	doc[fieldName] = merged
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false
	}
	return out, true
}

// writeMergedJSON re-marshals doc with fieldName set to arr and writes it, or
// falls back to serving fallback (the original captured bytes) verbatim on
// any marshal error.
func writeMergedJSON(w http.ResponseWriter, code int, doc map[string]json.RawMessage, fieldName string, arr []json.RawMessage, fallback []byte) {
	out, ok := setJSONArrayField(doc, fieldName, arr)
	if !ok {
		writeRawJSON(w, code, fallback)
		return
	}
	writeRawJSON(w, code, out)
}

// mergeVersionsIntoGroup appends additions to a single raw APIGroupList group
// entry's own "versions" array, leaving every other field of that entry (its
// name, preferredVersion, and pre-existing version entries) untouched.
// Returns rawGroup unchanged with ok=false if it isn't a JSON object with a
// "versions" array, or on a marshal error — callers should skip the merge
// for that entry (not fail the whole response) in either case.
func mergeVersionsIntoGroup(rawGroup json.RawMessage, additions []groupVersion) (json.RawMessage, bool) {
	doc, versions, ok := mergeJSONArrayField(rawGroup, "versions")
	if !ok {
		return rawGroup, false
	}
	for _, v := range sortedGroupVersions(additions) {
		if b, err := json.Marshal(v); err == nil {
			versions = append(versions, b)
		}
	}
	out, ok := setJSONArrayField(doc, "versions", versions)
	if !ok {
		return rawGroup, false
	}
	return out, true
}

// tryServeAPIGroupListFromStore serves the captured /apis document, merging
// in whatever a CRD created via the overlay (see registerCRDResourceInfo)
// registered that the document doesn't already reflect: a version added
// under a group the document already lists (merged into that group's own
// versions array — a second CRD commonly adds a new version to a group
// capture already saw, e.g. Istio's networking.istio.io), and any group the
// document never listed at all (appended wholesale). Every pre-existing
// group/version entry is passed through as the exact bytes capture
// recorded — unlike fully re-synthesizing the document from
// h.store.Resources(), this can't lose fields a real apiserver's response
// had that the synthesizer doesn't reproduce. Serves the captured bytes
// verbatim when there's nothing new to add (the common case). Returns false
// only when path wasn't captured at all, matching tryServeFromStore.
func (h *handler) tryServeAPIGroupListFromStore(w http.ResponseWriter, path string, at time.Time) bool {
	body, code, err := h.store.Latest(path, at)
	if err != nil || code != 200 {
		return false
	}
	if h.overlay == nil {
		writeRawJSON(w, code, body)
		return true
	}
	doc, groups, ok := mergeJSONArrayField(body, "groups")
	if !ok {
		writeRawJSON(w, code, body)
		return true
	}

	resources := h.store.Resources()
	changed := false
	known := make(map[string]bool, len(groups))
	for i, raw := range groups {
		var g struct {
			Name     string `json:"name"`
			Versions []struct {
				GroupVersion string `json:"groupVersion"`
			} `json:"versions"`
		}
		if json.Unmarshal(raw, &g) != nil || g.Name == "" {
			continue
		}
		known[g.Name] = true
		knownVersions := make(map[string]bool, len(g.Versions))
		for _, v := range g.Versions {
			if v.GroupVersion != "" {
				knownVersions[v.GroupVersion] = true
			}
		}
		additions := groupVersionsFor(resources, g.Name, knownVersions)
		if len(additions) == 0 {
			continue
		}
		if merged, ok := mergeVersionsIntoGroup(raw, additions); ok {
			groups[i] = merged
			changed = true
		}
	}

	// Append any group the captured document never listed at all.
	newGroups := groupVersionsByGroup(resources, known)
	for _, g := range groupEntries(newGroups) {
		if entryBytes, err := json.Marshal(g); err == nil {
			groups = append(groups, entryBytes)
			changed = true
		}
	}

	if !changed {
		writeRawJSON(w, code, body) // nothing new since capture — serve verbatim
		return true
	}
	writeMergedJSON(w, code, doc, "groups", groups, body)
	return true
}

// tryServeAPIGroupFromStore is tryServeAPIGroupListFromStore's analogue for
// the single-group /apis/<group> document: appends any version of group a
// CRD registered via the overlay that the captured document doesn't already
// list, leaving every other field (including preferredVersion — an appended
// version is treated as additive, never promoted over whatever the captured
// document already preferred) and pre-existing version entry untouched.
func (h *handler) tryServeAPIGroupFromStore(w http.ResponseWriter, path, group string, at time.Time) bool {
	body, code, err := h.store.Latest(path, at)
	if err != nil || code != 200 {
		return false
	}
	if h.overlay == nil {
		writeRawJSON(w, code, body)
		return true
	}
	doc, versions, ok := mergeJSONArrayField(body, "versions")
	if !ok {
		writeRawJSON(w, code, body)
		return true
	}
	known := make(map[string]bool, len(versions))
	for _, raw := range versions {
		var v struct {
			GroupVersion string `json:"groupVersion"`
		}
		if json.Unmarshal(raw, &v) == nil && v.GroupVersion != "" {
			known[v.GroupVersion] = true
		}
	}
	additions := groupVersionsFor(h.store.Resources(), group, known)
	if len(additions) == 0 {
		writeRawJSON(w, code, body)
		return true
	}
	for _, v := range sortedGroupVersions(additions) {
		if entryBytes, err := json.Marshal(v); err == nil {
			versions = append(versions, entryBytes)
		}
	}
	writeMergedJSON(w, code, doc, "versions", versions, body)
	return true
}

// tryServeAPIResourceListFromStore is tryServeAPIGroupListFromStore's
// analogue for /apis/<group>/<version>'s APIResourceList: appends any
// resource a CRD registered via the overlay under this exact group/version
// (e.g. a second CRD added to a group/version a chart's earlier CRDs already
// captured) that the captured document doesn't already list, leaving every
// pre-existing resource entry's original fields (verbs, categories, etc. —
// none of which apiResourceEntry's synthesized entry reproduces) untouched.
func (h *handler) tryServeAPIResourceListFromStore(w http.ResponseWriter, path string, at time.Time) bool {
	body, code, err := h.store.Latest(path, at)
	if err != nil || code != 200 {
		return false
	}
	if h.overlay == nil {
		writeRawJSON(w, code, body)
		return true
	}
	parts := strings.SplitN(strings.TrimPrefix(path, "/apis/"), "/", 2)
	if len(parts) != 2 {
		writeRawJSON(w, code, body)
		return true
	}
	group, version := parts[0], parts[1]
	doc, resources, ok := mergeJSONArrayField(body, "resources")
	if !ok {
		writeRawJSON(w, code, body)
		return true
	}
	known := make(map[string]bool, len(resources))
	for _, raw := range resources {
		var r struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &r) == nil && r.Name != "" {
			known[r.Name] = true
		}
	}
	added := false
	for _, ri := range h.store.Resources() {
		if ri.Group != group || ri.Version != version || known[ri.Resource] {
			continue
		}
		entryBytes, err := json.Marshal(apiResourceEntry(ri))
		if err != nil {
			continue
		}
		resources = append(resources, entryBytes)
		known[ri.Resource] = true
		added = true
	}
	if !added {
		writeRawJSON(w, code, body)
		return true
	}
	writeMergedJSON(w, code, doc, "resources", resources, body)
	return true
}

// isGroupVersionPath returns true when path is exactly /apis/<group>/<version>.
func isGroupVersionPath(path string) bool {
	rest := strings.TrimPrefix(path, "/apis/")
	return len(strings.Split(rest, "/")) == 2
}

// isBareGroupPath returns true when path is exactly /apis/<group> (no version) —
// the APIGroup discovery document, distinct from /apis/<group>/<version>'s
// resource list.
func isBareGroupPath(path string) bool {
	rest := strings.TrimPrefix(path, "/apis/")
	return rest != "" && len(strings.Split(rest, "/")) == 1
}

func (h *handler) serveVersion(w http.ResponseWriter) {
	kv := h.store.Metadata.KubernetesVersion
	if kv == "" {
		kv = "v0.0.0-k8shark"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"major":        "1",
		"minor":        "0",
		"gitVersion":   kv,
		"gitCommit":    "k8shark-replay",
		"gitTreeState": "clean",
		"buildDate":    h.store.Metadata.CapturedAt.UTC().Format(time.RFC3339),
		"goVersion":    "go0.0.0",
		"compiler":     "gc",
		"platform":     "linux/amd64",
	})
}

func (h *handler) serveAPIVersions(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":       "APIVersions",
		"apiVersion": "v1",
		"versions":   []string{"v1"},
		"serverAddressByClientCIDRs": []map[string]string{
			{"clientCIDR": "0.0.0.0/0", "serverAddress": h.store.Metadata.ServerAddress},
		},
	})
}

// groupVersionsByGroup buckets resources' distinct (version, groupVersion)
// pairs by non-core group name, skipping any group name present in exclude
// (nil is fine — a nil map read always reports "absent"). Shared by
// serveAPIGroupList (building a full APIGroupList from scratch) and
// tryServeAPIGroupListFromStore (checking for/merging only the groups a
// captured discovery document doesn't already list).
func groupVersionsByGroup(resources []kstore.ResourceInfo, exclude map[string]bool) map[string][]groupVersion {
	seen := map[string][]groupVersion{}
	for _, ri := range resources {
		if ri.Group == "" || exclude[ri.Group] {
			continue
		}
		gv := groupVersion{ri.Version, ri.Group + "/" + ri.Version}
		duplicate := false
		for _, existing := range seen[ri.Group] {
			if existing.groupVersion == gv.groupVersion {
				duplicate = true
				break
			}
		}
		if !duplicate {
			seen[ri.Group] = append(seen[ri.Group], gv)
		}
	}
	return seen
}

// groupEntries renders seen (as returned by groupVersionsByGroup) as the
// sorted []map[string]any groups list APIGroupList expects.
func groupEntries(seen map[string][]groupVersion) []map[string]any {
	groupNames := make([]string, 0, len(seen))
	for g := range seen {
		groupNames = append(groupNames, g)
	}
	sort.Strings(groupNames) // seen is a map; iteration order is nondeterministic

	groups := make([]map[string]any, 0, len(seen))
	for _, g := range groupNames {
		versions := sortedGroupVersions(seen[g])
		groups = append(groups, map[string]any{
			"name":             g,
			"versions":         versions,
			"preferredVersion": versions[0],
		})
	}
	return groups
}

func (h *handler) serveAPIGroupList(w http.ResponseWriter) {
	seen := groupVersionsByGroup(h.store.Resources(), nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":       "APIGroupList",
		"apiVersion": "v1",
		"groups":     groupEntries(seen),
	})
}

// groupVersion is one version entry within a group, as returned by
// kstore.CaptureStore.Resources() (an unordered map iteration — see
// sortedGroupVersions).
type groupVersion struct{ version, groupVersion string }

// sortedGroupVersions renders gvs (a single group's versions) as the
// {groupVersion, version} maps APIGroup/APIGroupList expect, ordered by real
// Kubernetes version priority (GA descending, then beta descending, then
// alpha descending — e.g. v2, v1, v1beta2, v1beta1, v1alpha1) so
// versions[0]/preferredVersion is deterministic and semantically correct.
// h.store.Resources() iterates a map internally, so gvs arrives in random
// order on every call — without this, preferredVersion could flip between
// any of the group's versions from one call to the next.
func sortedGroupVersions(gvs []groupVersion) []map[string]string {
	sorted := append([]groupVersion(nil), gvs...)
	sort.Slice(sorted, func(i, j int) bool {
		return apimachineryversion.CompareKubeAwareVersionStrings(sorted[i].version, sorted[j].version) > 0
	})
	versions := make([]map[string]string, 0, len(sorted))
	for _, v := range sorted {
		versions = append(versions, map[string]string{
			"groupVersion": v.groupVersion,
			"version":      v.version,
		})
	}
	return versions
}

// groupVersionsFor returns the distinct (version, groupVersion) pairs for a
// single group's resources, skipping any groupVersion string present in
// exclude (nil is fine). Shared by serveAPIGroup (building a full APIGroup
// from scratch) and tryServeAPIGroupFromStore (checking for/merging only the
// versions a captured discovery document doesn't already list).
func groupVersionsFor(resources []kstore.ResourceInfo, group string, exclude map[string]bool) []groupVersion {
	var gvs []groupVersion
	seen := map[string]bool{}
	for _, ri := range resources {
		if ri.Group != group {
			continue
		}
		gv := ri.Group + "/" + ri.Version
		if seen[gv] || exclude[gv] {
			continue
		}
		seen[gv] = true
		gvs = append(gvs, groupVersion{ri.Version, gv})
	}
	return gvs
}

// serveAPIGroup serves the single-group APIGroup discovery document for
// /apis/<group> — the same grouping serveAPIGroupList does for the full
// cross-group list, scoped to one group. 404s (matching a real apiserver)
// when the group has no captured resources at all, rather than serving an
// APIGroup with an empty version list.
func (h *handler) serveAPIGroup(w http.ResponseWriter, group string) {
	gvs := groupVersionsFor(h.store.Resources(), group, nil)
	if len(gvs) == 0 {
		h.writeStatus(w, http.StatusNotFound, fmt.Sprintf("the server could not find the requested resource, group %q not found in capture", group))
		return
	}
	versions := sortedGroupVersions(gvs)
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":             "APIGroup",
		"apiVersion":       "v1",
		"name":             group,
		"versions":         versions,
		"preferredVersion": versions[0],
	})
}

// shortNamesFor returns well-known kubectl short names for a resource.
func shortNamesFor(resource string) []string {
	known := map[string][]string{
		"pods":                      {"po"},
		"services":                  {"svc"},
		"deployments":               {"deploy"},
		"daemonsets":                {"ds"},
		"namespaces":                {"ns"},
		"nodes":                     {"no"},
		"configmaps":                {"cm"},
		"persistentvolumeclaims":    {"pvc"},
		"persistentvolumes":         {"pv"},
		"serviceaccounts":           {"sa"},
		"replicasets":               {"rs"},
		"statefulsets":              {"sts"},
		"jobs":                      {"job"},
		"cronjobs":                  {"cj"},
		"ingresses":                 {"ing"},
		"horizontalpodautoscalers":  {"hpa"},
		"replicationcontrollers":    {"rc"},
		"resourcequotas":            {"quota"},
		"limitranges":               {"limits"},
		"events":                    {"ev"},
		"endpoints":                 {"ep"},
		"networkpolicies":           {"netpol"},
		"poddisruptionbudgets":      {"pdb"},
		"clusterrolebindings":       {"crb"},
		"clusterroles":              {"cr"},
		"rolebindings":              {"rb"},
		"storageclasses":            {"sc"},
		"customresourcedefinitions": {"crd"},
	}
	return known[resource]
}

// apiResourceEntry renders a single APIResourceList entry for ri. verbs is
// always the read-only set — the store has no record of which verbs a
// resource's real APIResource advertised, only what got captured/registered
// (see kstore.ResourceInfo). Shared by serveAPIResourceList (building a full
// APIResourceList from scratch, where this is the entire entry) and
// tryServeAPIResourceListFromStore (only for a resource newly registered
// after capture — e.g. a CRD created via the overlay — since an
// already-captured resource keeps its original, richer entry untouched).
func apiResourceEntry(ri kstore.ResourceInfo) map[string]any {
	entry := map[string]any{
		"name":       ri.Resource,
		"namespaced": ri.Namespaced,
		"kind":       ri.Kind,
		"verbs":      []string{"get", "list", "watch"},
	}
	// Prefer short names from the captured discovery document; fall back to
	// the built-in static map for well-known Kubernetes types.
	sn := ri.ShortNames
	if len(sn) == 0 {
		sn = shortNamesFor(ri.Resource)
	}
	if len(sn) > 0 {
		entry["shortNames"] = sn
	}
	if ri.SingularName != "" {
		entry["singularName"] = ri.SingularName
	}
	return entry
}

func (h *handler) serveAPIResourceList(w http.ResponseWriter, group, version string) {
	resources := make([]map[string]any, 0)
	for _, ri := range h.store.Resources() {
		if ri.Group != group || ri.Version != version {
			continue
		}
		resources = append(resources, apiResourceEntry(ri))
	}

	groupVersion := version
	if group != "" {
		groupVersion = group + "/" + version
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":         "APIResourceList",
		"apiVersion":   "v1",
		"groupVersion": groupVersion,
		"resources":    resources,
	})
}

func (h *handler) serveGroupResourceList(w http.ResponseWriter, path string) {
	// path is /apis/<group>/<version>
	parts := strings.SplitN(strings.TrimPrefix(path, "/apis/"), "/", 2)
	if len(parts) != 2 {
		h.writeStatus(w, http.StatusNotFound, path+" not found")
		return
	}
	h.serveAPIResourceList(w, parts[0], parts[1])
}
