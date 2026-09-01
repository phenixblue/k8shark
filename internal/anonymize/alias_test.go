package anonymize

import (
	"fmt"
	"math/rand"
	"net"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// The central requirement: same (category, salt, original) always yields the
// same alias, run after run, process after process. This is the property the
// whole single-pass design exists to guarantee without any shared state.
func TestAliaser_Deterministic(t *testing.T) {
	salt := []byte("test-salt")
	cases := []struct {
		category Category
		value    string
	}{
		{CategoryIP, "10.244.0.5"},
		{CategoryIP, "2001:db8::1"},
		{CategoryNode, "worker-node-3"},
		{CategoryNamespace, "production"},
		{CategoryPod, "widget-controller-7f9647f7b6-q6vh8"},
		{CategoryWorkload, "widget-controller"},
	}
	for _, tc := range cases {
		t.Run(string(tc.category)+"/"+tc.value, func(t *testing.T) {
			a1 := NewAliaser(salt)
			a2 := NewAliaser(salt) // a fresh Aliaser, as a new process would construct
			got1 := a1.Alias(tc.category, tc.value)
			got2 := a2.Alias(tc.category, tc.value)
			if got1 != got2 {
				t.Errorf("nondeterministic: %q vs %q", got1, got2)
			}
			// And stable across repeated calls on the same Aliaser too —
			// Alias must not mutate any state that would change its own
			// next answer.
			got3 := a1.Alias(tc.category, tc.value)
			if got3 != got1 {
				t.Errorf("Alias is not idempotent across repeated calls on the same Aliaser: %q then %q", got1, got3)
			}
		})
	}
}

// A different salt must (with overwhelming probability) produce a different
// alias — otherwise "salt" wouldn't be doing anything, which is exactly the
// failure mode Open Question 1 in the design contrasts against a
// salt-independent sorted-counter scheme.
func TestAliaser_DifferentSaltDifferentAlias(t *testing.T) {
	a1 := NewAliaser([]byte("salt-one"))
	a2 := NewAliaser([]byte("salt-two"))
	got1 := a1.Alias(CategoryNode, "worker-node-3")
	got2 := a2.Alias(CategoryNode, "worker-node-3")
	if got1 == got2 {
		t.Errorf("two different salts produced the same alias %q for the same input", got1)
	}
}

// Different original values under the same salt+category should (almost
// always) diverge. A property test over a decent sample size rather than a
// hard uniqueness proof, since collisions are possible in principle (see
// name.go's wordsPerCategory comment) — this pins the *rate*, not a
// guarantee, so a real regression (e.g. an encoder that ignores most of the
// input) would show up as a large collision count, not just one.
func TestAliaser_DistinctInputsMostlyDistinctAliases(t *testing.T) {
	salt := []byte("distinctness-salt")
	a := NewAliaser(salt)
	r := rand.New(rand.NewSource(42)) // fixed seed: reproducible, not security-sensitive

	const n = 500
	seen := make(map[string]string, n) // alias -> first original that produced it
	collisions := 0
	for i := 0; i < n; i++ {
		original := fmt.Sprintf("node-%d-%d", i, r.Int())
		alias := a.Alias(CategoryNode, original)
		if prior, ok := seen[alias]; ok && prior != original {
			collisions++
		}
		seen[alias] = original
	}
	// 2-word node aliases have a 64*64 = 4096-combination space; by the
	// birthday bound, 500 draws should produce on the order of tens of
	// collisions, not hundreds. A generous ceiling — this test exists to
	// catch a broken encoder (e.g. one that collapses to a handful of
	// outputs), not to police the exact birthday-bound math.
	const maxAcceptableCollisions = 100
	if collisions > maxAcceptableCollisions {
		t.Errorf("got %d collisions across %d distinct inputs, want <= %d — the encoder may not be using enough of the digest",
			collisions, n, maxAcceptableCollisions)
	}
}

// Two different categories must never render into the same string, for any
// input — not as a probabilistic property, but structurally: each category
// renders into its own disjoint space (an IP literal vs. "<category>-word-
// word"), so this holds even if the underlying digests happened to collide.
func TestAliaser_CategoriesNeverCollide(t *testing.T) {
	a := NewAliaser([]byte("cross-category-salt"))
	values := []string{"shared-value", "10.0.0.1", "prod", "worker-1"}

	// implementedCategories, not AllCategories: AllCategories is the eventual
	// full set (#137's design target), and Alias panics on the categories in
	// it that aren't implemented yet — see TestAliaser_PanicsOnUnimplementedCategory.
	for _, v := range values {
		byCategory := make(map[Category]string)
		for cat := range implementedCategories {
			byCategory[cat] = a.Alias(cat, v)
		}
		seen := make(map[string]Category, len(byCategory))
		for cat, alias := range byCategory {
			if other, ok := seen[alias]; ok {
				t.Errorf("value %q: categories %q and %q both produced alias %q", v, other, cat, alias)
			}
			seen[alias] = cat
		}
	}
}

// aliasName's output must always be a valid DNS-1123 label — this is a
// consequence of the word list being pre-validated (wordlist_test.go), but
// pin it at the Alias level too, since that's the contract callers actually
// depend on.
func TestAliaser_NameAliasesAreDNS1123Safe(t *testing.T) {
	a := NewAliaser([]byte("name-salt"))
	for _, cat := range []Category{CategoryNode, CategoryNamespace, CategoryPod, CategoryWorkload} {
		for i := 0; i < 50; i++ {
			original := fmt.Sprintf("%s-original-%d", cat, i)
			alias := a.Alias(cat, original)
			if errs := validation.IsDNS1123Label(alias); len(errs) > 0 {
				t.Errorf("%s alias %q for input %q is not a valid DNS-1123 label: %v", cat, alias, original, errs)
			}
			if got := alias[:len(string(cat))]; got != string(cat) {
				t.Errorf("%s alias %q does not start with the category prefix", cat, alias)
			}
		}
	}
}

// IP aliases must parse back as valid IPs, land in the intended private
// range, and preserve the input's address family.
func TestAliaser_IPAliasesAreValidAndPrivate(t *testing.T) {
	_, v4Net, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	_, v6Net, err := net.ParseCIDR("fd00::/8")
	if err != nil {
		t.Fatal(err)
	}

	a := NewAliaser([]byte("ip-salt"))
	cases := []struct {
		original string
		wantV6   bool
	}{
		{"192.168.1.1", false},
		{"172.16.5.9", false},
		{"8.8.8.8", false},
		{"2001:db8::1", true},
		{"fe80::1", true},
	}
	for _, tc := range cases {
		t.Run(tc.original, func(t *testing.T) {
			alias := a.Alias(CategoryIP, tc.original)
			parsed := net.ParseIP(alias)
			if parsed == nil {
				t.Fatalf("alias %q for %q does not parse as an IP", alias, tc.original)
			}
			isV4 := parsed.To4() != nil
			if isV4 == tc.wantV6 {
				t.Errorf("alias %q for %q has the wrong address family (isV4=%v, wantV6=%v)", alias, tc.original, isV4, tc.wantV6)
			}
			if isV4 {
				if !v4Net.Contains(parsed) {
					t.Errorf("IPv4 alias %q for %q is not in %s", alias, tc.original, v4Net)
				}
			} else {
				if !v6Net.Contains(parsed) {
					t.Errorf("IPv6 alias %q for %q is not in %s", alias, tc.original, v6Net)
				}
			}
		})
	}
}

// Calling Alias with a category that isn't implemented at all must panic,
// not silently render something through the wrong encoder.
func TestAliaser_PanicsOnUnimplementedCategory(t *testing.T) {
	a := NewAliaser([]byte("panic-salt"))
	for _, cat := range []Category{Category("bogus")} {
		t.Run(string(cat), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("Alias(%q, ...) did not panic", cat)
				}
			}()
			a.Alias(cat, "some-value")
		})
	}
}

