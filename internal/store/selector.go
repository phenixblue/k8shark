package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/fields"
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
	key    string
	op     string // "=", "!=", "in", "notin", "exists", "doesnotexist"
	values []string
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

// MatchesLabels returns true if the object's labels satisfy all requirements.
func MatchesLabels(obj *K8sObject, reqs []LabelRequirement) bool {
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

// FieldSelectorReq is one parsed field= or field!= requirement.
type FieldSelectorReq struct {
	field string
	op    string // "=" or "!="
	value string
}

// parseFieldSelectorSegment parses one fieldSelector segment ("key=val",
// "key==val", or "key!=val") into a requirement. ok is false if the segment
// matches none of those forms.
func parseFieldSelectorSegment(seg string) (FieldSelectorReq, bool) {
	if i := strings.Index(seg, "!="); i >= 0 {
		return FieldSelectorReq{strings.TrimSpace(seg[:i]), "!=", strings.TrimSpace(seg[i+2:])}, true
	}
	if i := strings.Index(seg, "=="); i >= 0 {
		return FieldSelectorReq{strings.TrimSpace(seg[:i]), "=", strings.TrimSpace(seg[i+2:])}, true
	}
	if i := strings.Index(seg, "="); i >= 0 {
		return FieldSelectorReq{strings.TrimSpace(seg[:i]), "=", strings.TrimSpace(seg[i+1:])}, true
	}
	return FieldSelectorReq{}, false
}

// ParseFieldSelector parses a comma-separated fieldSelector for reads.
// Supported fields: metadata.name, metadata.namespace. An unparseable segment
// is silently skipped — best-effort, matching MatchesFields' generous
// handling of unsupported keys — safe for a read, where "matches more than
// intended" only affects display fidelity. deletecollection uses
// FilterItemsStrict instead (see below), which parses with apimachinery's
// real selector grammar rather than this lenient one, since the same
// leniency there would risk deleting more than the caller asked for.
func ParseFieldSelector(selector string) []FieldSelectorReq {
	if selector == "" {
		return nil
	}
	var reqs []FieldSelectorReq
	for _, seg := range strings.Split(selector, ",") {
		if req, ok := parseFieldSelectorSegment(strings.TrimSpace(seg)); ok {
			reqs = append(reqs, req)
		}
	}
	return reqs
}

// supportedFieldSelectorKeys are the metadata fields MatchesFields (and
// fieldsAdapter, below) actually filter on; FilterItemsStrict rejects any
// other key.
var supportedFieldSelectorKeys = map[string]bool{
	"metadata.name":      true,
	"metadata.namespace": true,
}

// fieldsAdapter exposes a K8sObject's supported field-selector keys through
// k8s.io/apimachinery/pkg/fields.Fields, so fields.Selector.Matches can
// evaluate it directly.
type fieldsAdapter struct{ obj *K8sObject }

func (f fieldsAdapter) Has(field string) bool { return supportedFieldSelectorKeys[field] }

func (f fieldsAdapter) Get(field string) string {
	switch field {
	case "metadata.name":
		return f.obj.Metadata.Name
	case "metadata.namespace":
		return f.obj.Metadata.Namespace
	default:
		return ""
	}
}

// FilterItemsStrict filters items for deletecollection using
// k8s.io/apimachinery's own label/field selector grammar and matching
// (labels.Parse, fields.ParseSelector) instead of FilterItems' best-effort,
// hand-rolled parser. Multiple rounds of review turned up ever-more-specific
// ways the hand-rolled parser leniently accepted a malformed selector as
// "matches everything" (empty keys, empty segments, unbalanced set syntax,
// invalid key characters, ...) — using the real, exhaustively-validated
// parser closes off that entire class of gaps at once rather than patching
// one shape of malformed input at a time. The read path (ApplySelectors,
// FilterTableRows) is unaffected — it keeps its existing best-effort
// FilterItems, where "matches more than intended" only affects display
// fidelity, not what gets deleted.
//
// Returns an error message suitable for a 400 response (with items nil), or
// ("", filtered) on success — filtered is items unchanged if both selectors
// are empty.
func FilterItemsStrict(items []json.RawMessage, labelSelector, fieldSelector string) (string, []json.RawMessage) {
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
	var fieldSel fields.Selector
	if fieldSelector != "" {
		sel, err := fields.ParseSelector(fieldSelector)
		if err != nil {
			return "invalid fieldSelector: " + err.Error(), nil
		}
		// fields.ParseSelector is, unlike labels.Parse, lenient about a stray
		// comma (e.g. "," or "a,,b" parses to zero requirements rather than
		// erroring) — same vacuous "matches everything" risk as above.
		if sel.Empty() {
			return fmt.Sprintf("invalid fieldSelector %q: does not restrict the selection", fieldSelector), nil
		}
		for _, r := range sel.Requirements() {
			if !supportedFieldSelectorKeys[r.Field] {
				return fmt.Sprintf("unsupported fieldSelector key %q", r.Field), nil
			}
		}
		fieldSel = sel
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
		if fieldSel != nil && !fieldSel.Matches(fieldsAdapter{&obj}) {
			continue
		}
		filtered = append(filtered, raw)
	}
	return "", filtered
}

// MatchesFields returns true if the object satisfies all field selector requirements.
// Only metadata.name and metadata.namespace are supported.
func MatchesFields(obj *K8sObject, reqs []FieldSelectorReq) bool {
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

// FilterTableRows applies label/field selectors to a Table-format response,
// keeping only rows whose embedded object satisfies both selectors. Returns the
// original body unchanged if selectors are empty, the body cannot be decoded as
// a Table, or the rows array is absent.
func FilterTableRows(tableBody []byte, labelSelector, fieldSelector string) ([]byte, error) {
	if labelSelector == "" && fieldSelector == "" {
		return tableBody, nil
	}

	labelReqs, err := ParseRequirements(labelSelector)
	if err != nil {
		return tableBody, nil // malformed selector — serve unfiltered (best-effort)
	}
	fieldReqs := ParseFieldSelector(fieldSelector)

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
			Object K8sObject `json:"object"`
		}
		if err := json.Unmarshal(row, &r); err != nil {
			filtered = append(filtered, row) // can't inspect — include to avoid data loss
			continue
		}
		if MatchesLabels(&r.Object, labelReqs) && MatchesFields(&r.Object, fieldReqs) {
			filtered = append(filtered, row)
		}
	}
	table.Rows = filtered
	return json.Marshal(table)
}

