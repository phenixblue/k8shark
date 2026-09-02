package anonymize

import (
	"fmt"
	"regexp"

	"github.com/phenixblue/k8shark/internal/config"
)

// fieldPathBracketPattern matches any "[...]" segment of a rule's
// FieldPath, so newExcludeMatcher can reject one that isn't exactly "[*]"
// — this package's field-path convention (walk.go, resourcename.go, and
// every other rewrite site that computes a path to check against
// excludedFunc) always writes an array index as the wildcard "[*]", never
// a literal index. A rule written as "spec.containers[0].image" would
// never match anything a real occurrence's computed path could produce —
// a silent, hard-to-debug no-op exclusion rather than a loud error.
var fieldPathBracketPattern = regexp.MustCompile(`\[[^\]]*\]`)

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
//
// Beyond required-ness, Category and FieldPath are validated for whether
// the rule could ever actually match anything: an unrecognized category
// never matches any real occurrence (every category this package can find
// is listed in archiveCategories, archive.go), and a literal array index in
// FieldPath never matches this package's own "[*]"-only convention for a
// computed path. Both are rejected up front rather than accepted as a
// silently-useless rule — the same reasoning collision detection already
// uses for a different kind of "wrong but not caught" failure mode.
func newExcludeMatcher(rules []config.AnonymizeRule) (excludedFunc, error) {
	for i, r := range rules {
		if !r.Exclude {
			return nil, fmt.Errorf("anonymize: rule %d (category %q, fieldPath %q): exclude must be true — anonymize rules are exclusions only", i, r.Category, r.FieldPath)
		}
		if r.Category == "" {
			return nil, fmt.Errorf("anonymize: rule %d: category is required", i)
		}
		if !archiveCategories[Category(r.Category)] {
			return nil, fmt.Errorf("anonymize: rule %d: category %q is not a supported category — it could never match a real occurrence", i, r.Category)
		}
		if r.FieldPath == "" {
			return nil, fmt.Errorf("anonymize: rule %d (category %q): fieldPath is required", i, r.Category)
		}
		for _, seg := range fieldPathBracketPattern.FindAllString(r.FieldPath, -1) {
			if seg != "[*]" {
				return nil, fmt.Errorf(`anonymize: rule %d (category %q): fieldPath %q must use "[*]" for every array index, not a literal index like %q — a computed occurrence path never contains one`, i, r.Category, r.FieldPath, seg)
			}
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
