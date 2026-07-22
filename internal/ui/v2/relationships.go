package v2

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
)

// RelatedGroup is a titled list of related objects for the object view.
type RelatedGroup struct {
	Title string        `json:"title"`
	Items []RelatedItem `json:"items"`
}

// ObjectRelationships is the response from /v2/api/object-relationships.
type ObjectRelationships struct {
	Path   string         `json:"path"`
	Name   string         `json:"name"`
	Groups []RelatedGroup `json:"groups"`
}

func (h *Handler) serveObjectRelationships(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	path := r.URL.Query().Get("path")
	name := r.URL.Query().Get("name")
	if path == "" || name == "" {
		writeError(w, http.StatusBadRequest, "missing path or name query parameter")
		return
	}
	at := h.resolveAt(r)
	g, _, res, ns := parseAPIPath(path)
	_ = g
	out := ObjectRelationships{Path: path, Name: name}

	raw, ok := h.readObjectRaw(path, name, at)
	if ok {
		if items := ownerItems(raw, ns); len(items) > 0 {
			out.Groups = append(out.Groups, RelatedGroup{Title: "Owned by", Items: items})
		}
	}

	switch res {
	case "pods":
		if ok {
			out.Groups = append(out.Groups, podMountGroups(raw, ns)...)
		}
	case "persistentvolumeclaims":
		out.Groups = append(out.Groups, h.pvcGroups(raw, name, ns, at)...)
	case "persistentvolumes":
		out.Groups = append(out.Groups, h.pvGroups(raw, at)...)
	case "configmaps":
		out.Groups = append(out.Groups, h.usedByPods(ns, name, "configmap", at)...)
	case "secrets":
		out.Groups = append(out.Groups, h.usedByPods(ns, name, "secret", at)...)
	case "deployments", "replicasets", "statefulsets", "daemonsets", "jobs":
		out.Groups = append(out.Groups, h.ownedChildren(res, name, ns, at)...)
	}

	writeJSON(w, http.StatusOK, out)
}

// readObjectRaw returns the raw JSON of one object, by list path + name,
// merged with any overlay write (overlay wins, including a tombstone dropping
// the item so it reads as not-found).
func (h *Handler) readObjectRaw(path, name string, at time.Time) (json.RawMessage, bool) {
	for _, it := range h.reconstructMergedItems(path, at) {
		if getName(it) == name {
			return it, true
		}
	}
	return nil, false
}

func (h *Handler) podsInNS(ns string, at time.Time) []json.RawMessage {
	return h.reconstructMergedItems("/api/v1/namespaces/"+ns+"/pods", at)
}

