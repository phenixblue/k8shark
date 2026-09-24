package anonymize

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"

	"github.com/phenixblue/k8shark/internal/capture"
)

// This file implements Options.FullSweep (#361): a second, opt-in pass that
// replaces any substring occurrence of an already-discovered namespace/
// node/pod/workload/ip/url value with its alias, anywhere in a record's
// string content — not just at the field paths the schema-aware/full-tree
// matchers in namespace.go/resourcename.go/ipmatch.go/urlmatch.go already
// know to look at. It exists to catch a real value that leaks through an
// unrecognized CRD's field, an escaped-JSON-string annotation (e.g.
// kubectl.kubernetes.io/last-applied-configuration), an Event message, or
// an opaque Table cell — none of which any schema-aware or self-evident-
// shape matcher can see. See Archive's own FullSweep comment for how this
// integrates into the two-pass architecture.
//
// Image is deliberately not covered: a registry-host alias
// (aliasRegistryHost) is tied to a specific image-reference field position,
// not free-standing text, so sweeping for it elsewhere has no clear
// motivating case the way a name, IP, or hostname leaking into free text
// does.

// minSweepCandidateLength is a v1 heuristic, not a guarantee (same tone as
// name.go/collision.go's own sizing comments): a candidate shorter than
// this is dropped from the sweep entirely, to blunt — not eliminate — the
// risk of a short, common value (a two-letter namespace, a one-octet-short
// IP fragment) colliding with ordinary, unrelated text.
const minSweepCandidateLength = 4

// sweepCandidate is one (original, alias) pair eligible for the sweep.
type sweepCandidate struct {
	Category Category
	Original string
	Alias    string
}

// candidateGroup is one compiled alternation of every candidate sharing a
// boundary predicate, plus the lookup sweepRecord needs to resolve a raw
// regex match back to its alias and category.
type candidateGroup struct {
	re         *regexp.Regexp
	byOriginal map[string]sweepCandidate
	// reject reports whether a byte immediately before or after a raw match
	// disqualifies it — see nameBoundaryReject/ipBoundaryReject.
	reject func(byte) bool
}

// sweepCandidateSet is the compiled, ready-to-scan form of every distinct
// original value the sweep covers, built once per Archive run by
// buildSweepCandidates after its collection pass has fully populated the
// trackers.
type sweepCandidateSet struct {
	// nameGroup covers namespace/node/pod/workload candidates and any
	// URL-category candidate that isn't itself an IP literal. ipGroup
	// covers the IP category plus any URL-category candidate that is an IP
	// literal (a URL's host position can be a bare IP — see
	// Result.HostsRenamed's own doc comment). Both are nil when there was
	// nothing eligible, so sweepRecord can skip the work entirely.
	nameGroup *candidateGroup
	ipGroup   *candidateGroup
	// ambiguousSkipped is the number of distinct original values excluded
	// from the sweep because the same literal string was a candidate under
	// more than one category with a different alias (e.g. a namespace and
	// a pod both named "prod") — see buildSweepCandidates.
	ambiguousSkipped int
}

// isAlnum reports whether b is an ASCII letter or digit. Sweep candidates
// (DNS-1123 names, IP literals, hostnames) are always ASCII, so a
// byte-level check is enough — no need to decode UTF-8 runes, and any
// non-ASCII adjacent byte is definitionally not a continuation of one of
// these candidates regardless of what larger character it's part of.
func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// nameBoundaryReject is the boundary predicate for namespace/node/pod/
// workload candidates and non-IP-shaped URL candidates. Rejects only
// alphanumeric adjacency — deliberately NOT '.' or ':' — so a name embedded
// in a cluster-local FQDN (svc.<namespace>.svc.cluster.local) or a
// namespace immediately followed by a colon in free text is still caught,
// not silently missed the way rejecting those characters would cause.
func nameBoundaryReject(b byte) bool {
	return isAlnum(b)
}

