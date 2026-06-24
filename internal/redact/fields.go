package redact

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/phenixblue/k8shark/internal/config"
)

// tokenKind represents the type of a path token.
type tokenKind int

const (
	tokenKey       tokenKind = iota // plain key, e.g. "spec"
	tokenIndex                      // numeric index, e.g. [3]
	tokenWildcard                   // [*]
	tokenRecursive                  // **
)

type pathToken struct {
	kind  tokenKind
	key   string // tokenKey only
	index int    // tokenIndex only
}

var schemaKindTypes = map[string]reflect.Type{
	"Pod":                   reflect.TypeOf(corev1.Pod{}),
	"Deployment":            reflect.TypeOf(appsv1.Deployment{}),
	"DaemonSet":             reflect.TypeOf(appsv1.DaemonSet{}),
	"StatefulSet":           reflect.TypeOf(appsv1.StatefulSet{}),
	"ReplicaSet":            reflect.TypeOf(appsv1.ReplicaSet{}),
	"Job":                   reflect.TypeOf(batchv1.Job{}),
	"CronJob":               reflect.TypeOf(batchv1.CronJob{}),
	"ConfigMap":             reflect.TypeOf(corev1.ConfigMap{}),
	"Secret":                reflect.TypeOf(corev1.Secret{}),
	"Service":               reflect.TypeOf(corev1.Service{}),
	"Namespace":             reflect.TypeOf(corev1.Namespace{}),
	"Node":                  reflect.TypeOf(corev1.Node{}),
	"PersistentVolume":      reflect.TypeOf(corev1.PersistentVolume{}),
	"PersistentVolumeClaim": reflect.TypeOf(corev1.PersistentVolumeClaim{}),
}

// tokenizePath converts a field path string into a slice of pathTokens.
// Supported syntax:
//
//	foo.bar         → key("foo"), key("bar")
//	items[*].value  → key("items"), wildcard, key("value")
//	items[2].value  → key("items"), index(2), key("value")
//	**.password     → recursive, key("password")
func tokenizePath(path string) ([]pathToken, error) {
	var tokens []pathToken
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			continue
		}
		// Handle recursive descent sentinel "**"
		if seg == "**" {
			tokens = append(tokens, pathToken{kind: tokenRecursive})
			continue
		}
		// Check for bracket notation within the segment, e.g. "items[*]" or "items[3]"
		bracketIdx := strings.Index(seg, "[")
		if bracketIdx == -1 {
			tokens = append(tokens, pathToken{kind: tokenKey, key: seg})
			continue
		}
		// Part before the bracket is a key
		if bracketIdx > 0 {
			tokens = append(tokens, pathToken{kind: tokenKey, key: seg[:bracketIdx]})
		}
		// Parse zero or more [...] suffixes
		rest := seg[bracketIdx:]
		for len(rest) > 0 {
			if rest[0] != '[' {
				return nil, fmt.Errorf("unexpected character %q in path segment %q", rest[0], seg)
			}
			end := strings.Index(rest, "]")
			if end == -1 {
				return nil, fmt.Errorf("unclosed '[' in path segment %q", seg)
			}
			inner := rest[1:end]
			if inner == "*" {
				tokens = append(tokens, pathToken{kind: tokenWildcard})
			} else {
				n, err := strconv.Atoi(inner)
				if err != nil {
					return nil, fmt.Errorf("invalid array index %q in path %q", inner, path)
				}
				tokens = append(tokens, pathToken{kind: tokenIndex, index: n})
			}
			rest = rest[end+1:]
		}
	}
	return tokens, nil
}

// inferType returns its best-guess type name for a JSON-decoded value.
func inferType(v interface{}) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	case nil:
		return "null"
	default:
		return "string"
	}
}

// convertReplacement parses the string replacement into a typed Go value whose
// JSON serialization will match the expected type.
func convertReplacement(replacement, valueType string) (interface{}, error) {
	switch strings.ToLower(valueType) {
	case "string":
		return replacement, nil
	case "int", "integer":
		n, err := strconv.ParseInt(replacement, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("replacement %q is not a valid integer: %w", replacement, err)
		}
		return float64(n), nil
	case "number", "float":
		f, err := strconv.ParseFloat(replacement, 64)
		if err != nil {
			return nil, fmt.Errorf("replacement %q is not a valid number: %w", replacement, err)
		}
		return f, nil
	case "bool":
		b, err := strconv.ParseBool(replacement)
		if err != nil {
			return nil, fmt.Errorf("replacement %q is not a valid bool (use true/false): %w", replacement, err)
		}
		return b, nil
	case "array":
		return []interface{}{}, nil
	case "object":
		return map[string]interface{}{}, nil
	case "null":
		return nil, nil
	default:
		// Unknown hint — treat as string
		return replacement, nil
	}
}

