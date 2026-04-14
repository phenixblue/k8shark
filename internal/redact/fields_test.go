package redact

import (
	"encoding/json"
	"testing"

	"github.com/phenixblue/k8shark/internal/config"
)

// helper: decode JSON into map[string]interface{}
func mustDecode(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("mustDecode: %v", err)
	}
	return m
}

// helper: extract a nested string value for test assertions
func getNestedString(t *testing.T, obj map[string]interface{}, keys ...string) string {
	t.Helper()
	var cur interface{} = obj
	for _, k := range keys {
		m, ok := cur.(map[string]interface{})
		if !ok {
			t.Fatalf("getNestedString: expected map at key %q, got %T", k, cur)
		}
		cur = m[k]
	}
	s, ok := cur.(string)
	if !ok {
		t.Fatalf("getNestedString: expected string at %v, got %T (%v)", keys, cur, cur)
	}
	return s
}

// ─── tokenizePath ────────────────────────────────────────────────────────────

func TestTokenizePath_SimpleKeys(t *testing.T) {
	tokens, err := tokenizePath("spec.template.metadata")
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(tokens))
	}
	for i, want := range []string{"spec", "template", "metadata"} {
		if tokens[i].kind != tokenKey || tokens[i].key != want {
			t.Errorf("token[%d]: want key %q, got %+v", i, want, tokens[i])
		}
	}
}

func TestTokenizePath_ArrayWildcard(t *testing.T) {
	tokens, err := tokenizePath("spec.containers[*].env[*].value")
	if err != nil {
		t.Fatal(err)
	}
	wantKinds := []tokenKind{tokenKey, tokenKey, tokenWildcard, tokenKey, tokenWildcard, tokenKey}
	if len(tokens) != len(wantKinds) {
		t.Fatalf("expected %d tokens, got %d: %+v", len(wantKinds), len(tokens), tokens)
	}
	for i, wk := range wantKinds {
		if tokens[i].kind != wk {
			t.Errorf("token[%d]: want kind %d, got %d", i, wk, tokens[i].kind)
		}
	}
}

func TestTokenizePath_RecursiveDescent(t *testing.T) {
	tokens, err := tokenizePath("**.password")
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %d: %+v", len(tokens), tokens)
	}
	if tokens[0].kind != tokenRecursive {
		t.Errorf("token[0]: want recursive, got %d", tokens[0].kind)
	}
	if tokens[1].kind != tokenKey || tokens[1].key != "password" {
		t.Errorf("token[1]: want key 'password', got %+v", tokens[1])
	}
}

func TestTokenizePath_NumericIndex(t *testing.T) {
	tokens, err := tokenizePath("items[0].name")
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens: %+v", tokens)
	}
	if tokens[1].kind != tokenIndex || tokens[1].index != 0 {
		t.Errorf("expected index token with index=0, got %+v", tokens[1])
	}
}

// ─── typedReplacement / convertReplacement ────────────────────────────────────

func TestTypedReplacement_StringDefault(t *testing.T) {
	rule := &config.RedactionRule{Replacement: "REDACTED"}
	got, err := typedReplacement("original", rule)
	if err != nil {
		t.Fatal(err)
	}
	if got != "REDACTED" {
		t.Errorf("want REDACTED, got %v", got)
	}
}

func TestTypedReplacement_NumberInference(t *testing.T) {
	rule := &config.RedactionRule{Replacement: "0"}
	got, err := typedReplacement(float64(42), rule)
	if err != nil {
		t.Fatal(err)
	}
	if got != float64(0) {
		t.Errorf("want 0.0, got %v (%T)", got, got)
	}
}

func TestTypedReplacement_BoolInference(t *testing.T) {
	rule := &config.RedactionRule{Replacement: "false"}
	got, err := typedReplacement(true, rule)
	if err != nil {
		t.Fatal(err)
	}
	if got != false {
		t.Errorf("want false, got %v", got)
	}
}

func TestTypedReplacement_ExplicitIntegerHint(t *testing.T) {
	rule := &config.RedactionRule{Replacement: "99", ValueType: "integer"}
	got, err := typedReplacement("ignored", rule)
	if err != nil {
		t.Fatal(err)
	}
	if got != float64(99) {
		t.Errorf("want 99.0, got %v", got)
	}
}

func TestTypedReplacement_ExplicitBoolHint(t *testing.T) {
	rule := &config.RedactionRule{Replacement: "False", ValueType: "bool"}
	got, err := typedReplacement("ignored", rule)
	if err != nil {
		t.Fatal(err)
	}
	if got != false {
		t.Errorf("want false, got %v", got)
	}
}

func TestTypedReplacement_ArrayHint(t *testing.T) {
	rule := &config.RedactionRule{Replacement: "ignored", ValueType: "array"}
	got, err := typedReplacement([]interface{}{"a", "b"}, rule)
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := got.([]interface{})
	if !ok || len(arr) != 0 {
		t.Errorf("expected empty array, got %v", got)
	}
}

