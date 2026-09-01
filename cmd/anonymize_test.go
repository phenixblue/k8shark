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

	t.Run("ip, url, and image are accepted", func(t *testing.T) {
		for cat, want := range map[string]anonymize.Category{
			"ip":    anonymize.CategoryIP,
			"url":   anonymize.CategoryURL,
			"image": anonymize.CategoryImage,
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
		got, err := parseAnonymizeCategories([]string{"namespace", "node", "pod", "workload", "ip", "url", "image"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 7 {
			t.Errorf("got %v, want 7 categories", got)
		}
	})

	// As of M4 (#137), every real Category constant is supported by the
	// archive-rewrite path — there's no longer a real category name left to
	// exercise this with. A made-up category string still needs to be
	// rejected, though.
	t.Run("a made-up category is rejected", func(t *testing.T) {
		if _, err := parseAnonymizeCategories([]string{"bogus"}); err == nil {
			t.Error("want an error for a made-up category")
		}
	})

	t.Run("one bad category in a list rejects the whole list", func(t *testing.T) {
		if _, err := parseAnonymizeCategories([]string{"namespace", "bogus"}); err == nil {
			t.Error("want an error when any requested category is unsupported")
		}
	})
}
