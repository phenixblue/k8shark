package anonymize

// joinFieldPath appends key to base using this package's field-path
// convention (dot-separated, base=="" for the root), shared by every
// rewrite function that builds a path to check against excludedFunc —
// walkStrings' own recursive descent and imagematch.go's container walker
// alike — so the two can't drift into subtly different notations.
func joinFieldPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

// walkStrings recursively visits every string value in a decoded JSON tree
// (the map[string]interface{}/[]interface{}/string shape json.Unmarshal
// produces into interface{}), replacing a string leaf with whatever visit
// returns when visit reports changed=true. Shared by the IP and URL
// categories' full-tree safety-net scans (ipmatch.go, urlmatch.go): both
// need to find a value-shaped string (a valid IP literal; a scheme://host
// substring) wherever it occurs, not just at a fixed set of field paths —
// unlike the node/pod/workload categories, which only ever expect their
// values at specific, known field names (resourcename.go).
//
// path is the field's dot-notation path from wherever the walk started
// (typically the decoded object's own root), with every list index written
// as the literal "[*]" rather than its real index — an exclude rule targets
// a kind of occurrence (e.g. every annotation value), not one specific
// array element, so this is the same convention every other rewrite
// function in this package uses when building a path to check against
// excludedFunc. The top-level call should pass path="" for the root; walk
// itself supplies the rest as it descends.
//
// Mutates map/slice values in place (both are reference types in Go) and
// returns whether anything changed, so a caller can skip re-marshaling a
// record whose body wasn't actually touched.
func walkStrings(node interface{}, path string, visit func(path, s string) (string, bool)) bool {
	switch v := node.(type) {
	case map[string]interface{}:
		changed := false
		for k, val := range v {
			childPath := joinFieldPath(path, k)
			if s, ok := val.(string); ok {
				if newS, ok := visit(childPath, s); ok {
					v[k] = newS
					changed = true
				}
				continue
			}
			if walkStrings(val, childPath, visit) {
				changed = true
			}
		}
		return changed
	case []interface{}:
		changed := false
		childPath := path + "[*]"
		for i, val := range v {
			if s, ok := val.(string); ok {
				if newS, ok := visit(childPath, s); ok {
					v[i] = newS
					changed = true
				}
				continue
			}
			if walkStrings(val, childPath, visit) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}
