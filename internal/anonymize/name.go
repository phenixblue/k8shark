package anonymize

// wordsPerCategory is how many words from the adjective/noun lists are
// chained together for a category's alias. Categories with realistically
// high cardinality in a real cluster (pods, workloads) get an extra word to
// keep the combination space large enough that a same-category collision
// within one archive stays unlikely; node/namespace counts are realistically
// much smaller, so two words keeps those aliases shorter and easier to read
// in the common case without a meaningful collision-risk cost.
//
// This is a v1 sizing choice, not a correctness guarantee: two different
// original values can still collide onto the same alias (see Aliaser's doc
// comment on the archive-rewrite loop, which owns detecting and resolving an
// actual collision against its own per-run accumulator — that requires
// cross-record state this package deliberately does not hold).
var wordsPerCategory = map[Category]int{
	CategoryNode:      2,
	CategoryNamespace: 2,
	CategoryPod:       3,
	CategoryWorkload:  3,
}

// defaultWords is used for any category not listed in wordsPerCategory. In
// practice Alias (alias.go) only ever calls aliasName with a category from
// its own implementedCategories set, and every one of those is listed above
// — so this path isn't reachable through Alias today. It exists so aliasName
// stays total on its own terms rather than silently returning a 0-word
// "<category>" alias for an unlisted one, independent of whatever Alias's
// current dispatch policy happens to be.
const defaultWords = 2

// aliasName renders digest as a "<category>-<word>-<word>[-<word>]" string.
// Every word comes from the curated lists in wordlist.go, which are
// lowercase-letters-only by construction (checked in wordlist_test.go), so
// the result is always a valid DNS-1123 label regardless of which words are
// picked — there is no separate validation step here because there is
// nothing left for it to catch that the word list hasn't already ruled out.
//
// Each word position reads a distinct byte of digest (digest[i] for word i)
// so the words vary independently rather than moving in lockstep. SHA-256
// digests are 32 bytes and wordsPerCategory never exceeds 3, so digest[i] is
// always in range without needing to wrap.
func aliasName(category Category, digest []byte) string {
	n, ok := wordsPerCategory[category]
	if !ok {
		n = defaultWords
	}

	out := string(category)
	for i := 0; i < n; i++ {
		out += "-" + wordAt(i, digest[i%len(digest)])
	}
	return out
}

// wordAt returns the word for alias position i (alternating adjective/noun
// by position) selected by byte b.
func wordAt(i int, b byte) string {
	if i%2 == 0 {
		return adjectives[int(b)%len(adjectives)]
	}
	return nouns[int(b)%len(nouns)]
}