func TestTypedReplacement_InvalidIntHint(t *testing.T) {
	rule := &config.RedactionRule{Replacement: "not-a-number", ValueType: "integer"}
	_, err := typedReplacement("x", rule)
	if err == nil {
		t.Error("expected error for non-numeric replacement with integer hint")
	}
}

// ─── applyRuleToObj ──────────────────────────────────────────────────────────

func TestApplyRule_ExactPath(t *testing.T) {
	obj := mustDecode(t, `{"data":{"api-key":"secret123"}}`)
	rule := &config.RedactionRule{FieldPath: "data.api-key", Replacement: "REDACTED"}
	changed, err := applyRuleToObj(obj, rule)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected change")
	}
	if got := getNestedString(t, obj, "data", "api-key"); got != "REDACTED" {
		t.Errorf("want REDACTED, got %q", got)
	}
}

func TestApplyRule_ArrayWildcard(t *testing.T) {
	obj := mustDecode(t, `{
		"spec": {
			"containers": [
				{"env": [{"name":"FOO","value":"secret1"},{"name":"BAR","value":"secret2"}]},
				{"env": [{"name":"BAZ","value":"secret3"}]}
			]
		}
	}`)
	rule := &config.RedactionRule{
		FieldPath:   "spec.containers[*].env[*].value",
		Replacement: "REDACTED",
	}
	changed, err := applyRuleToObj(obj, rule)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected change")
	}
	// Check all three env values were redacted
	spec := obj["spec"].(map[string]interface{})
	containers := spec["containers"].([]interface{})
	for ci, c := range containers {
		cm := c.(map[string]interface{})
		envs := cm["env"].([]interface{})
		for ei, e := range envs {
			em := e.(map[string]interface{})
			if em["value"] != "REDACTED" {
				t.Errorf("containers[%d].env[%d].value not redacted: %v", ci, ei, em["value"])
			}
		}
	}
}

func TestApplyRule_RecursiveDescent(t *testing.T) {
	obj := mustDecode(t, `{
		"spec": {
			"password": "top-secret",
			"nested": {
				"password": "also-secret",
				"other": "keep-me"
			}
		}
	}`)
	rule := &config.RedactionRule{FieldPath: "**.password", Replacement: "REDACTED"}
	changed, err := applyRuleToObj(obj, rule)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected change")
	}
	spec := obj["spec"].(map[string]interface{})
	if spec["password"] != "REDACTED" {
		t.Errorf("top-level password not redacted: %v", spec["password"])
	}
	nested := spec["nested"].(map[string]interface{})
	if nested["password"] != "REDACTED" {
		t.Errorf("nested password not redacted: %v", nested["password"])
	}
	if nested["other"] != "keep-me" {
		t.Errorf("non-password field was modified: %v", nested["other"])
	}
}

func TestApplyRule_NonExistentPath_NoChange(t *testing.T) {
	obj := mustDecode(t, `{"spec":{"containers":[]}}`)
	rule := &config.RedactionRule{FieldPath: "spec.nonexistent.field", Replacement: "REDACTED"}
	changed, err := applyRuleToObj(obj, rule)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("expected no change for non-existent path")
	}
}

func TestApplyRule_MaintainsNumberType(t *testing.T) {
	obj := mustDecode(t, `{"spec":{"replicas":3}}`)
	rule := &config.RedactionRule{FieldPath: "spec.replicas", Replacement: "0"}
	changed, err := applyRuleToObj(obj, rule)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected change")
	}
	spec := obj["spec"].(map[string]interface{})
	if spec["replicas"] != float64(0) {
		t.Errorf("expected float64(0), got %v (%T)", spec["replicas"], spec["replicas"])
	}
}

func TestApplyRule_MaintainsBoolType(t *testing.T) {
	obj := mustDecode(t, `{"spec":{"automountServiceAccountToken":true}}`)
	rule := &config.RedactionRule{FieldPath: "spec.automountServiceAccountToken", Replacement: "false"}
	changed, err := applyRuleToObj(obj, rule)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected change")
	}
	spec := obj["spec"].(map[string]interface{})
	if spec["automountServiceAccountToken"] != false {
		t.Errorf("expected false, got %v", spec["automountServiceAccountToken"])
	}
}

// ─── ApplyRules (kind/namespace scoping) ─────────────────────────────────────

func TestApplyRules_KindScoping_Matches(t *testing.T) {
	obj := mustDecode(t, `{
		"kind":"ConfigMap",
		"metadata":{"name":"app-config","namespace":"default"},
		"data":{"api-key":"secret"}
	}`)
	rules := []config.RedactionRule{
		{FieldPath: "data.api-key", Kind: "ConfigMap", Replacement: "REDACTED"},
	}
	changed, err := ApplyRules(obj, rules)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected change")
	}
	data := obj["data"].(map[string]interface{})
	if data["api-key"] != "REDACTED" {
		t.Errorf("expected REDACTED, got %v", data["api-key"])
	}
}