// ipBoundaryReject is the boundary predicate for IP-shaped candidates —
// stricter than nameBoundaryReject on purpose: without also rejecting '.'
// and ':' adjacency, a short IPv6 literal like "::1" would incorrectly
// match inside an unrelated, longer address like "fe80::1", and an IPv4
// literal would incorrectly match as a truncated fragment of a longer
// dotted-decimal string.
func ipBoundaryReject(b byte) bool {
	return isAlnum(b) || b == '.' || b == ':'
}

// buildSweepCandidates gathers every distinct (original, alias) pair the
// namespace/node/pod/workload/ip/url trackers discovered — after Archive's
// collection pass has fully populated them, so every distinct value
// anywhere in the archive is present regardless of which record it was
// first seen in — and compiles them into the two boundary-predicate groups
// sweepRecord scans against.
//
// Cross-category ambiguity: the same literal string can legitimately be a
// candidate under more than one category (a namespace named "prod" and a
// pod named "prod" are both realistic). A bare mention of "prod" in free
// text is then genuinely ambiguous about which alias applies — matching
// this package's existing collision-detection discipline (refuse rather
// than silently guess), any original value seen under more than one
// distinct alias is excluded from the sweep entirely, not guessed at.
func buildSweepCandidates(namespaceTracker, nodeTracker, podTracker, workloadTracker, ipTracker, urlTracker *collisionTracker) *sweepCandidateSet {
	type firstSeenEntry struct {
		alias string
	}
	firstSeen := make(map[string]firstSeenEntry)
	ambiguous := make(map[string]bool)

	var all []sweepCandidate
	add := func(cat Category, mapping map[string]string) {
		for original, alias := range mapping {
			all = append(all, sweepCandidate{Category: cat, Original: original, Alias: alias})
			if prior, ok := firstSeen[original]; ok {
				if prior.alias != alias {
					ambiguous[original] = true
				}
				continue
			}
			firstSeen[original] = firstSeenEntry{alias: alias}
		}
	}
	add(CategoryNamespace, namespaceTracker.Mapping())
	add(CategoryNode, nodeTracker.Mapping())
	add(CategoryPod, podTracker.Mapping())
	add(CategoryWorkload, workloadTracker.Mapping())
	add(CategoryIP, ipTracker.Mapping())
	add(CategoryURL, urlTracker.Mapping())

	var nameCands, ipCands []sweepCandidate
	for _, c := range all {
		if ambiguous[c.Original] || len(c.Original) < minSweepCandidateLength {
			continue
		}
		if net.ParseIP(c.Original) != nil {
			ipCands = append(ipCands, c)
		} else {
			nameCands = append(nameCands, c)
		}
	}

	return &sweepCandidateSet{
		nameGroup:        buildCandidateGroup(nameCands, nameBoundaryReject),
		ipGroup:          buildCandidateGroup(ipCands, ipBoundaryReject),
		ambiguousSkipped: len(ambiguous),
	}
}

// buildCandidateGroup compiles cands into one alternation regex, sorted by
// descending length (then lexicographically, for byte-identical output
// across runs regardless of Go map iteration order — the same determinism
// discipline sortedKeys enforces elsewhere in this package). Descending
// length matters for correctness, not just style: Go's RE2 alternation is
// leftmost-first, not leftmost-longest, so a shorter candidate that's a
// prefix of a longer one (e.g. "web" vs. "web-1") would otherwise win
// arbitrarily depending on list order — sorting longest-first guarantees
// the more specific candidate is always preferred.
func buildCandidateGroup(cands []sweepCandidate, reject func(byte) bool) *candidateGroup {
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		if len(cands[i].Original) != len(cands[j].Original) {
			return len(cands[i].Original) > len(cands[j].Original)
		}
		return cands[i].Original < cands[j].Original
	})
	byOriginal := make(map[string]sweepCandidate, len(cands))
	parts := make([]string, len(cands))
	for i, c := range cands {
		byOriginal[c.Original] = c
		parts[i] = regexp.QuoteMeta(c.Original)
	}
	return &candidateGroup{
		re:         regexp.MustCompile(strings.Join(parts, "|")),
		byOriginal: byOriginal,
		reject:     reject,
	}
}

