package anonymize

import (
	"strings"
	"testing"
)

func TestCollisionTracker_NoCollision(t *testing.T) {
	tr := newCollisionTracker(CategoryNamespace, upper) // upper: s -> s+"-ALIASED", injective
	if got := tr.Alias("prod"); got != "prod-ALIASED" {
		t.Errorf("Alias(prod) = %q, want prod-ALIASED", got)
	}
	if got := tr.Alias("staging"); got != "staging-ALIASED" {
		t.Errorf("Alias(staging) = %q, want staging-ALIASED", got)
	}
	if err := tr.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
	if got := tr.Count(); got != 2 {
		t.Errorf("Count() = %d, want 2", got)
	}
}

func TestCollisionTracker_RepeatedCallSameOriginal_NotACollision(t *testing.T) {
	tr := newCollisionTracker(CategoryNamespace, upper)
	first := tr.Alias("prod")
	second := tr.Alias("prod")
	third := tr.Alias("prod")
	if first != second || second != third {
		t.Errorf("repeated calls for the same original returned different aliases: %q, %q, %q", first, second, third)
	}
	if err := tr.Err(); err != nil {
		t.Errorf("Err() = %v, want nil — the same value seen repeatedly is not a collision", err)
	}
	if got := tr.Count(); got != 1 {
		t.Errorf("Count() = %d, want 1 (one distinct original, however many times it was looked up)", got)
	}
}

// The central case: two DISTINCT original values that happen to map to the
// same alias (a deliberately collision-prone injected function, since
// finding two real strings that collide under the real HMAC-based Aliaser
// isn't practical to do in a unit test) must be detected and reported, not
// silently allowed through.
func TestCollisionTracker_DetectsCollision(t *testing.T) {
	alwaysSame := func(string) string { return "namespace-same-alias" }
	tr := newCollisionTracker(CategoryNamespace, alwaysSame)

	if got := tr.Alias("prod"); got != "namespace-same-alias" {
		t.Fatalf("Alias(prod) = %q", got)
	}
	if err := tr.Err(); err != nil {
		t.Fatalf("Err() after the first distinct value = %v, want nil", err)
	}

	tr.Alias("staging") // a second, distinct original -> the same alias
	err := tr.Err()
	if err == nil {
		t.Fatal("want a collision error after two distinct originals map to the same alias")
	}
	// The error must name both real values and the category — this is what
	// makes it actionable rather than a generic "something collided".
	for _, want := range []string{"prod", "staging", "namespace-same-alias", string(CategoryNamespace)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// Once a collision is detected, Err() must keep reporting it (a caller might
// check it more than once, e.g. once per loop iteration as archive.go does)
// and it must not be overwritten by a later, different collision — the
// first one found is the one reported.
func TestCollisionTracker_ErrSticksToTheFirstCollision(t *testing.T) {
	alwaysSame := func(string) string { return "same-alias" }
	tr := newCollisionTracker(CategoryNamespace, alwaysSame)
	tr.Alias("a")
	tr.Alias("b") // first collision: a vs b
	first := tr.Err()
	tr.Alias("c") // also collides, but should not replace the first error
	second := tr.Err()
	if first == nil {
		t.Fatal("want a collision error after the second distinct value")
	}
	if first != second {
		t.Errorf("Err() changed after a second collision: %v -> %v, want it to stay the same", first, second)
	}
}
