package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

// k8s object shape we inspect for filtering.
type k8sObject struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec   map[string]any `json:"spec"`
	Status map[string]any `json:"status"`
}

// labelRequirement is one parsed segment of a labelSelector.
type labelRequirement struct {
	key   string
	op    string // "=", "!=", "in", "notin", "exists", "doesnotexist"
	values []string
}

// parseRequirements parses a comma-separated labelSelector string into requirements.
// Supports: key=val, key==val, key!=val, key in (v1,v2), key notin (v1,v2),
//           key (existence), !key (non-existence).
func parseRequirements(selector string) ([]labelRequirement, error) {
	if selector == "" {
		return nil, nil
	}
	var reqs []labelRequirement
	for _, seg := range splitRespectingParens(selector) {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		r, err := parseOneRequirement(seg)
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, r)
	}
	return reqs, nil
}

func parseOneRequirement(seg string) (labelRequirement, error) {
	var r labelRequirement
	// key notin (v1,v2)
	if i := strings.Index(seg, " notin "); i >= 0 {
		r.key = strings.TrimSpace(seg[:i])
		r.op = "notin"
		r.values = parseParenList(seg[i+7:])
		return r, nil
	}
	// key in (v1,v2)
	if i := strings.Index(seg, " in "); i >= 0 {
		r.key = strings.TrimSpace(seg[:i])
		r.op = "in"
		r.values = parseParenList(seg[i+4:])
		return r, nil
	}
	// key!=val
	if i := strings.Index(seg, "!="); i >= 0 {
		r.key = strings.TrimSpace(seg[:i])
		r.op = "!="
		r.values = []string{strings.TrimSpace(seg[i+2:])}
		return r, nil
	}
	// key==val or key=val
	for _, eq := range []string{"==", "="} {
		if i := strings.Index(seg, eq); i >= 0 {
			r.key = strings.TrimSpace(seg[:i])
			r.op = "="
			r.values = []string{strings.TrimSpace(seg[i+len(eq):])}
			return r, nil
		}
	}
	// !key (non-existence)
	if strings.HasPrefix(seg, "!") {
		r.key = strings.TrimSpace(seg[1:])
		r.op = "doesnotexist"
		return r, nil
	}
	// key (existence check)
	if seg != "" {
		r.key = seg
		r.op = "exists"
		return r, nil
	}
	return r, fmt.Errorf("empty requirement segment")
}

func parseParenList(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "(")
	s = strings.TrimSuffix(s, ")")
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		result = append(result, strings.TrimSpace(p))
	}
	return result
}

// splitRespectingParens splits on commas that are not inside parentheses.
func splitRespectingParens(s string) []string {
	var parts []string
	depth := 0
	start := 0
	for i, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

// matchesLabels returns true if the object's labels satisfy all requirements.
func matchesLabels(obj *k8sObject, reqs []labelRequirement) bool {
	for _, r := range reqs {
		val, exists := obj.Metadata.Labels[r.key]
		switch r.op {
		case "=":
			if !exists || val != r.values[0] {
				return false
			}
		case "!=":
			if exists && val == r.values[0] {
				return false
			}
		case "in":
			if !exists {
				return false
			}
			found := false
			for _, v := range r.values {
				if val == v {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "notin":
			for _, v := range r.values {
				if exists && val == v {
					return false
				}
			}
		case "exists":
			if !exists {
				return false
			}
		case "doesnotexist":
			if exists {
				return false
			}
		}
	}
	return true
}

// fieldSelectorReq is one parsed field= or field!= requirement.
type fieldSelectorReq struct {
	field   string
	op      string // "=" or "!="
	value   string
}

// parseFieldSelector parses a comma-separated fieldSelector.
// Supported fields: metadata.name, metadata.namespace.
func parseFieldSelector(selector string) []fieldSelectorReq {
	if selector == "" {
		return nil
	}
	var reqs []fieldSelectorReq
	for _, seg := range strings.Split(selector, ",") {
		seg = strings.TrimSpace(seg)
		if i := strings.Index(seg, "!="); i >= 0 {
			reqs = append(reqs, fieldSelectorReq{seg[:i], "!=", seg[i+2:]})
		} else if i := strings.Index(seg, "=="); i >= 0 {
			reqs = append(reqs, fieldSelectorReq{seg[:i], "=", seg[i+2:]})
		} else if i := strings.Index(seg, "="); i >= 0 {
			reqs = append(reqs, fieldSelectorReq{seg[:i], "=", seg[i+1:]})
		}
	}
	return reqs
}

// matchesFields returns true if the object satisfies all field selector requirements.
// Only metadata.name and metadata.namespace are supported.
func matchesFields(obj *k8sObject, reqs []fieldSelectorReq) bool {
	for _, r := range reqs {
		var actual string
		switch r.field {
		case "metadata.name":
			actual = obj.Metadata.Name
		case "metadata.namespace":
			actual = obj.Metadata.Namespace
		default:
			// Unknown field — skip (generous, avoids false negatives on unsupported fields).
			continue
		}
		switch r.op {
		case "=":
			if actual != r.value {
				return false
			}
		case "!=":
			if actual == r.value {
				return false
			}
		}
	}
	return true
}

// applySelectors filters a JSON list body keeping only items that match both
// labelSelector and fieldSelector. Returns the original body unchanged if
// both selectors are empty or if the body is not a list.
func applySelectors(body []byte, labelSelector, fieldSelector string) ([]byte, error) {
	if labelSelector == "" && fieldSelector == "" {
		return body, nil
	}

	labelReqs, err := parseRequirements(labelSelector)
	if err != nil {
		// Malformed selector — return body unfiltered (best-effort).
		return body, nil
	}
	fieldReqs := parseFieldSelector(fieldSelector)

	// Unmarshal as a generic list so we preserve all top-level fields.
	var list struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Metadata   map[string]any    `json:"metadata"`
		Items      []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil || list.Items == nil {
		// Not a list body; return as-is.
		return body, nil
	}

	filtered := list.Items[:0]
	for _, raw := range list.Items {
		var obj k8sObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			// Can't parse this item; include it to avoid hiding data.
			filtered = append(filtered, raw)
			continue
		}
		if matchesLabels(&obj, labelReqs) && matchesFields(&obj, fieldReqs) {
			filtered = append(filtered, raw)
		}
	}

	list.Items = filtered
	return json.Marshal(list)
}
