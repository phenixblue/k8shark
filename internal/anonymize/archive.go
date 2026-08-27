package anonymize

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// sortedKeys returns m's keys sorted lexicographically, so a caller that
// needs to visit a map's entries in a fixed, repeatable order (see Archive's
// two index loops) doesn't inherit Go's per-process-randomized map iteration
// order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// archiveCategories are the categories Archive currently knows how to find
// and rewrite occurrences of in a real archive. Namespace-only for this
// milestone; node/pod/workload land in the next one, then IP/URL/image
// (see #137's milestone plan).
//
// Deliberately a separate set from Aliaser's own implementedCategories
// (alias.go): that one is about which categories the aliasing *primitive*
// can render at all — a lower-level, already-broader set from M1 (it
// already covers node/pod/workload, since Alias's per-category encoders
// don't depend on knowing where in a record those values live). This one is
// about which categories this package's archive-rewrite path knows how to
// *find*. A category can be alias-ready before it's archive-ready; letting
// Archive silently accept a category it can't actually locate occurrences
// of would be the same mistake M1's review caught for Alias itself, one
// layer up.
var archiveCategories = map[Category]bool{
	CategoryNamespace: true,
}

// Options controls what Archive() anonymizes.
type Options struct {
	// Categories is the set of categories to anonymize. Every entry must be
	// in archiveCategories — checked up front, before any archive I/O, so
	// an unsupported category fails loudly rather than silently doing
	// nothing for it.
	Categories []Category
	// Salt is the Aliaser salt. Callers resolve this themselves (a CLI
	// flag, a file, an environment variable, or a freshly generated value)
	// — this package never generates, stores, or reads one on its own. See
	// the package doc and #137's design notes on why a salt must never come
	// from a config file: doing so would silently turn a personal secret
	// into a team-shared one the moment a config profile is exported
	// (relevant to #139, not built here, but worth designing away from).
	Salt []byte
	// Identities, when non-empty, decrypt an encrypted source archive
	// before anonymizing. Mirrors redact.Options.Identities exactly.
	Identities []age.Identity
	// Recipients, when non-empty, cause the anonymized output archive to be
	// written as an age-encrypted envelope. Mirrors redact.Options.Recipients.
	Recipients []age.Recipient
}

// Result reports how many distinct values were anonymized, per category.
type Result struct {
	// NamespacesRenamed is the count of distinct original namespace names
	// encountered, not the count of records touched — renaming a namespace
	// that appears on 50 records still counts once.
	NamespacesRenamed int
}

