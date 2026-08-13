package anonymize

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
)

// Aliaser deterministically maps an original value to a stable, structurally
// valid replacement, given a fixed salt: Alias(cat, salt, x) always returns
// the same string for the same (cat, salt, x).
//
// That determinism is the whole point of this package, and it is achieved by
// construction rather than by bookkeeping: Alias is a pure function with no
// shared mutable state across calls. That is deliberate, not an
// implementation shortcut.
//
// The alternative — assign "the next sequential alias" the first time a
// value is seen while walking an archive's records — would depend on the
// order records are visited in. redact.Archive (internal/redact/redact.go)
// visits every record in the archive by ranging directly over the decoded
// index map, and Go's map iteration order is randomized per process. That is
// harmless for redact today because every field-redact replacement is a
// fixed constant, but it would silently break anonymize's own stated
// requirement — "same input + same salt ⇒ same output, so re-runs and diffs
// are stable" — because two runs over the identical archive could assign
// different aliases to the same real value depending on which run's random
// map order happened to visit it first. A pure function of (category,
// value, salt) has no order to depend on, so this can't happen.
//
// It also gets consistency across the regular index and the watch index for
// free: redact.Archive walks those via two separate loops, one per index.
// Because Alias holds no state, both loops independently compute the
// identical alias for the same value without needing to coordinate with
// each other.
type Aliaser struct {
	salt []byte
}

// NewAliaser returns an Aliaser using salt. salt should be resolved by the
// caller (CLI flag, file, or a freshly generated value that gets echoed back
// to the user) — this package never generates, stores, or reads a salt from
// anywhere on its own, precisely so it composes safely with however the
// caller chooses to source one.
func NewAliaser(salt []byte) *Aliaser {
	return &Aliaser{salt: append([]byte(nil), salt...)}
}

// implementedCategories are the categories Alias currently knows how to
// render. Deliberately explicit and exhaustive rather than "everything
// except IP falls through to aliasName": CategoryURL and CategoryImage are
// real constants (used by later milestones' matchers once those exist), but
// their encoders don't exist yet — URL/hostname aliasing needs substring
// splicing, and image aliasing needs registry-host-only rewriting, neither
// of which is word-based name aliasing. Letting them silently render via
// aliasName today would lock in an accidental encoding nobody decided on,
// for categories whose real encoding is different milestones' work. Alias
// panics for anything not listed here so that can't happen unnoticed;
// TestAliaser_ImplementedCategoriesMatchDispatch pins this list against
// Alias's own switch so the two can't drift apart.
var implementedCategories = map[Category]bool{
	CategoryIP:        true,
	CategoryNode:      true,
	CategoryNamespace: true,
	CategoryPod:       true,
	CategoryWorkload:  true,
}

// Alias returns the deterministic, category-appropriate replacement for
// original. The returned value is always structurally valid for its
// category (a parseable IP of the same family as original; a DNS-1123-safe
// label otherwise) — see ip.go and name.go for the per-category encoders.
//
// Panics if category is not yet implemented (see implementedCategories) —
// this is an internal package with no callers yet, so a loud failure now is
// strictly better than a silently-wrong render that a later milestone would
// have to notice and unwind.
func (a *Aliaser) Alias(category Category, original string) string {
	if !implementedCategories[category] {
		panic(fmt.Sprintf("anonymize: category %q has no encoder yet (see implementedCategories)", category))
	}
	digest := a.digest(category, original)
	switch category {
	case CategoryIP:
		return aliasIP(digest, original)
	case CategoryNode, CategoryNamespace, CategoryPod, CategoryWorkload:
		return aliasName(category, digest)
	default:
		// Unreachable: implementedCategories and this switch are kept in
		// sync by TestAliaser_ImplementedCategoriesMatchDispatch.
		panic(fmt.Sprintf("anonymize: category %q is in implementedCategories but has no case in Alias's switch", category))
	}
}

// digest returns a value's category-scoped HMAC-SHA256 digest under this
// Aliaser's salt. Mixing the category into the MAC input (not just into the
// rendering afterward) means a hash collision between categories would
// require breaking HMAC-SHA256 itself, not just an unlucky rendering choice
// — though in practice categories can't collide as *strings* anyway, since
// each renders into its own disjoint space (see alias_test.go).
func (a *Aliaser) digest(category Category, original string) []byte {
	mac := hmac.New(sha256.New, a.salt)
	mac.Write([]byte(category))
	mac.Write([]byte(":"))
	mac.Write([]byte(original))
	return mac.Sum(nil)
}