func jsonTypeFromReflect(t reflect.Type) (string, bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.String:
		return "string", true
	case reflect.Bool:
		return "bool", true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer", true
	case reflect.Float32, reflect.Float64:
		return "number", true
	case reflect.Slice, reflect.Array:
		return "array", true
	case reflect.Struct, reflect.Map:
		return "object", true
	default:
		return "", false
	}
}

func fieldByJSONName(t reflect.Type, name string) (reflect.Type, bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		jsonTag := f.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}
		tagName := strings.Split(jsonTag, ",")[0]
		if tagName == "" {
			tagName = f.Name
		}
		if tagName == name {
			return f.Type, true
		}
	}
	return nil, false
}

func schemaTypeForPath(kind, fieldPath string) (string, bool) {
	if kind == "" {
		return "", false
	}
	kind = strings.TrimSuffix(kind, "List")
	root, ok := schemaKindTypes[kind]
	if !ok {
		return "", false
	}

	tokens, err := tokenizePath(fieldPath)
	if err != nil || len(tokens) == 0 {
		return "", false
	}

	t := root
	for i := range tokens {
		tok := tokens[i]
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}

		switch tok.kind {
		case tokenRecursive:
			return "", false
		case tokenWildcard, tokenIndex:
			if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
				return "", false
			}
			t = t.Elem()
		case tokenKey:
			switch t.Kind() {
			case reflect.Struct:
				next, ok := fieldByJSONName(t, tok.key)
				if !ok {
					return "", false
				}
				t = next
			case reflect.Map:
				t = t.Elem()
			default:
				return "", false
			}
		}
	}

	return jsonTypeFromReflect(t)
}

// typedReplacement returns the typed replacement value for a given rule and the
// current field value.
func typedReplacement(current interface{}, kind, fieldPath string, rule *config.RedactionRule) (interface{}, error) {
	t := rule.ValueType
	if t == "" {
		if schemaType, ok := schemaTypeForPath(kind, fieldPath); ok {
			t = schemaType
		} else {
			t = inferType(current)
		}
	}
	return convertReplacement(rule.Replacement, t)
}

// walk traverses the JSON-decoded value tree following tokens and calls
// replaceFn at every terminal position. Returns the (possibly new) value,
// whether anything was changed, and any error.
func walk(val interface{}, tokens []pathToken, replaceFn func(interface{}) (interface{}, error)) (interface{}, bool, error) {
	if len(tokens) == 0 {
		// Terminal — perform replacement
		newVal, err := replaceFn(val)
		if err != nil {
			return val, false, err
		}
		return newVal, true, nil
	}

	tok := tokens[0]
	rest := tokens[1:]

	switch tok.kind {
	case tokenKey:
		m, ok := val.(map[string]interface{})
		if !ok {
			return val, false, nil
		}
		child, exists := m[tok.key]
		if !exists {
			return val, false, nil
		}
		newChild, changed, err := walk(child, rest, replaceFn)
		if err != nil {
			return val, false, err
		}
		if changed {
			m[tok.key] = newChild
		}
		return val, changed, nil

	case tokenIndex:
		s, ok := val.([]interface{})
		if !ok || tok.index < 0 || tok.index >= len(s) {
			return val, false, nil
		}
		newElem, changed, err := walk(s[tok.index], rest, replaceFn)
		if err != nil {
			return val, false, err
		}
		if changed {
			s[tok.index] = newElem
		}
		return val, changed, nil

	case tokenWildcard:
		s, ok := val.([]interface{})
		if !ok {
			return val, false, nil
		}
		modified := false
		for i, elem := range s {
			newElem, changed, err := walk(elem, rest, replaceFn)
			if err != nil {
				return val, modified, err
			}
			if changed {
				s[i] = newElem
				modified = true
			}
		}
		return val, modified, nil

	case tokenRecursive:
		// Try to match rest starting at this level
		newVal, modified, err := walk(val, rest, replaceFn)
		if err != nil {
			return val, false, err
		}
		if modified {
			val = newVal
		}
		// Also recurse into all children with the full recursive pattern
		switch v := val.(type) {
		case map[string]interface{}:
			for k, child := range v {
				newChild, changed, err := walk(child, tokens, replaceFn)
				if err != nil {
					return val, modified, err
				}
				if changed {
					v[k] = newChild
					modified = true
				}
			}
		case []interface{}:
			for i, elem := range v {
				newElem, changed, err := walk(elem, tokens, replaceFn)
				if err != nil {
					return val, modified, err
				}
				if changed {
					v[i] = newElem
					modified = true
				}
			}
		}
		return val, modified, nil
	}

	return val, false, nil
}