// FilterItems returns the subset of items matching both labelSelector and
// fieldSelector. Best-effort, matching ApplySelectors (the only caller): a
// malformed labelSelector returns items unfiltered rather than erroring, and
// an item that fails to unmarshal is kept (never silently hidden) — safe for
// a read, where "matches more than intended" only affects display fidelity.
// The writable overlay's deletecollection uses the stricter FilterItemsStrict
// instead (below), where the same leniency would risk deleting more than the
// caller asked for.
func FilterItems(items []json.RawMessage, labelSelector, fieldSelector string) []json.RawMessage {
	if labelSelector == "" && fieldSelector == "" {
		return items
	}
	labelReqs, err := ParseRequirements(labelSelector)
	if err != nil {
		return items // malformed selector — best-effort, same as ApplySelectors
	}
	fieldReqs := ParseFieldSelector(fieldSelector)

	filtered := items[:0]
	for _, raw := range items {
		var obj K8sObject
		if err := json.Unmarshal(raw, &obj); err != nil {
			// Can't parse this item; include it to avoid hiding data.
			filtered = append(filtered, raw)
			continue
		}
		if MatchesLabels(&obj, labelReqs) && MatchesFields(&obj, fieldReqs) {
			filtered = append(filtered, raw)
		}
	}
	return filtered
}

// ApplySelectors filters a JSON list body keeping only items that match both
// labelSelector and fieldSelector. Returns the original body unchanged if
// both selectors are empty or if the body is not a list.
func ApplySelectors(body []byte, labelSelector, fieldSelector string) ([]byte, error) {
	if labelSelector == "" && fieldSelector == "" {
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

	list.Items = FilterItems(list.Items, labelSelector, fieldSelector)
	return json.Marshal(list)
}