// ownerItems parses metadata.ownerReferences into linked RelatedItems.
func ownerItems(raw json.RawMessage, ns string) []RelatedItem {
	var m struct {
		Metadata struct {
			OwnerReferences []ownerRef `json:"ownerReferences"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var out []RelatedItem
	for _, o := range m.Metadata.OwnerReferences {
		out = append(out, RelatedItem{Kind: o.Kind, Name: o.Name, Link: ownerLink(o, ns)})
	}
	return out
}

// podMountGroups extracts the ConfigMaps / Secrets / PVCs a pod mounts.
func podMountGroups(raw json.RawMessage, ns string) []RelatedGroup {
	var p podScan
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	cms, secs, pvcs := p.references()
	var groups []RelatedGroup
	add := func(title, resource string, names []string) {
		if len(names) == 0 {
			return
		}
		var items []RelatedItem
		for _, n := range names {
			items = append(items, RelatedItem{Kind: kindFromResource(resource), Name: n, Link: objectLink(apiListPath("", "v1", resource, ns), n)})
		}
		groups = append(groups, RelatedGroup{Title: title, Items: items})
	}
	add("Mounts ConfigMaps", "configmaps", cms)
	add("Mounts Secrets", "secrets", secs)
	add("Mounts PVCs", "persistentvolumeclaims", pvcs)
	return groups
}

// pvcGroups: the PV this PVC is bound to + pods that mount it.
func (h *Handler) pvcGroups(raw json.RawMessage, name, ns string, at time.Time) []RelatedGroup {
	var groups []RelatedGroup
	var pvc struct {
		Spec struct {
			VolumeName string `json:"volumeName"`
		} `json:"spec"`
	}
	if raw != nil && json.Unmarshal(raw, &pvc) == nil && pvc.Spec.VolumeName != "" {
		groups = append(groups, RelatedGroup{Title: "Bound volume", Items: []RelatedItem{{
			Kind: "PersistentVolume", Name: pvc.Spec.VolumeName,
			Link: objectLink(apiListPath("", "v1", "persistentvolumes", ""), pvc.Spec.VolumeName),
		}}})
	}
	// Pods in this namespace that mount this PVC.
	var items []RelatedItem
	for _, praw := range h.podsInNS(ns, at) {
		var p podScan
		if json.Unmarshal(praw, &p) != nil {
			continue
		}
		_, _, pvcs := p.references()
		if containsStr(pvcs, name) {
			items = append(items, RelatedItem{Kind: "Pod", Name: p.Metadata.Name, Link: podLink(ns, p.Metadata.Name)})
		}
	}
	if len(items) > 0 {
		groups = append(groups, RelatedGroup{Title: "Used by pods", Items: items})
	}
	return groups
}

// pvGroups: the PVC a PV is bound to.
func (h *Handler) pvGroups(raw json.RawMessage, at time.Time) []RelatedGroup {
	var pv struct {
		Spec struct {
			ClaimRef struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"claimRef"`
		} `json:"spec"`
	}
	if raw == nil || json.Unmarshal(raw, &pv) != nil || pv.Spec.ClaimRef.Name == "" {
		return nil
	}
	cr := pv.Spec.ClaimRef
	return []RelatedGroup{{Title: "Bound claim", Items: []RelatedItem{{
		Kind: "PersistentVolumeClaim", Name: cr.Name,
		Link: objectLink(apiListPath("", "v1", "persistentvolumeclaims", cr.Namespace), cr.Name),
	}}}}
}

// usedByPods: pods in ns that reference a configmap/secret (volumes/env).
func (h *Handler) usedByPods(ns, name, kind string, at time.Time) []RelatedGroup {
	var items []RelatedItem
	for _, praw := range h.podsInNS(ns, at) {
		var p podScan
		if json.Unmarshal(praw, &p) != nil {
			continue
		}
		cms, secs, _ := p.references()
		hit := (kind == "configmap" && containsStr(cms, name)) || (kind == "secret" && containsStr(secs, name))
		if hit {
			items = append(items, RelatedItem{Kind: "Pod", Name: p.Metadata.Name, Link: podLink(ns, p.Metadata.Name)})
		}
	}
	if len(items) == 0 {
		return nil
	}
	return []RelatedGroup{{Title: "Used by pods", Items: items}}
}