// extractString returns the string value of a top-level key in a JSON object map.
func extractString(m map[string]interface{}, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// extractNestedString returns a string value at a two-level path.
func extractNestedString(m map[string]interface{}, key1, key2 string) string {
	v, ok := m[key1]
	if !ok {
		return ""
	}
	nested, ok := v.(map[string]interface{})
	if !ok {
		return ""
	}
	s, _ := nested[key2].(string)
	return s
}

func extractNestedLabels(m map[string]interface{}) map[string]string {
	v, ok := m["metadata"]
	if !ok {
		return nil
	}
	meta, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	lv, ok := meta["labels"]
	if !ok {
		return nil
	}
	lm, ok := lv.(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]string, len(lm))
	for k, raw := range lm {
		s, ok := raw.(string)
		if ok {
			out[k] = s
		}
	}
	return out
}

func ruleMatchesLabels(rule *config.RedactionRule, labelsMap map[string]string) (bool, error) {
	if strings.TrimSpace(rule.LabelSelector) == "" {
		return true, nil
	}
	sel, err := labels.Parse(rule.LabelSelector)
	if err != nil {
		return false, fmt.Errorf("invalid labelSelector %q: %w", rule.LabelSelector, err)
	}
	return sel.Matches(labels.Set(labelsMap)), nil
}

// applyRuleToObj applies a single redaction rule to a decoded JSON object (one
// resource, not a list). Returns true if any modification was made.
func applyRuleToObj(obj map[string]interface{}, kind string, rule *config.RedactionRule) (bool, error) {
	tokens, err := tokenizePath(rule.FieldPath)
	if err != nil {
		return false, fmt.Errorf("parsing field path %q: %w", rule.FieldPath, err)
	}
	if len(tokens) == 0 {
		return false, nil
	}

	replaceFn := func(current interface{}) (interface{}, error) {
		return typedReplacement(current, kind, rule.FieldPath, rule)
	}

	_, changed, err := walk(obj, tokens, replaceFn)
	return changed, err
}

// ruleMatchesKind reports whether a rule should be considered for an object with
// the given kind. rule.Kind == "" or "*" matches all kinds.
// A kind of "FooList" also matches a rule with Kind == "Foo".
func ruleMatchesKind(rule *config.RedactionRule, kind string) bool {
	rk := rule.Kind
	if rk == "" || rk == "*" {
		return true
	}
	return kind == rk || kind == rk+"List"
}

// ApplyRules applies all matching redaction rules to obj, which is a fully
// JSON-decoded resource object (map[string]interface{}). It handles both
// individual resource objects and list responses (items[]).
// Returns true if any field was modified.
func ApplyRules(obj map[string]interface{}, rules []config.RedactionRule) (bool, error) {
	if len(rules) == 0 {
		return false, nil
	}

	kind := extractString(obj, "kind")
	namespace := extractNestedString(obj, "metadata", "namespace")
	isList := strings.HasSuffix(kind, "List")

	modified := false

	for i := range rules {
		rule := &rules[i]

		if !ruleMatchesKind(rule, kind) {
			continue
		}

		// For list objects, apply rule to each matching item in items[].
		if isList {
			itemsVal, ok := obj["items"]
			if !ok {
				continue
			}
			items, ok := itemsVal.([]interface{})
			if !ok {
				continue
			}
			for j, item := range items {
				itemMap, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				itemKind := extractString(itemMap, "kind")
				if itemKind == "" {
					itemKind = strings.TrimSuffix(kind, "List")
				}
				// Namespace scoping on individual items
				if rule.Namespace != "" {
					itemNS := extractNestedString(itemMap, "metadata", "namespace")
					if itemNS != rule.Namespace {
						continue
					}
				}
				labelsMatch, err := ruleMatchesLabels(rule, extractNestedLabels(itemMap))
				if err != nil {
					return false, err
				}
				if !labelsMatch {
					continue
				}
				changed, err := applyRuleToObj(itemMap, itemKind, rule)
				if err != nil {
					return false, fmt.Errorf("applying rule %q to list item %d: %w", rule.FieldPath, j, err)
				}
				if changed {
					items[j] = itemMap
					modified = true
				}
			}
			if modified {
				obj["items"] = items
			}
			continue
		}

		// Direct object: check namespace scoping
		if rule.Namespace != "" && namespace != "" && namespace != rule.Namespace {
			continue
		}

		labelsMatch, err := ruleMatchesLabels(rule, extractNestedLabels(obj))
		if err != nil {
			return false, err
		}
		if !labelsMatch {
			continue
		}

		changed, err := applyRuleToObj(obj, kind, rule)
		if err != nil {
			return false, fmt.Errorf("applying rule %q: %w", rule.FieldPath, err)
		}
		if changed {
			modified = true
		}
	}

	return modified, nil
}