// implementedCategories (used by Alias's guard) and the case list in Alias's
// own switch must agree exactly — pinned here so the two can't quietly drift
// apart (a category added to one without the other would either panic
// despite being "implemented," or silently reach the switch's unreachable
// default panic branch instead of the clearer guard message).
func TestAliaser_ImplementedCategoriesMatchDispatch(t *testing.T) {
	a := NewAliaser([]byte("dispatch-sync-salt"))
	for _, cat := range AllCategories {
		implemented := implementedCategories[cat]
		panicked := panics(func() { a.Alias(cat, "probe-value") })
		if implemented == panicked {
			t.Errorf("category %q: implementedCategories says implemented=%v but Alias panicked=%v — implementedCategories and Alias's switch have drifted apart",
				cat, implemented, panicked)
		}
	}
}

// panics reports whether f panics, recovering so the test can assert on the
// outcome rather than crash.
func panics(f func()) (didPanic bool) {
	defer func() {
		if recover() != nil {
			didPanic = true
		}
	}()
	f()
	return false
}

// aliasIP's documented fallback: an input that doesn't parse as an IP at all
// still returns a valid (IPv4) alias rather than panicking, since
// Aliaser.Alias has no error return. Exercised directly here because
// Aliaser.Alias's category dispatch is the only caller in this milestone,
// and it's easy to lose this behavior in a future refactor without a test
// pinning it.
func TestAliasIP_MalformedInputFallsBackToIPv4(t *testing.T) {
	a := NewAliaser([]byte("fallback-salt"))
	alias := a.Alias(CategoryIP, "not-an-ip-at-all")
	parsed := net.ParseIP(alias)
	if parsed == nil || parsed.To4() == nil {
		t.Errorf("malformed-input fallback: got %q, want a valid IPv4 alias", alias)
	}
}
