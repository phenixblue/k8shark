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
	key    string
	op     string // "=", "!=", "in", "notin", "exists", "doesnotexist"
	values []string
}

// parseRequirements parses a comma-separated labelSelector string into requirements.
// Supports: key=val, key==val, key!=val, key in (v1,v2), key notin (v1,v2),
//
//	key (existence), !key (non-existence).
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
	field string
	op    string // "=" or "!="
	value string
}

// parseFieldSelectorSegment parses one fieldSelector segment ("key=val",
// "key==val", or "key!=val") into a requirement. ok is false if the segment
// matches none of those forms.
func parseFieldSelectorSegment(seg string) (fieldSelectorReq, bool) {
	if i := strings.Index(seg, "!="); i >= 0 {
		return fieldSelectorReq{strings.TrimSpace(seg[:i]), "!=", seg[i+2:]}, true
	}
	if i := strings.Index(seg, "=="); i >= 0 {
		return fieldSelectorReq{strings.TrimSpace(seg[:i]), "=", seg[i+2:]}, true
	}
	if i := strings.Index(seg, "="); i >= 0 {
		return fieldSelectorReq{strings.TrimSpace(seg[:i]), "=", seg[i+1:]}, true
	}
	return fieldSelectorReq{}, false
}

// parseFieldSelector parses a comma-separated fieldSelector for reads.
// Supported fields: metadata.name, metadata.namespace. An unparseable segment
// is silently skipped — best-effort, matching matchesFields' generous
// handling of unsupported keys — safe for a read, where "matches more than
// intended" only affects display fidelity. deletecollection uses the stricter
// validateSelectorsStrict instead (see below), where the same leniency would
// risk deleting more than the caller asked for.
func parseFieldSelector(selector string) []fieldSelectorReq {
	if selector == "" {
		return nil
	}
	var reqs []fieldSelectorReq
	for _, seg := range strings.Split(selector, ",") {
		if req, ok := parseFieldSelectorSegment(strings.TrimSpace(seg)); ok {
			reqs = append(reqs, req)
		}
	}
	return reqs
}

// supportedFieldSelectorKeys are the metadata fields matchesFields actually
// filters on; validateSelectorsStrict rejects any other key.
var supportedFieldSelectorKeys = map[string]bool{
	"metadata.name":      true,
	"metadata.namespace": true,
}

// validateSelectorsStrict validates labelSelector/fieldSelector the way a real
// apiserver rejects a malformed one — for callers where the best-effort
// leniency applySelectors/filterItems/parseFieldSelector use for reads would
// be unsafe. deletecollection is the motivating case: silently treating an
// unparseable labelSelector or an unsupported fieldSelector key as "matches
// everything" would delete more than the caller asked for, not just display
// more than intended. Returns a message suitable for a 400 response, or ""
// when both selectors are well-formed and reference only supported keys.
func validateSelectorsStrict(labelSelector, fieldSelector string) string {
	if labelSelector != "" {
		// parseRequirements silently skips an empty comma-separated segment
		// (e.g. from "," or "a,,b") rather than erroring, so a selector of just
		// "," parses to zero requirements — which matchesLabels treats as
		// "matches everything" (an empty requirement list vacuously passes).
		// Reject any empty segment explicitly before that leniency can apply.
		for _, seg := range splitRespectingParens(labelSelector) {
			if strings.TrimSpace(seg) == "" {
				return "invalid labelSelector: empty selector segment"
			}
		}
		reqs, err := parseRequirements(labelSelector)
		if err != nil {
			return "invalid labelSelector: " + err.Error()
		}
		for _, r := range reqs {
			if r.key == "" {
				// parseOneRequirement doesn't itself reject this: e.g. a bare "!" or
				// "=foo" segment parses to a "doesnotexist"/"="-with-empty-key
				// requirement rather than an error. An empty key never legitimately
				// exists on a real object's labels, so "doesnotexist" (and similar)
				// against it matches every item — exactly the "delete everything"
				// failure mode this validation exists to catch.
				return "invalid labelSelector: empty label key"
			}
		}
	}
	for _, seg := range strings.Split(fieldSelector, ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		req, ok := parseFieldSelectorSegment(seg)
		if !ok {
			return fmt.Sprintf("invalid fieldSelector segment %q", seg)
		}
		if !supportedFieldSelectorKeys[req.field] {
			return fmt.Sprintf("unsupported fieldSelector key %q", req.field)
		}
	}
	return ""
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

// filterTableRows applies label/field selectors to a Table-format response,
// keeping only rows whose embedded object satisfies both selectors. Returns the
// original body unchanged if selectors are empty, the body cannot be decoded as
// a Table, or the rows array is absent.
func filterTableRows(tableBody []byte, labelSelector, fieldSelector string) ([]byte, error) {
	if labelSelector == "" && fieldSelector == "" {
		return tableBody, nil
	}

	labelReqs, err := parseRequirements(labelSelector)
	if err != nil {
		return tableBody, nil // malformed selector — serve unfiltered (best-effort)
	}
	fieldReqs := parseFieldSelector(fieldSelector)

	var table struct {
		APIVersion        json.RawMessage   `json:"apiVersion"`
		Kind              json.RawMessage   `json:"kind"`
		Metadata          json.RawMessage   `json:"metadata"`
		ColumnDefinitions json.RawMessage   `json:"columnDefinitions"`
		Rows              []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(tableBody, &table); err != nil || table.Rows == nil {
		return tableBody, nil
	}

	filtered := make([]json.RawMessage, 0, len(table.Rows))
	for _, row := range table.Rows {
		var r struct {
			Object k8sObject `json:"object"`
		}
		if err := json.Unmarshal(row, &r); err != nil {
			filtered = append(filtered, row) // can't inspect — include to avoid data loss
			continue
		}
		if matchesLabels(&r.Object, labelReqs) && matchesFields(&r.Object, fieldReqs) {
			filtered = append(filtered, row)
		}
	}
	table.Rows = filtered
	return json.Marshal(table)
}

// filterItems returns the subset of items matching both labelSelector and
// fieldSelector. Best-effort, matching applySelectors: a malformed
// labelSelector returns items unfiltered rather than erroring, and an item
// that fails to unmarshal is kept (never silently hidden). Shared by
// applySelectors (list-body reads) and the writable overlay's deletecollection
// (which deletes matching items directly — there's no list body to build).
func filterItems(items []json.RawMessage, labelSelector, fieldSelector string) []json.RawMessage {
	if labelSelector == "" && fieldSelector == "" {
		return items
	}
	labelReqs, err := parseRequirements(labelSelector)
	if err != nil {
		return items // malformed selector — best-effort, same as applySelectors
	}
	fieldReqs := parseFieldSelector(fieldSelector)

	filtered := items[:0]
	for _, raw := range items {
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
	return filtered
}

// applySelectors filters a JSON list body keeping only items that match both
// labelSelector and fieldSelector. Returns the original body unchanged if
// both selectors are empty or if the body is not a list.
func applySelectors(body []byte, labelSelector, fieldSelector string) ([]byte, error) {
	if labelSelector == "" && fieldSelector == "" {
		return body, nil
	}
	if _, err := parseRequirements(labelSelector); err != nil {
		// Malformed selector — return body unfiltered (best-effort).
		return body, nil
	}

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

	list.Items = filterItems(list.Items, labelSelector, fieldSelector)
	return json.Marshal(list)
}
