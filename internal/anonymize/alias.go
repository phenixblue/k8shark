package anonymize

import (
	"crypto/hmac"
	"crypto/sha256"
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
// visits records via `for apiPath, entry := range idx`, and Go's map
// iteration order is randomized per process. That is harmless for redact
// today because every field-redact replacement is a fixed constant, but it
// would silently break anonymize's own stated requirement — "same input +
// same salt ⇒ same output, so re-runs and diffs are stable" — because two
// runs over the identical archive could assign different aliases to the
// same real value depending on which run's random map order happened to
// visit it first. A pure function of (category, value, salt) has no order
// to depend on, so this can't happen.
//
// It also gets consistency across the regular index and the watch index for
// free: redact.Archive walks those in two separate loops (redact.go:96 and
// redact.go:139). Because Alias holds no state, both loops independently
// compute the identical alias for the same value without needing to
// coordinate with each other.
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

// Alias returns the deterministic, category-appropriate replacement for
// original. The returned value is always structurally valid for its
// category (a parseable IP of the same family as original; a DNS-1123-safe
// label otherwise) — see ip.go and name.go for the per-category encoders.
func (a *Aliaser) Alias(category Category, original string) string {
	digest := a.digest(category, original)
	if category == CategoryIP {
		return aliasIP(digest, original)
	}
	return aliasName(category, digest)
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
