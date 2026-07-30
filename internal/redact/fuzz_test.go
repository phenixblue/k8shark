package redact

import (
	"encoding/json"
	"testing"

	"github.com/phenixblue/k8shark/internal/config"
)

// FuzzApplyRules fuzzes tokenizePath (exercised internally by ApplyRules'
// per-rule path resolution) together with ApplyRules' traversal itself. A
// panic here aborts a redaction run mid-way, which for a redaction tool
// means a partially-redacted archive that a user may believe is clean —
// ApplyRules must only ever return an error, never panic, regardless of
// path syntax or object shape.
func FuzzApplyRules(f *testing.F) {
	seeds := []struct {
		objJSON, fieldPath, kind, replacement string
	}{
		{`{"kind":"Secret","data":{"password":"hunter2"}}`, "data.password", "Secret", "REDACTED"},
		{`{"kind":"PodList","items":[{"kind":"Pod","spec":{"containers":[{"env":[{"name":"X","value":"secret"}]}]}}]}`,
			"spec.containers[*].env[*].value", "Pod", "REDACTED"},
		{`{"kind":"ConfigMap","data":{"a":{"b":{"c":"v"}}}}`, "**.c", "*", "REDACTED"},
		{`{}`, "", "", ""},
		{`{"a":1}`, "a[0]", "", "x"},
		{`{"a":[1,2,3]}`, "a[-1]", "", "x"},
		{`{"a":1}`, "**", "", "x"},
	}
	for _, s := range seeds {
		f.Add(s.objJSON, s.fieldPath, s.kind, s.replacement)
	}

	f.Fuzz(func(t *testing.T, objJSON, fieldPath, kind, replacement string) {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(objJSON), &obj); err != nil {
			return // ApplyRules' contract is a decoded object; invalid JSON isn't its input
		}
		rule := config.RedactionRule{FieldPath: fieldPath, Kind: kind, Replacement: replacement}
		_, _ = ApplyRules(obj, []config.RedactionRule{rule})
	})
}