// Archive reads srcPath, anonymizes it per opts, and writes to dstPath. The
// original archive is not modified.
//
// This mirrors internal/redact.Archive's overall shape (open; read
// metadata/index/watch-index; stream every record through a rewrite into a
// fresh archive; rebuild the index with corrected sequence numbers) but
// keeps its own copy of that loop rather than sharing redact's. That's
// deliberate, not an oversight: redact.Archive's loop never needs to change
// the apiPath a record is stored under, and anonymize's namespace category
// must — forcing that divergence into a shared abstraction before it had
// proven out risked regressing a shipped, fuzz-tested command for an
// unproven second caller. Revisit sharing once this package's shape is
// settled across more categories.
func Archive(srcPath, dstPath string, opts Options) (Result, error) {
	if len(opts.Categories) == 0 {
		return Result{}, fmt.Errorf("anonymize: no categories requested")
	}
	if len(opts.Salt) == 0 {
		// An empty salt still produces deterministic aliases (HMAC accepts
		// any key, including an empty one) — it just makes them trivially
		// weak, and an empty Options.Salt is an easy mistake for a caller
		// that isn't the CLI (which always resolves a non-empty one; see
		// cmd/anonymize_flags.go) to make by accident, e.g. a test fixture
		// or a future caller that forgets to wire salt resolution at all.
		// Rejecting it here keeps the library API safe by default rather
		// than relying on every caller to remember.
		return Result{}, fmt.Errorf("anonymize: Options.Salt must not be empty")
	}
	doNamespace := false
	for _, cat := range opts.Categories {
		if !archiveCategories[cat] {
			return Result{}, fmt.Errorf("anonymize: category %q is not yet supported for archive rewriting", cat)
		}
		if cat == CategoryNamespace {
			doNamespace = true
		}
	}

	ar, err := archive.OpenWithIdentities(srcPath, opts.Identities)
	if err != nil {
		return Result{}, fmt.Errorf("opening archive: %w", err)
	}
	defer ar.Close()

	meta, err := ar.ReadMetadata()
	if err != nil {
		return Result{}, fmt.Errorf("reading metadata: %w", err)
	}
	// The anonymized archive is written by the current writer, so stamp it
	// with the current format version — same reasoning as redact.Archive.
	meta.FormatVersion = capture.CurrentFormatVersion

	idx, err := ar.ReadIndex()
	if err != nil {
		return Result{}, fmt.Errorf("reading index: %w", err)
	}
	// Watch-index may be absent for older archives, but a present-and-
	// malformed one is a corrupt archive, not an absent one — surface that
	// error rather than silently anonymizing as if it were never captured
	// (same reasoning as redact.Archive).
	wi, _, err := ar.ReadWatchIndex()
	if err != nil {
		return Result{}, fmt.Errorf("reading watch index: %w", err)
	}

	aliaser := NewAliaser(opts.Salt)
	// Wrapped in a collisionTracker rather than called directly: two distinct
	// original namespace names landing on the same alias is a real
	// possibility, not a theoretical one (see collision.go's doc comment for
	// the numbers), and left unchecked it would let the index/watch-index
	// rebuild below silently clobber one entry with another.
	namespaceTracker := newCollisionTracker(CategoryNamespace, func(original string) string {
		return aliaser.Alias(CategoryNamespace, original)
	})
	namespaceAlias := namespaceTracker.Alias

	var sw *archive.StreamWriter
	if len(opts.Recipients) > 0 {
		sw, err = archive.NewEncryptedStreamWriter(dstPath, opts.Recipients)
	} else {
		sw, err = archive.NewStreamWriter(dstPath)
	}
	if err != nil {
		return Result{}, fmt.Errorf("creating output archive: %w", err)
	}
	// Ensure the output writer's file handle is released if we return early
	// (e.g. a malformed record, or a detected alias collision) before Finish.
	// Abort is a no-op once Finish has run, so the success path is
	// unaffected. Abort only closes the file handle, though — it
	// deliberately does not delete dstPath (matching redact.Archive's
	// identical pattern), so on any early return this also removes the
	// partial file. Otherwise a rejected run (e.g. a collision, forcing the
	// user to pick a different salt and try again) would leave a stale,
	// misleading file sitting at the requested output path even though
	// Archive reported failure. Best-effort: the original error already
	// explains what went wrong, so a removal error here is not surfaced —
	// this is cleanup, not the primary failure.
	finished := false
	defer func() {
		_ = sw.Abort()
		if !finished {
			_ = os.Remove(dstPath)
		}
	}()

	newIdx := make(capture.Index, len(idx))
	newWI := make(capture.WatchIndex, len(wi))

	// rewriteEntryRecords reads every record under apiPath (the *original*
	// path — that's what the source archive is keyed and stored under),
	// rewrites each one's body and its own APIPath field, and writes it back
	// under newAPIPath (the *rewritten* path). It returns the new sequence
	// numbers assigned by the writer.
	//
	// newAPIPath is computed once, before this loop, by the caller — every
	// record sharing one index entry shares one apiPath, so the rewrite is
	// the same for all of them. The critical invariant callers must hold:
	// newAPIPath must be the exact same string used both here (as the
	// WriteRecordRaw argument) and as the key stored in newIdx/newWI.
	// WriteRecordRaw derives the on-disk shard via a hash of whatever string
	// it's given, so if the index key and the write argument ever diverge,
	// the archive ends up structurally corrupt — the index points at one
	// shard, the bytes live in another — in a way a body-content check alone
	// would not catch.
	rewriteEntryRecords := func(apiPath, newAPIPath string, seqs []int) ([]int, error) {
		newSeqs := make([]int, 0, len(seqs))
		for _, seq := range seqs {
			data, err := ar.ReadRecord(apiPath, seq)
			if err != nil {
				return nil, fmt.Errorf("reading record %s seq %d: %w", apiPath, seq, err)
			}
			var rec capture.Record
			if err := json.Unmarshal(data, &rec); err != nil {
				return nil, fmt.Errorf("parsing record %s seq %d: %w", apiPath, seq, err)
			}

			if doNamespace {
				if _, err := rewriteNamespaceInRecord(&rec, namespaceAlias); err != nil {
					return nil, fmt.Errorf("anonymizing record %s seq %d: %w", apiPath, seq, err)
				}
			}
			// Keep the record's own APIPath field in sync with wherever it's
			// actually being stored, unconditionally — not just when a
			// rewrite happened to fire. If this ever fell out of sync with
			// newAPIPath, the record's body would disagree with the index
			// entry it's filed under.
			rec.APIPath = newAPIPath

			newSeq, err := sw.WriteRecordRaw(newAPIPath, rec)
			if err != nil {
				return nil, fmt.Errorf("writing record %s seq %d: %w", newAPIPath, seq, err)
			}
			newSeqs = append(newSeqs, newSeq)
		}
		return newSeqs, nil
	}

	// Visit both indexes in sorted-by-original-key order, not Go's randomized
	// map iteration order. This isn't about alias consistency — Alias itself
	// is already order-independent by construction (see alias.go) — it's
	// about the *physical layout* of the output ZIP: WriteRecordRaw appends
	// to the archive in whatever order it's called, so without a fixed
	// traversal order, two runs over the identical source with the identical
	// salt would compute identical aliases but still emit byte-different
	// archives, just because Go happened to range over the map differently.
	// Sorting the *original* keys is enough: the source archive's key set is
	// fixed from one run to the next, and the alias function is
	// deterministic, so a fixed traversal of the original keys yields a
	// fixed traversal of the rewritten ones too. TestArchive_Deterministic
	// diffs two real runs' output bytes to hold this, not just infer it.
	for _, apiPath := range sortedKeys(map[string]*capture.IndexEntry(idx)) {
		entry := idx[apiPath]
		newAPIPath := apiPath
		if doNamespace {
			if rewritten, ok := rewriteNamespaceInPath(apiPath, namespaceAlias); ok {
				newAPIPath = rewritten
			}
			// Check as soon as a collision could have been produced, rather
			// than waiting until the whole pass finishes: the specific,
			// actionable error from the tracker (naming the two colliding
			// original values) is what a caller should see, not the generic
			// one below — which exists only as a defensive backstop for a
			// collision this check somehow didn't catch, and would fire on
			// this same iteration if we let it run first.
			if err := namespaceTracker.Err(); err != nil {
				return Result{}, err
			}
		}
		if existing, ok := newIdx[newAPIPath]; ok {
			return Result{}, fmt.Errorf("anonymize: internal error: both %q and %q rewrite to output path %q — refusing to silently drop one", existing.APIPath, apiPath, newAPIPath)
		}
		newSeqs, err := rewriteEntryRecords(apiPath, newAPIPath, entry.Seqs)
		if err != nil {
			return Result{}, err
		}
		newIdx[newAPIPath] = &capture.IndexEntry{
			APIPath: newAPIPath,
			Seqs:    newSeqs,
			Times:   entry.Times,
			Counts:  entry.Counts,
		}
	}

	for _, apiPath := range sortedKeys(map[string]*capture.WatchIndexEntry(wi)) {
		wiEntry := wi[apiPath]
		newAPIPath := apiPath
		if doNamespace {
			if rewritten, ok := rewriteNamespaceInPath(apiPath, namespaceAlias); ok {
				newAPIPath = rewritten
			}
			if err := namespaceTracker.Err(); err != nil {
				return Result{}, err
			}
		}
		if existing, ok := newWI[newAPIPath]; ok {
			return Result{}, fmt.Errorf("anonymize: internal error: both %q and %q rewrite to output watch-path %q — refusing to silently drop one", existing.APIPath, apiPath, newAPIPath)
		}
		newSeqs, err := rewriteEntryRecords(apiPath, newAPIPath, wiEntry.Seqs)
		if err != nil {
			return Result{}, err
		}
		newWI[newAPIPath] = &capture.WatchIndexEntry{
			APIPath:    newAPIPath,
			Seqs:       newSeqs,
			Times:      wiEntry.Times,
			EventTypes: wiEntry.EventTypes,
		}
	}

	if err := sw.Finish(&meta, newIdx, newWI); err != nil {
		return Result{}, fmt.Errorf("finishing output archive: %w", err)
	}
	finished = true

	return Result{NamespacesRenamed: namespaceTracker.Count()}, nil
}
