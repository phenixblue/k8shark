package anonymize

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/phenixblue/k8shark/internal/capture"
)

// This file implements Options.Verify: a leak check run on every record's
// *final* body just before it is written, so a run can say whether the
// artifact it produced still contains any original value — not merely
// whether each matcher did its own job.
//
// It deliberately reuses the sweep's own matching (sweepRecord, with the same
// candidate groups, context rules, key handling and exclude rules) instead
// of a second, independent search, for two reasons. A leak check that is
// stricter than the sweep (a raw substring search) would flag "api" inside
// "/api/v1" and "kube-api-access-x" on every run, and a check nobody can get
// a clean result from gets ignored. And a check that is looser or different
// would pass things the sweep itself would still change. So "residual" here
// means exactly: an occurrence the sweep considers a match, in an output
// record, at a path no exclude rule covers.
//
// It runs from the in-flight record rather than re-reading the output file
// so it also works when the output is age-encrypted to recipients (which this
// process can't decrypt), and it never reports the original values — only
// where they were found — so its output is safe to paste into a bug report
// or a CI log.

// ResidualLocation aggregates the places an original value survived in the
// output: one entry per (category, kind, field path). It deliberately carries
// no original value.
type ResidualLocation struct {
	// Category is the anonymize category of the surviving original.
	Category string `json:"category"`
	// Kind is the object Kind the occurrence was found under, or "" for
	// something outside any object (the record's API path, the archive
	// metadata).
	Kind string `json:"kind"`
	// FieldPath is the field's path in this package's usual dotted/"[*]"
	// notation, usable as a rule's fieldPath to exclude it deliberately.
	FieldPath string `json:"field_path"`
	// Occurrences is how many times an original value was found there.
	Occurrences int `json:"occurrences"`
	// ExampleAPIPath is the (already-anonymized) API path of one record
	// where it was found, to make the location easy to look up.
	ExampleAPIPath string `json:"example_api_path"`
}

// MaxResidualLocations caps ResidualLocation entries reported; the total
// count is always exact regardless.
const MaxResidualLocations = 25

type residualKey struct {
	category Category
	kind     string
	path     string
}

type residualTracker struct {
	candidates *sweepCandidateSet
	excluded   excludedFunc
	// scrubOrder is every candidate, longest original first, so a longer
	// original is replaced before any shorter one inside it.
	scrubOrder []sweepCandidate
	counts     map[residualKey]int
	example    map[residualKey]string
	total      int
}

func newResidualTracker(candidates *sweepCandidateSet, excluded excludedFunc) *residualTracker {
	var order []sweepCandidate
	for _, g := range []*candidateGroup{candidates.nameGroup, candidates.ipGroup, candidates.shortGroup} {
		if g == nil {
			continue
		}
		for _, c := range g.byOriginal {
			order = append(order, c)
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if len(order[i].Original) != len(order[j].Original) {
			return len(order[i].Original) > len(order[j].Original)
		}
		return order[i].Original < order[j].Original
	})
	return &residualTracker{
		candidates: candidates,
		excluded:   excluded,
		scrubOrder: order,
		counts:     make(map[residualKey]int),
		example:    make(map[residualKey]string),
	}
}

// managedFieldsKey matches the "k:{...}" and "v:..." keys managedFields uses
// to identify a list item by value. Every distinct item would otherwise be
// its own location in the report; they're all the same finding.
var managedFieldsKey = regexp.MustCompile(`k:\{[^}]*\}|v:"[^"]*"`)

// scrub replaces every original value inside s with its alias, by plain
// substring — deliberately not the context-aware matching the check itself
// uses. A field path is assembled from the record's own keys, so a key that
// still embeds an original (the very thing being reported) would otherwise
// put that original into the report; the report must never carry one, so
// over-scrubbing (a path segment that happens to equal a short name) is the
// right side to err on.
func (r *residualTracker) scrub(s string) string {
	for _, c := range r.scrubOrder {
		s = strings.ReplaceAll(s, c.Original, c.Alias)
	}
	return s
}

// recording wraps the exclude matcher: an occurrence a rule excludes is a
// deliberate keep, not a leak; anything else is recorded as residual.
func (r *residualTracker) recording(apiPath string) excludedFunc {
	return func(cat Category, kind, path string) bool {
		if r.excluded(cat, kind, path) {
			return true
		}
		k := residualKey{cat, kind, managedFieldsKey.ReplaceAllString(r.scrub(path), "k:{*}")}
		r.counts[k]++
		if _, ok := r.example[k]; !ok {
			r.example[k] = r.scrub(apiPath)
		}
		r.total++
		return false
	}
}

// checkRecord scans rec — a record in its final, about-to-be-written form —
// for surviving originals. It works on a copy; rec is never modified.
func (r *residualTracker) checkRecord(rec *capture.Record) {
	cp := *rec
	cp.ResponseBody = append(json.RawMessage(nil), rec.ResponseBody...)
	// An error here can only be a key-rename collision in the throwaway copy
	// (the real sweep would have failed the run on the same input), so there
	// is nothing useful to add to it.
	_, _, _ = sweepRecord(&cp, r.candidates, r.recording(rec.APIPath))
}

// checkString scans one string that lives outside any record body (the
// record's own API path, the archive's server_address) under the given
// pseudo-field name.
func (r *residualTracker) checkString(s, field, apiPath string) {
	if s == "" {
		return
	}
	rec := r.recording(apiPath)
	for _, g := range []*candidateGroup{r.candidates.nameGroup, r.candidates.ipGroup, r.candidates.shortGroup} {
		s, _, _ = spliceCandidatesIn(s, g, rec, "", field, false)
	}
}

// locations returns the aggregated locations, most occurrences first (ties
// broken by key for a stable, deterministic report), capped at
// MaxResidualLocations. Never nil, so the JSON field is always an array.
func (r *residualTracker) locations() []ResidualLocation {
	out := make([]ResidualLocation, 0, len(r.counts))
	for k, n := range r.counts {
		out = append(out, ResidualLocation{
			Category:       string(k.category),
			Kind:           k.kind,
			FieldPath:      k.path,
			Occurrences:    n,
			ExampleAPIPath: r.example[k],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Occurrences != out[j].Occurrences {
			return out[i].Occurrences > out[j].Occurrences
		}
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].FieldPath < out[j].FieldPath
	})
	if len(out) > MaxResidualLocations {
		out = out[:MaxResidualLocations]
	}
	return out
}