// spliceCandidates replaces every accepted match of group's alternation in
// s with its candidate's alias. kind/path identify the field being swept,
// for the excluded(...) check.
//
// Matches are found via FindAllStringIndex — deliberately not a pattern
// with boundary characters baked into the consumed match: Go's RE2 has no
// lookaround, and consuming a boundary character as part of one match would
// make it unavailable as the boundary for an immediately-adjacent next
// match (e.g. two candidates separated by exactly one space). Checking the
// surrounding bytes manually, without consuming them, avoids that bug
// entirely — both a rejected and an accepted match leave every byte outside
// the actual candidate text untouched and available for the next check.
func spliceCandidates(s string, group *candidateGroup, excluded excludedFunc, kind, path string) (string, bool, int) {
	if group == nil {
		return s, false, 0
	}
	matches := group.re.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s, false, 0
	}
	var out strings.Builder
	last := 0
	occurrences := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if start > 0 && group.reject(s[start-1]) {
			continue
		}
		if end < len(s) && group.reject(s[end]) {
			continue
		}
		cand := group.byOriginal[s[start:end]]
		if excluded(cand.Category, kind, path) {
			continue
		}
		out.WriteString(s[last:start])
		out.WriteString(cand.Alias)
		last = end
		occurrences++
	}
	if occurrences == 0 {
		return s, false, 0
	}
	out.WriteString(s[last:])
	return out.String(), true, occurrences
}

// sweepRecord applies the full sweep to rec: every string leaf in the
// decoded body is scanned against candidates' two boundary-predicate
// groups, splicing in the alias for any accepted match. Runs after the
// schema-aware/full-tree matchers (see Archive's own ordering comment), so
// a leaf they already rewrote no longer contains its original value for
// this pass to re-match — SweepOccurrencesFound stays a clean count of
// occurrences those matchers would have missed, not inflated by
// double-counting the same occurrence twice.
//
// List-aware exactly like the other matchers in this package: an exclude
// rule scoped to a Kind applies correctly to a List response's items[], not
// the List wrapper itself.
func sweepRecord(rec *capture.Record, candidates *sweepCandidateSet, excluded excludedFunc) (bool, int, error) {
	if candidates == nil || (candidates.nameGroup == nil && candidates.ipGroup == nil) {
		return false, 0, nil
	}

	var obj map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
		return false, 0, nil
	}

	kind, _ := obj["kind"].(string)
	total := 0
	sweepItem := func(item map[string]interface{}, itemKind string) bool {
		return walkStrings(item, "", func(path, s string) (string, bool) {
			changed := false
			if out, ok, n := spliceCandidates(s, candidates.nameGroup, excluded, itemKind, path); ok {
				s, changed, total = out, true, total+n
			}
			if out, ok, n := spliceCandidates(s, candidates.ipGroup, excluded, itemKind, path); ok {
				s, changed, total = out, true, total+n
			}
			return s, changed
		})
	}

	modified := false
	if strings.HasSuffix(kind, "List") {
		items, _ := obj["items"].([]interface{})
		itemKind := strings.TrimSuffix(kind, "List")
		for i, itemRaw := range items {
			item, ok := itemRaw.(map[string]interface{})
			if !ok {
				continue
			}
			ik := itemKind
			if k, ok := item["kind"].(string); ok && k != "" {
				ik = k
			}
			if sweepItem(item, ik) {
				items[i] = item
				modified = true
			}
		}
	} else if sweepItem(obj, kind) {
		modified = true
	}

	if !modified {
		return false, 0, nil
	}
	newBody, err := json.Marshal(obj)
	if err != nil {
		return false, 0, fmt.Errorf("re-marshaling record: %w", err)
	}
	rec.ResponseBody = newBody
	return true, total, nil
}
