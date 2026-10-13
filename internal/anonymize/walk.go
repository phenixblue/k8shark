package anonymize

import "fmt"

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

// walkKeys is walkStrings' counterpart for object *keys*: it visits every
// key of every map in a decoded JSON tree and renames the key to whatever
// visit returns when it reports changed=true. walkStrings deliberately only
// ever sees values, which is right for the IP/URL matchers that share it —
// but the full sweep (sweep.go) has to reach keys too, because some real
// Kubernetes data is *keyed* by a name or IP: managedFields' fieldsV1 uses
// keys like `k:{"name":"redis"}` and `k:{"ip":"10.244.0.20"}`, and
// ConfigMap/Secret data keys are routinely derived from a resource name.
//
// path is the dotted path of the *containing* object, and the path handed to
// visit for a key is that object's path plus the key's own original name
// (same convention as walkStrings' value paths, so a field-path exclude rule
// that names a field also covers a rename of that field's key). Children are
// always walked under their key's original name, never its renamed one, so
// every path a rule could name is computed from the capture as it was.
//
// Renaming two distinct keys to the same name (or onto an existing key that
// isn't itself being renamed away) would silently drop one of them, so that
// is reported as an error instead — the caller fails the whole run rather
// than writing an archive that lost data. Mutates in place and returns
// whether anything changed.
func walkKeys(node interface{}, path string, visit func(path, key string) (string, bool)) (bool, error) {
	switch v := node.(type) {
	case map[string]interface{}:
		changed := false
		renames := make(map[string]string)
		for k, val := range v {
			childPath := joinFieldPath(path, k)
			if newK, ok := visit(childPath, k); ok && newK != k {
				renames[k] = newK
			}
			c, err := walkKeys(val, childPath, visit)
			if err != nil {
				return false, err
			}
			if c {
				changed = true
			}
		}
		if len(renames) == 0 {
			return changed, nil
		}
		final := make(map[string]string, len(v))
		for k := range v {
			nk := k
			if r, ok := renames[k]; ok {
				nk = r
			}
			if prior, dup := final[nk]; dup {
				return false, fmt.Errorf("anonymize: full sweep: renaming object key %q to %q at %q would collide with key %q", k, nk, path, prior)
			}
			final[nk] = k
		}
		rebuilt := make(map[string]interface{}, len(v))
		for nk, origK := range final {
			rebuilt[nk] = v[origK]
		}
		for k := range v {
			delete(v, k)
		}
		for k, val := range rebuilt {
			v[k] = val
		}
		return true, nil
	case []interface{}:
		changed := false
		childPath := path + "[*]"
		for _, val := range v {
			c, err := walkKeys(val, childPath, visit)
			if err != nil {
				return false, err
			}
			if c {
				changed = true
			}
		}
		return changed, nil
	default:
		return false, nil
	}
}
