package anonymize

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Every word must independently be a valid DNS-1123 label, since aliasName
// joins them with "-" and relies on the word list alone to guarantee
// validity — this is the test that would catch a future editing mistake
// (a stray space, an uppercase letter, a digit) before it ships as a
// silently-invalid alias.
func TestWordLists_EveryWordIsDNS1123Safe(t *testing.T) {
	for _, list := range map[string][]string{"adjectives": adjectives, "nouns": nouns} {
		for _, w := range list {
			if errs := validation.IsDNS1123Label(w); len(errs) > 0 {
				t.Errorf("word %q is not a valid DNS-1123 label: %v", w, errs)
			}
		}
	}
}

func TestWordLists_NoDuplicates(t *testing.T) {
	for name, list := range map[string][]string{"adjectives": adjectives, "nouns": nouns} {
		seen := make(map[string]bool, len(list))
		for _, w := range list {
			if seen[w] {
				t.Errorf("%s contains duplicate word %q", name, w)
			}
			seen[w] = true
		}
	}
}

// A short list defeats the whole point of adding a word layer on top of the
// raw hash — pin a floor so the list can't quietly shrink back down without
// a test noticing.
func TestWordLists_MinimumSize(t *testing.T) {
	const min = 32
	if len(adjectives) < min {
		t.Errorf("len(adjectives) = %d, want >= %d", len(adjectives), min)
	}
	if len(nouns) < min {
		t.Errorf("len(nouns) = %d, want >= %d", len(nouns), min)
	}
}
