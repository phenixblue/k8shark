package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
)

// K8sObject is the k8s object shape we inspect for filtering.
type K8sObject struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec   map[string]any `json:"spec"`
	Status map[string]any `json:"status"`
}

// LabelRequirement is one parsed segment of a labelSelector.
type LabelRequirement struct {
	Key    string
	Op     string // "=", "!=", "in", "notin", "exists", "doesnotexist"
	Values []string
}

// ParseRequirements parses a comma-separated labelSelector string into requirements.
// Supports: key=val, key==val, key!=val, key in (v1,v2), key notin (v1,v2),
//
//	key (existence), !key (non-existence).
func ParseRequirements(selector string) ([]LabelRequirement, error) {
	if selector == "" {
		return nil, nil
	}
	var reqs []LabelRequirement
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

func parseOneRequirement(seg string) (LabelRequirement, error) {
	var r LabelRequirement
	// key notin (v1,v2)
	if i := strings.Index(seg, " notin "); i >= 0 {
		r.Key = strings.TrimSpace(seg[:i])
		r.Op = "notin"
		r.Values = parseParenList(seg[i+7:])
		return r, nil
	}
	// key in (v1,v2)
	if i := strings.Index(seg, " in "); i >= 0 {
		r.Key = strings.TrimSpace(seg[:i])
		r.Op = "in"
		r.Values = parseParenList(seg[i+4:])
		return r, nil
	}
	// key!=val
	if i := strings.Index(seg, "!="); i >= 0 {
		r.Key = strings.TrimSpace(seg[:i])
		r.Op = "!="
		r.Values = []string{strings.TrimSpace(seg[i+2:])}
		return r, nil
	}
	// key==val or key=val
	for _, eq := range []string{"==", "="} {
		if i := strings.Index(seg, eq); i >= 0 {
			r.Key = strings.TrimSpace(seg[:i])
			r.Op = "="
			r.Values = []string{strings.TrimSpace(seg[i+len(eq):])}
			return r, nil
		}
	}
	// !key (non-existence)
	if strings.HasPrefix(seg, "!") {
		r.Key = strings.TrimSpace(seg[1:])
		r.Op = "doesnotexist"
		return r, nil
	}
	// key (existence check)
	if seg != "" {
		r.Key = seg
		r.Op = "exists"
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

// MatchesLabels returns true if the object's labels satisfy all requirements.
func MatchesLabels(obj *K8sObject, reqs []LabelRequirement) bool {
	for _, r := range reqs {
		val, exists := obj.Metadata.Labels[r.Key]
		switch r.Op {
		case "=":
			if !exists || val != r.Values[0] {
				return false
			}
		case "!=":
			if exists && val == r.Values[0] {
				return false
			}
		case "in":
			if !exists {
				return false
			}
			found := false
			for _, v := range r.Values {
				if val == v {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		case "notin":
			for _, v := range r.Values {
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

// FilterItemsStrict filters items for deletecollection. The field selector
// arrives already parsed and validated by ParseFieldSelector (which uses
// apimachinery's real grammar and the per-kind field-label contract), so this
// only adds the write path's extra strictness on top: labels parsed with
// labels.Parse rather than the best-effort ParseRequirements, and a selector
// that parses to zero requirements rejected instead of treated as "matches
// everything". Multiple rounds of review turned up ever-more-specific ways the
// hand-rolled label parser leniently accepted a malformed selector as "matches
// everything" (empty keys, empty segments, unbalanced set syntax, invalid key
// characters, ...) — using the real, exhaustively-validated parser closes off
// that entire class of gaps at once rather than patching one shape of malformed
// input at a time.
//
// Returns an error message suitable for a 400 response (with items nil), or
// ("", filtered) on success — filtered is items unchanged if both selectors
// are empty.
func FilterItemsStrict(items []json.RawMessage, labelSelector, fieldSelector string, fieldSel *FieldSelector) (string, []json.RawMessage) {
	var labelSel labels.Selector
	if labelSelector != "" {
		sel, err := labels.Parse(labelSelector)
		if err != nil {
			return "invalid labelSelector: " + err.Error(), nil
		}
		// A non-empty input string that parses to a selector with zero
		// requirements (e.g. all-whitespace) restricts nothing — apimachinery's
		// parser treats that the same as "no selector supplied" rather than
		// erroring, which would otherwise let it slip through as "matches
		// everything".
		if sel.Empty() {
			return fmt.Sprintf("invalid labelSelector %q: does not restrict the selection", labelSelector), nil
		}
		labelSel = sel
	}
	// fields.ParseSelector is, unlike labels.Parse, lenient about a stray comma
	// (e.g. "," or "a,,b" parses to zero requirements rather than erroring) —
	// same vacuous "matches everything" risk as above. A real apiserver accepts
	// that and deletes everything matching the remaining requirements; refusing
	// is a deliberate divergence on the write path only.
	if fieldSelector != "" && !fieldSel.Restricts() {
		return fmt.Sprintf("invalid fieldSelector %q: does not restrict the selection", fieldSelector), nil
	}
	if labelSel == nil && fieldSel == nil {
		return "", items
	}

	filtered := items[:0]
	for _, raw := range items {
		var obj K8sObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			filtered = append(filtered, raw) // can't parse — keep, don't hide (matches FilterItems' convention)
			continue
		}
		if labelSel != nil && !labelSel.Matches(labels.Set(obj.Metadata.Labels)) {
			continue
		}
		if !fieldSel.Matches(raw, &obj) {
			continue
		}
		filtered = append(filtered, raw)
	}
	return "", filtered
}

// FilterTableRows applies label/field selectors to a Table-format response,
// keeping only rows whose embedded object satisfies both selectors. Returns the
// original body unchanged if selectors are empty, the body cannot be decoded as
// a Table, or the rows array is absent.
func FilterTableRows(tableBody []byte, labelSelector string, fieldSel *FieldSelector) ([]byte, error) {
	if labelSelector == "" && fieldSel == nil {
		return tableBody, nil
	}

	labelReqs, err := ParseRequirements(labelSelector)
	if err != nil {
		return tableBody, nil // malformed selector — serve unfiltered (best-effort)
	}

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
			Object json.RawMessage `json:"object"`
		}
		if err := json.Unmarshal(row, &r); err != nil {
			filtered = append(filtered, row) // can't inspect — include to avoid data loss
			continue
		}
		var obj K8sObject
		if err := json.Unmarshal(r.Object, &obj); err != nil {
			filtered = append(filtered, row)
			continue
		}
		if MatchesLabels(&obj, labelReqs) && fieldSel.Matches(r.Object, &obj) {
			filtered = append(filtered, row)
		}
	}
	table.Rows = filtered
	return json.Marshal(table)
}

// ObjectIdentity is a namespace/name pair — enough to match a Table row back to
// the list item it was projected from.
type ObjectIdentity struct {
	Namespace string
	Name      string
}

// identityOnly decodes just the fields ObjectIdentity needs. Decoding a full
// K8sObject would impose type constraints this does not — its Labels is a
// map[string]string, so an object with a non-string label value fails to
// unmarshal entirely. That matters here: FilterItems *keeps* an item it cannot
// decode rather than hiding it, so dropping the same item's identity would make
// the Table path lose a row the JSON list path kept.
type identityOnly struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

// ListIdentities returns the identity of every item in a JSON list body. ok is
// false if the body is not a list, meaning the caller has no basis to filter a
// Table against and should not pretend otherwise.
func ListIdentities(listBody []byte) (map[ObjectIdentity]bool, bool) {
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(listBody, &list); err != nil || list.Items == nil {
		return nil, false
	}
	out := make(map[ObjectIdentity]bool, len(list.Items))
	for _, raw := range list.Items {
		var o identityOnly
		if err := json.Unmarshal(raw, &o); err != nil {
			continue
		}
		out[ObjectIdentity{o.Metadata.Namespace, o.Metadata.Name}] = true
	}
	return out, true
}

// FilterTableRowsToIdentities keeps only the Table rows whose embedded object is
// in allow.
//
// A stored Table's rows embed a PartialObjectMetadata — metadata only, since
// that is what the apiserver serves for a Table projection — so a selector on
// spec or status cannot be evaluated against them at all. A real apiserver
// filters the full objects and *then* projects to Table; this reproduces that
// order by intersecting with the identities that survived on the JSON list,
// which was filtered with the full objects in hand.
//
// Returns the body unchanged if it cannot be decoded as a Table or has no rows.
func FilterTableRowsToIdentities(tableBody []byte, allow map[ObjectIdentity]bool) ([]byte, error) {
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
			Object identityOnly `json:"object"`
		}
		if err := json.Unmarshal(row, &r); err != nil {
			filtered = append(filtered, row) // can't inspect — include to avoid data loss
			continue
		}
		if allow[ObjectIdentity{r.Object.Metadata.Namespace, r.Object.Metadata.Name}] {
			filtered = append(filtered, row)
		}
	}
	table.Rows = filtered
	return json.Marshal(table)
}

// FilterItems returns the subset of items matching both selectors. The field
// selector arrives already parsed and validated per-kind by ParseFieldSelector,
// so an unsupported label never reaches here — it was rejected as a 400 by the
// caller. Label handling stays best-effort: a malformed labelSelector returns
// items unfiltered rather than erroring, and an item that fails to unmarshal is
// kept (never silently hidden).
func FilterItems(items []json.RawMessage, labelSelector string, fieldSel *FieldSelector) []json.RawMessage {
	if labelSelector == "" && fieldSel == nil {
		return items
	}
	labelReqs, err := ParseRequirements(labelSelector)
	if err != nil {
		return items // malformed selector — best-effort, same as ApplySelectors
	}

	filtered := items[:0]
	for _, raw := range items {
		var obj K8sObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			// Can't parse this item; include it to avoid hiding data.
			filtered = append(filtered, raw)
			continue
		}
		if MatchesLabels(&obj, labelReqs) && fieldSel.Matches(raw, &obj) {
			filtered = append(filtered, raw)
		}
	}
	return filtered
}

// ApplySelectors filters a JSON list body keeping only items that match both
// selectors. Returns the original body unchanged if both selectors are empty or
// if the body is not a list.
func ApplySelectors(body []byte, labelSelector string, fieldSel *FieldSelector) ([]byte, error) {
	if labelSelector == "" && fieldSel == nil {
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

	list.Items = FilterItems(list.Items, labelSelector, fieldSel)
	return json.Marshal(list)
}
