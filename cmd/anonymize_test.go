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

	// This is the one that matters most: node/pod/workload/ip/url/image are
	// all real Category constants that exist in internal/anonymize (later
	// milestones' work), but the archive-rewrite path doesn't support them
	// yet. Accepting them here would let a user believe --categories node
	// did something when it silently anonymized nothing.
	t.Run("a category not yet supported by archive rewriting is rejected", func(t *testing.T) {
		for _, c := range []string{"node", "pod", "workload", "ip", "url", "image", "bogus"} {
			if _, err := parseAnonymizeCategories([]string{c}); err == nil {
				t.Errorf("category %q: want an error, got none", c)
			}
		}
	})

	t.Run("one bad category in a list rejects the whole list", func(t *testing.T) {
		if _, err := parseAnonymizeCategories([]string{"namespace", "node"}); err == nil {
			t.Error("want an error when any requested category is unsupported")
		}
	})
}
