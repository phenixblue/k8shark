package anonymize

import "fmt"

// collisionTracker wraps a category's alias function with collision
// detection. Two distinct original values landing on the same alias is not
// astronomically unlikely — for a 2-word encoding (4096 combinations, see
// name.go's wordsPerCategory), a cluster with 50 same-category resources has
// roughly a 1-in-4 chance of at least one collision, and 100 roughly 2-in-3,
// by the ordinary birthday bound (a 3-word, 262,144-combination encoding
// pushes that same 100-resource case down to roughly 1-in-53 (~1.9%) — see
// #359, which bumped the namespace category from 2 to 3 words for exactly
// this reason). Silently
// proceeding on a collision is not an acceptable failure mode here: it means
// two distinct source API paths would rewrite to the same destination key,
// and the archive-rewrite loop's index/watch-index rebuild would silently
// let the second one clobber the first — records vanish from the archive
// with no indication anything went wrong. Detected and reported as a hard
// error instead: this milestone does not attempt to resolve a collision
// (e.g. by widening the encoding for just the colliding values), only to
// refuse producing an archive that has one.
type collisionTracker struct {
	category   Category
	alias      func(string) string
	aliasOf    map[string]string // original -> alias (memoizes repeat lookups)
	originalOf map[string]string // alias -> the first original that produced it
	err        error
}

// newCollisionTracker wraps alias (typically an Aliaser's Alias method bound
// to one category) with collision detection. alias is injected rather than
// this type constructing its own Aliaser so it can be tested with a
// deliberately colliding function, without needing to find or brute-force a
// real HMAC collision.
func newCollisionTracker(category Category, alias func(string) string) *collisionTracker {
	return &collisionTracker{
		category:   category,
		alias:      alias,
		aliasOf:    make(map[string]string),
		originalOf: make(map[string]string),
	}
}

// Alias returns the alias for original. Once a collision has been detected,
// it keeps returning a value (never panics) so callers already mid-loop
// don't need their own error-plumbing through every intermediate function —
// but that value must not be trusted or written anywhere durable. Callers
// MUST check Err() and abort before finishing the archive.
func (c *collisionTracker) Alias(original string) string {
	if a, ok := c.aliasOf[original]; ok {
		return a
	}
	a := c.alias(original)
	if prior, ok := c.originalOf[a]; ok && prior != original {
		if c.err == nil {
			c.err = fmt.Errorf(
				"anonymize: alias collision for category %q under this salt: both %q and %q map to %q — use a different --anonymize-salt-file value and try again",
				c.category, prior, original, a)
		}
	} else {
		c.originalOf[a] = original
	}
	c.aliasOf[original] = a
	return a
}

// Err returns the first collision detected, or nil if none was.
func (c *collisionTracker) Err() error { return c.err }

// Count returns the number of distinct original values seen so far —
// exactly the bookkeeping Result's per-category counts need, and a
// byproduct of collision detection rather than separate state to maintain.
func (c *collisionTracker) Count() int { return len(c.aliasOf) }

// Mapping returns a copy of the original-to-alias mapping accumulated so
// far, for Options.EmitMapping's benefit (archive.go) — a copy, not the
// live map, so a caller holding onto it can't observe (or corrupt) this
// tracker's own state as more values are aliased after the copy is taken.
func (c *collisionTracker) Mapping() map[string]string {
	m := make(map[string]string, len(c.aliasOf))
	for k, v := range c.aliasOf {
		m[k] = v
	}
	return m
}
