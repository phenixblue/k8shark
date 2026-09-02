package anonymize

import (
	"testing"

	"github.com/phenixblue/k8shark/internal/config"
)

func TestNewExcludeMatcher_Validation(t *testing.T) {
	t.Run("exclude=false is rejected", func(t *testing.T) {
		_, err := newExcludeMatcher([]config.AnonymizeRule{
			{Category: "namespace", FieldPath: "metadata.name", Exclude: false},
		})
		if err == nil {
			t.Fatal("want an error for exclude=false")
		}
	})

	t.Run("empty category is rejected", func(t *testing.T) {
		_, err := newExcludeMatcher([]config.AnonymizeRule{
			{FieldPath: "metadata.name", Exclude: true},
		})
		if err == nil {
			t.Fatal("want an error for an empty category")
		}
	})

	t.Run("empty fieldPath is rejected", func(t *testing.T) {
		_, err := newExcludeMatcher([]config.AnonymizeRule{
			{Category: "namespace", Exclude: true},
		})
		if err == nil {
			t.Fatal("want an error for an empty fieldPath")
		}
	})

	t.Run("an unrecognized category is rejected", func(t *testing.T) {
		_, err := newExcludeMatcher([]config.AnonymizeRule{
			{Category: "namespac", FieldPath: "metadata.name", Exclude: true}, // typo
		})
		if err == nil {
			t.Fatal("want an error for a category that could never match a real occurrence")
		}
	})

	t.Run("a fieldPath with a literal array index is rejected", func(t *testing.T) {
		_, err := newExcludeMatcher([]config.AnonymizeRule{
			{Category: "image", FieldPath: "spec.containers[0].image", Exclude: true},
		})
		if err == nil {
			t.Fatal(`want an error for a fieldPath with a literal index instead of "[*]"`)
		}
	})

	t.Run("a fieldPath using the wildcard convention is accepted", func(t *testing.T) {
		_, err := newExcludeMatcher([]config.AnonymizeRule{
			{Category: "image", FieldPath: "spec.containers[*].image", Exclude: true},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("no rules is fine — the resulting func always returns false", func(t *testing.T) {
		excluded, err := newExcludeMatcher(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if excluded(CategoryNamespace, "Pod", "metadata.namespace") {
			t.Error("want false with no rules configured")
		}
	})
}

func TestExcludeMatcher_Matching(t *testing.T) {
	rules := []config.AnonymizeRule{
		{Category: "namespace", Kind: "Pod", FieldPath: "metadata.namespace", Exclude: true},
		{Category: "ip", FieldPath: "status.podIP", Exclude: true}, // no Kind — matches every kind
	}
	excluded, err := newExcludeMatcher(rules)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cases := []struct {
		name     string
		category Category
		kind     string
		path     string
		want     bool
	}{
		{"exact category/kind/path match", CategoryNamespace, "Pod", "metadata.namespace", true},
		{"different kind does not match a kind-scoped rule", CategoryNamespace, "Node", "metadata.namespace", false},
		{"different path does not match", CategoryNamespace, "Pod", "metadata.name", false},
		{"different category does not match", CategoryPod, "Pod", "metadata.namespace", false},
		{"kind-less rule matches any kind", CategoryIP, "Pod", "status.podIP", true},
		{"kind-less rule matches a different kind too", CategoryIP, "Node", "status.podIP", true},
		{"kind-less rule still requires the right path", CategoryIP, "Pod", "status.hostIP", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := excluded(tc.category, tc.kind, tc.path); got != tc.want {
				t.Errorf("excluded(%q, %q, %q) = %v, want %v", tc.category, tc.kind, tc.path, got, tc.want)
			}
		})
	}

	t.Run(`Kind: "*" matches any kind, same as omitting it`, func(t *testing.T) {
		excluded, err := newExcludeMatcher([]config.AnonymizeRule{
			{Category: "workload", Kind: "*", FieldPath: "metadata.name", Exclude: true},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !excluded(CategoryWorkload, "Deployment", "metadata.name") {
			t.Error("want true for Deployment")
		}
		if !excluded(CategoryWorkload, "ReplicaSet", "metadata.name") {
			t.Error("want true for ReplicaSet")
		}
	})
}
