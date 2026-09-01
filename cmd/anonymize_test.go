package cmd

import (
	"testing"

	"github.com/phenixblue/k8shark/internal/anonymize"
)

func TestParseAnonymizeCategories(t *testing.T) {
	t.Run("namespace is accepted", func(t *testing.T) {
		got, err := parseAnonymizeCategories([]string{"namespace"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != anonymize.CategoryNamespace {
			t.Errorf("got %v, want [namespace]", got)
		}
	})

	t.Run("case and whitespace are normalized", func(t *testing.T) {
		got, err := parseAnonymizeCategories([]string{"  Namespace  "})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 || got[0] != anonymize.CategoryNamespace {
			t.Errorf("got %v, want [namespace]", got)
		}
	})

	t.Run("empty is rejected — --categories is required", func(t *testing.T) {
		if _, err := parseAnonymizeCategories(nil); err == nil {
			t.Error("want an error when no categories are given")
		}
	})

	t.Run("node, pod, and workload are accepted", func(t *testing.T) {
		for cat, want := range map[string]anonymize.Category{
			"node":     anonymize.CategoryNode,
			"pod":      anonymize.CategoryPod,
			"workload": anonymize.CategoryWorkload,
		} {
			got, err := parseAnonymizeCategories([]string{cat})
			if err != nil {
				t.Errorf("category %q: unexpected error: %v", cat, err)
				continue
			}
			if len(got) != 1 || got[0] != want {
				t.Errorf("category %q: got %v, want [%v]", cat, got, want)
			}
		}
	})

	t.Run("multiple supported categories in one call are all accepted", func(t *testing.T) {
		got, err := parseAnonymizeCategories([]string{"namespace", "node", "pod", "workload"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 4 {
			t.Errorf("got %v, want 4 categories", got)
		}
	})

	// This is the one that matters most: ip/url/image are real Category
	// constants that exist in internal/anonymize (later milestones' work),
	// but the archive-rewrite path doesn't support them yet. Accepting them
	// here would let a user believe --categories ip did something when it
	// silently anonymized nothing.
	t.Run("a category not yet supported by archive rewriting is rejected", func(t *testing.T) {
		for _, c := range []string{"ip", "url", "image", "bogus"} {
			if _, err := parseAnonymizeCategories([]string{c}); err == nil {
				t.Errorf("category %q: want an error, got none", c)
			}
		}
	})

	t.Run("one bad category in a list rejects the whole list", func(t *testing.T) {
		if _, err := parseAnonymizeCategories([]string{"namespace", "ip"}); err == nil {
			t.Error("want an error when any requested category is unsupported")
		}
	})
}