func TestApplyRules_KindScoping_NoMatch(t *testing.T) {
	obj := mustDecode(t, `{
		"kind":"Secret",
		"metadata":{"name":"mysecret","namespace":"default"},
		"data":{"api-key":"secret"}
	}`)
	rules := []config.RedactionRule{
		{FieldPath: "data.api-key", Kind: "ConfigMap", Replacement: "REDACTED"},
	}
	changed, err := ApplyRules(obj, rules)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("rule scoped to ConfigMap should not match Secret")
	}
}

func TestApplyRules_WildcardKind_MatchesAll(t *testing.T) {
	obj := mustDecode(t, `{
		"kind":"Deployment",
		"metadata":{"name":"app","namespace":"default"},
		"spec":{"password":"hunter2"}
	}`)
	rules := []config.RedactionRule{
		{FieldPath: "spec.password", Kind: "*", Replacement: "REDACTED"},
	}
	changed, err := ApplyRules(obj, rules)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("wildcard kind should match Deployment")
	}
}

func TestApplyRules_NamespaceScoping_Matches(t *testing.T) {
	obj := mustDecode(t, `{
		"kind":"ConfigMap",
		"metadata":{"name":"app","namespace":"production"},
		"data":{"api-key":"secret"}
	}`)
	rules := []config.RedactionRule{
		{FieldPath: "data.api-key", Kind: "ConfigMap", Namespace: "production", Replacement: "REDACTED"},
	}
	changed, err := ApplyRules(obj, rules)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected change for matching namespace")
	}
}

func TestApplyRules_NamespaceScoping_NoMatch(t *testing.T) {
	obj := mustDecode(t, `{
		"kind":"ConfigMap",
		"metadata":{"name":"app","namespace":"staging"},
		"data":{"api-key":"secret"}
	}`)
	rules := []config.RedactionRule{
		{FieldPath: "data.api-key", Kind: "ConfigMap", Namespace: "production", Replacement: "REDACTED"},
	}
	changed, err := ApplyRules(obj, rules)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("rule scoped to production should not match staging namespace")
	}
}

func TestApplyRules_ListResponse_RedactsItems(t *testing.T) {
	obj := mustDecode(t, `{
		"kind":"PodList",
		"items":[
			{
				"kind":"Pod",
				"metadata":{"name":"p1","namespace":"default"},
				"spec":{"containers":[{"env":[{"name":"TOKEN","value":"secret1"}]}]}
			},
			{
				"kind":"Pod",
				"metadata":{"name":"p2","namespace":"default"},
				"spec":{"containers":[{"env":[{"name":"TOKEN","value":"secret2"}]}]}
			}
		]
	}`)
	rules := []config.RedactionRule{
		{
			FieldPath:   "spec.containers[*].env[*].value",
			Kind:        "Pod",
			Replacement: "REDACTED",
		},
	}
	changed, err := ApplyRules(obj, rules)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected change for PodList items")
	}
	items := obj["items"].([]interface{})
	for i, item := range items {
		pod := item.(map[string]interface{})
		spec := pod["spec"].(map[string]interface{})
		containers := spec["containers"].([]interface{})
		for _, c := range containers {
			cm := c.(map[string]interface{})
			envs := cm["env"].([]interface{})
			for _, e := range envs {
				em := e.(map[string]interface{})
				if em["value"] != "REDACTED" {
					t.Errorf("items[%d] env value not redacted: %v", i, em["value"])
				}
			}
		}
	}
}

func TestApplyRules_ListResponse_NamespaceScoping(t *testing.T) {
	obj := mustDecode(t, `{
		"kind":"ConfigMapList",
		"items":[
			{"kind":"ConfigMap","metadata":{"name":"cm1","namespace":"production"},"data":{"key":"secret"}},
			{"kind":"ConfigMap","metadata":{"name":"cm2","namespace":"staging"},"data":{"key":"keep-me"}}
		]
	}`)
	rules := []config.RedactionRule{
		{FieldPath: "data.key", Kind: "ConfigMap", Namespace: "production", Replacement: "REDACTED"},
	}
	changed, err := ApplyRules(obj, rules)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected change")
	}
	items := obj["items"].([]interface{})
	cm1 := items[0].(map[string]interface{})["data"].(map[string]interface{})
	cm2 := items[1].(map[string]interface{})["data"].(map[string]interface{})
	if cm1["key"] != "REDACTED" {
		t.Errorf("production cm not redacted: %v", cm1["key"])
	}
	if cm2["key"] != "keep-me" {
		t.Errorf("staging cm should not be redacted: %v", cm2["key"])
	}
}

func TestApplyRules_EmptyRules_NoChange(t *testing.T) {
	obj := mustDecode(t, `{"kind":"Pod","data":{"key":"value"}}`)
	changed, err := ApplyRules(obj, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("empty rules should produce no change")
	}
}
