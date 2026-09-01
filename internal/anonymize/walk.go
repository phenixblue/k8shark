package anonymize

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
// Mutates map/slice values in place (both are reference types in Go) and
// returns whether anything changed, so a caller can skip re-marshaling a
// record whose body wasn't actually touched.
func walkStrings(node interface{}, visit func(string) (string, bool)) bool {
	switch v := node.(type) {
	case map[string]interface{}:
		changed := false
		for k, val := range v {
			if s, ok := val.(string); ok {
				if newS, ok := visit(s); ok {
					v[k] = newS
					changed = true
				}
				continue
			}
			if walkStrings(val, visit) {
				changed = true
			}
		}
		return changed
	case []interface{}:
		changed := false
		for i, val := range v {
			if s, ok := val.(string); ok {
				if newS, ok := visit(s); ok {
					v[i] = newS
					changed = true
				}
				continue
			}
			if walkStrings(val, visit) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}