// ownedChildren: pods (and, for Deployments, ReplicaSets) owned by this object.
func (h *Handler) ownedChildren(res, name, ns string, at time.Time) []RelatedGroup {
	var groups []RelatedGroup
	// Direct child pods.
	var pods []RelatedItem
	for _, praw := range h.podsInNS(ns, at) {
		var p podScan
		if json.Unmarshal(praw, &p) != nil {
			continue
		}
		for _, o := range p.Metadata.OwnerReferences {
			if o.Name == name {
				pods = append(pods, RelatedItem{Kind: "Pod", Name: p.Metadata.Name, Link: podLink(ns, p.Metadata.Name)})
				break
			}
		}
	}
	if len(pods) > 0 {
		groups = append(groups, RelatedGroup{Title: "Pods", Items: pods})
	}
	// Deployments own ReplicaSets (which in turn own the pods).
	if res == "deployments" {
		var rss []RelatedItem
		for _, rraw := range h.reconstructMergedItems("/apis/apps/v1/namespaces/"+ns+"/replicasets", at) {
			var rs struct {
				Metadata struct {
					Name            string     `json:"name"`
					OwnerReferences []ownerRef `json:"ownerReferences"`
				} `json:"metadata"`
			}
			if json.Unmarshal(rraw, &rs) != nil {
				continue
			}
			for _, o := range rs.Metadata.OwnerReferences {
				if o.Name == name {
					rss = append(rss, RelatedItem{Kind: "ReplicaSet", Name: rs.Metadata.Name, Link: objectLink(apiListPath("apps", "v1", "replicasets", ns), rs.Metadata.Name)})
					break
				}
			}
		}
		if len(rss) > 0 {
			groups = append(groups, RelatedGroup{Title: "ReplicaSets", Items: rss})
		}
	}
	for i := range groups {
		sort.SliceStable(groups[i].Items, func(a, b int) bool { return groups[i].Items[a].Name < groups[i].Items[b].Name })
	}
	return groups
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// podScan is a minimal pod shape for extracting ConfigMap/Secret/PVC references
// from volumes and container env.
type podScan struct {
	Metadata struct {
		Name            string     `json:"name"`
		OwnerReferences []ownerRef `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		Volumes []struct {
			ConfigMap *struct {
				Name string `json:"name"`
			} `json:"configMap"`
			Secret *struct {
				SecretName string `json:"secretName"`
			} `json:"secret"`
			PersistentVolumeClaim *struct {
				ClaimName string `json:"claimName"`
			} `json:"persistentVolumeClaim"`
			Projected *struct {
				Sources []struct {
					ConfigMap *struct {
						Name string `json:"name"`
					} `json:"configMap"`
					Secret *struct {
						Name string `json:"name"`
					} `json:"secret"`
				} `json:"sources"`
			} `json:"projected"`
		} `json:"volumes"`
		Containers     []containerRefs `json:"containers"`
		InitContainers []containerRefs `json:"initContainers"`
	} `json:"spec"`
}

type containerRefs struct {
	EnvFrom []struct {
		ConfigMapRef *struct {
			Name string `json:"name"`
		} `json:"configMapRef"`
		SecretRef *struct {
			Name string `json:"name"`
		} `json:"secretRef"`
	} `json:"envFrom"`
	Env []struct {
		ValueFrom *struct {
			ConfigMapKeyRef *struct {
				Name string `json:"name"`
			} `json:"configMapKeyRef"`
			SecretKeyRef *struct {
				Name string `json:"name"`
			} `json:"secretKeyRef"`
		} `json:"valueFrom"`
	} `json:"env"`
}

// references returns the distinct ConfigMap, Secret, and PVC names a pod uses
// via volumes and container env.
func (p podScan) references() (configMaps, secrets, pvcs []string) {
	cmSet, secSet, pvcSet := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range p.Spec.Volumes {
		if v.ConfigMap != nil && v.ConfigMap.Name != "" {
			cmSet[v.ConfigMap.Name] = true
		}
		if v.Secret != nil && v.Secret.SecretName != "" {
			secSet[v.Secret.SecretName] = true
		}
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != "" {
			pvcSet[v.PersistentVolumeClaim.ClaimName] = true
		}
		if v.Projected != nil {
			for _, src := range v.Projected.Sources {
				if src.ConfigMap != nil && src.ConfigMap.Name != "" {
					cmSet[src.ConfigMap.Name] = true
				}
				if src.Secret != nil && src.Secret.Name != "" {
					secSet[src.Secret.Name] = true
				}
			}
		}
	}
	scan := func(cs []containerRefs) {
		for _, c := range cs {
			for _, ef := range c.EnvFrom {
				if ef.ConfigMapRef != nil && ef.ConfigMapRef.Name != "" {
					cmSet[ef.ConfigMapRef.Name] = true
				}
				if ef.SecretRef != nil && ef.SecretRef.Name != "" {
					secSet[ef.SecretRef.Name] = true
				}
			}
			for _, e := range c.Env {
				if e.ValueFrom == nil {
					continue
				}
				if e.ValueFrom.ConfigMapKeyRef != nil && e.ValueFrom.ConfigMapKeyRef.Name != "" {
					cmSet[e.ValueFrom.ConfigMapKeyRef.Name] = true
				}
				if e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Name != "" {
					secSet[e.ValueFrom.SecretKeyRef.Name] = true
				}
			}
		}
	}
	scan(p.Spec.Containers)
	scan(p.Spec.InitContainers)
	return keysOf(cmSet), keysOf(secSet), keysOf(pvcSet)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
