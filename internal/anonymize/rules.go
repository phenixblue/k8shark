package anonymize

import (
	"fmt"

	"github.com/phenixblue/k8shark/internal/config"
)

// excludedFunc reports whether a specific occurrence — a value of category,
// found on an object of the given kind, at the given field path — should be
// left alone rather than aliased. Every rewrite call site in this package
// that's about to write an alias checks this first; a nil excludedFunc (the
// common case, when Options.Rules is empty) always returns false via
// newExcludeMatcher's own zero-rule fast path, so no call site needs its
// own nil check.
type excludedFunc func(category Category, kind, path string) bool

// newExcludeMatcher builds an excludedFunc from rules. Every rule must have
// Exclude set: anonymize rules are exclusions only — a value's category
// membership is already determined automatically by where and how it
// occurs, so there is no separate "include" action for a rule to request.
// Category and FieldPath are required; Kind is optional ("" or "*" matches
// every kind), mirroring RedactionRule.Kind's own convention.
func newExcludeMatcher(rules []config.AnonymizeRule) (excludedFunc, error) {
	for i, r := range rules {
		if !r.Exclude {
			return nil, fmt.Errorf("anonymize: rule %d (category %q, fieldPath %q): exclude must be true — anonymize rules are exclusions only", i, r.Category, r.FieldPath)
		}
		if r.Category == "" {
			return nil, fmt.Errorf("anonymize: rule %d: category is required", i)
		}
		if r.FieldPath == "" {
			return nil, fmt.Errorf("anonymize: rule %d (category %q): fieldPath is required", i, r.Category)
		}
	}
	if len(rules) == 0 {
		return func(Category, string, string) bool { return false }, nil
	}
	return func(category Category, kind, path string) bool {
		for _, r := range rules {
			if Category(r.Category) != category {
				continue
			}
			if r.Kind != "" && r.Kind != "*" && r.Kind != kind {
				continue
			}
			if r.FieldPath != path {
				continue
			}
			return true
		}
		return false
	}, nil
}
