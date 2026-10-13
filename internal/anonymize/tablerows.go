package anonymize

// tableCellIndex returns the index into a Table/TableSchema response's own
// rows[*].cells arrays whose columnDefinitions[*].name matches colName
// (e.g. "Name", "Namespace") — the positional slot kubectl's default output
// actually prints, as opposed to a row's embedded "object" field, which has
// real field names but is never what kubectl renders by default.
func tableCellIndex(obj map[string]interface{}, colName string) (int, bool) {
	colDefs, ok := obj["columnDefinitions"].([]interface{})
	if !ok {
		return 0, false
	}
	for i, raw := range colDefs {
		cd, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if name, _ := cd["name"].(string); name == colName {
			return i, true
		}
	}
	return 0, false
}

// rewriteTableRows applies rewriteObject — the same per-object schema-aware
// rewrite function a plain GET/List response's item already goes through —
// to every row[*].object in a captured Table/TableSchema response, using
// the real Kind resolved from the record's own APIPath (see
// tableRowKindFromAPIPath's own doc comment for why that's the only place
// it's recoverable from at all).
//
// When a row's object was actually changed, mirrorCell (if non-nil) runs
// next so the caller can copy whatever field it just rewrote into that
// Table's own printed cell representation too — a row's "cells" are a
// second, unnamed-field representation of the same value (what kubectl's
// default output actually renders), which rewriteObject's own field-path
// matching can't reach on its own. mirrorCell is handed the whole Table
// response (for tableCellIndex), the row, the row's own (already-rewritten)
// object, and the resolved rowKind — callers decide for themselves which
// field(s) and category/exclude check apply, since that differs between
// namespace.go's and resourcename.go's own callers.
//
// Returns false (not an error) when apiPath's resource type isn't one
// tableRowKindFromAPIPath recognizes (every native kind outside
// resourceKindTable/"namespaces", and any CRD) or the response has no rows
// at all — the caller's own Table branch already means "this is a Table/
// TableSchema response", so neither case is an error, just nothing this
// function can usefully do; --full-sweep remains the only way to reach
// those.
func rewriteTableRows(
	obj map[string]interface{},
	apiPath string,
	rewriteObject func(item map[string]interface{}, itemKind string) bool,
	mirrorCell func(tableObj, row, rowObj map[string]interface{}, rowKind string),
) bool {
	rowKind, ok := tableRowKindFromAPIPath(apiPath)
	if !ok {
		return false
	}
	rows, ok := obj["rows"].([]interface{})
	if !ok {
		return false
	}
	modified := false
	for i, rowRaw := range rows {
		row, ok := rowRaw.(map[string]interface{})
		if !ok {
			continue
		}
		rowObj, ok := row["object"].(map[string]interface{})
		if !ok {
			continue
		}
		if !rewriteObject(rowObj, rowKind) {
			continue
		}
		if mirrorCell != nil {
			mirrorCell(obj, row, rowObj, rowKind)
		}
		rows[i] = row
		modified = true
	}
	return modified
}
