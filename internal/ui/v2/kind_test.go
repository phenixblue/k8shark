package v2

import "testing"

func TestKindFromResource(t *testing.T) {
	cases := map[string]string{
		// Known map entries.
		"pods":              "Pod",
		"configmaps":        "ConfigMap",
		"componentstatuses": "ComponentStatus",
		"endpoints":         "Endpoints",
		"networkpolicies":   "NetworkPolicy",
		// Generic singularization fallback (not in the known map).
		"widgets":  "Widget",  // simple plural
		"gateways": "Gateway", // ...s
		"policies": "Policy",  // ...ies -> y
		"classes":  "Class",   // ...sses -> ...ss
		"foxes":    "Fox",     // ...xes -> ...x  (best-effort; discovery is authoritative)
		"":         "",
	}
	for in, want := range cases {
		if got := kindFromResource(in); got != want {
			t.Errorf("kindFromResource(%q) = %q, want %q", in, got, want)
		}
	}
}
