package anonymize

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/config"
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
// and rewrite occurrences of in a real archive — every category #137's
// design plan calls out is now covered.
//
// Deliberately a separate set from Aliaser's own implementedCategories
// (alias.go): that one is about which categories the aliasing *primitive*
// can render at all — a lower-level, already-broader set from M1. This one
// is about which categories this package's archive-rewrite path knows how
// to *find*. A category can be alias-ready before it's archive-ready;
// letting Archive silently accept a category it can't actually locate
// occurrences of would be the same mistake M1's review caught for Alias
// itself, one layer up.
var archiveCategories = map[Category]bool{
	CategoryNamespace: true,
	CategoryNode:      true,
	CategoryPod:       true,
	CategoryWorkload:  true,
	CategoryIP:        true,
	CategoryURL:       true,
	CategoryImage:     true,
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
	// Rules is a list of field-path exclusions layered on top of
	// Categories — see config.AnonymizeRule's own doc comment. Every entry
	// must have Exclude set; Archive rejects the whole run otherwise (see
	// newExcludeMatcher).
	Rules []config.AnonymizeRule
}

// SchemaVersion is the version of the `kshrk anonymize -o json` output
// shape (Result). Bump on a breaking change to the documented shape, per
// docs/stability-policy.md's convention for the other -o json commands
// (internal/query, internal/diagnose, internal/inspect).
const SchemaVersion = 1

// Result reports how many distinct values were anonymized, per category.
// Every count is the number of distinct original values encountered, not
// the number of records touched — a value that appears on 50 records still
// counts once.
type Result struct {
	SchemaVersion     int `json:"schema_version"`
	NamespacesRenamed int `json:"namespaces_renamed"`
	NodesRenamed      int `json:"nodes_renamed"`
	PodsRenamed       int `json:"pods_renamed"`
	WorkloadsRenamed  int `json:"workloads_renamed"`
	// IPsRenamed counts distinct IP literals.
	IPsRenamed int `json:"ips_renamed"`
	// HostsRenamed counts distinct URL-category values: bare hostnames (an
	// Ingress host, a Service externalName) and whatever occupies a
	// scheme://host URL's host position — which is not always a DNS name.
	// CaptureMetadata.ServerAddress (e.g. "https://127.0.0.1:6443") goes
	// through this same category, so an IP literal sitting in a URL's host
	// position counts here too, not under IPsRenamed — see Archive's own
	// ServerAddress-rewrite comment for why that's the deliberate choice.
	HostsRenamed int `json:"hosts_renamed"`
	// RegistriesRenamed counts distinct container-image registry hosts
	// (the leading host[:port] segment of an image reference).
	RegistriesRenamed int `json:"registries_renamed"`
	// OutputPath and OutputBytes are populated by the CLI layer (cmd/anonymize.go),
	// not by Archive itself — Archive doesn't know its own caller's chosen
	// output path or need to stat the file it just finished writing.
	// Deliberately not omitempty: docs/stability-policy.md's -o json
	// contract requires every command's top-level key set to be identical
	// on every run, present even when the value is empty/zero — a stray
	// omitempty here would silently drop output_path/output_bytes from the
	// rare case where OutputBytes is legitimately 0 (e.g. os.Stat failed)
	// or a caller marshals a zero-value Result directly.
	OutputPath  string `json:"output_path"`
	OutputBytes int64  `json:"output_bytes"`
	// Mapping is the original-to-alias mapping, keyed by category, for
	// every category Options.Categories requested — regardless of whether
	// the caller actually asked to emit it. Deliberately excluded from the
	// -o json output (json:"-"): unlike every other field here, this is the
	// one genuinely sensitive payload Archive can produce, and #137's own
	// design explicitly requires it be "never persisted by default" — that
	// has to hold for accidentally-scripted `-o json | jq` output too, not
	// just for the default text summary. The CLI layer (cmd/anonymize.go)
	// only writes this out at all when --emit-mapping was explicitly given.
	Mapping map[Category]map[string]string `json:"-"`
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
	doNode := false
	doPod := false
	doWorkload := false
	doIP := false
	doURL := false
	doImage := false
	for _, cat := range opts.Categories {
		if !archiveCategories[cat] {
			return Result{}, fmt.Errorf("anonymize: category %q is not supported for archive rewriting", cat)
		}
		switch cat {
		case CategoryNamespace:
			doNamespace = true
		case CategoryNode:
			doNode = true
		case CategoryPod:
			doPod = true
		case CategoryWorkload:
			doWorkload = true
		case CategoryIP:
			doIP = true
		case CategoryURL:
			doURL = true
		case CategoryImage:
			doImage = true
		}
	}
	// enabledResource gates rewriteResourceNameInObject/InPath's field-level
	// checks: a field belonging to a category not requested this run is left
	// untouched even where it's recognized, e.g. --categories pod alone must
	// not also alias a Pod's spec.nodeName.
	enabledResource := map[Category]bool{
		CategoryNode:     doNode,
		CategoryPod:      doPod,
		CategoryWorkload: doWorkload,
	}
	doResourceName := doNode || doPod || doWorkload

	excluded, err := newExcludeMatcher(opts.Rules)
	if err != nil {
		return Result{}, err
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
	// Each category gets its own collisionTracker rather than being called
	// directly: two distinct original values landing on the same alias is a
	// real possibility, not a theoretical one (see collision.go's doc
	// comment for the numbers), and left unchecked it would let the
	// index/watch-index rebuild below silently clobber one entry with
	// another.
	namespaceTracker := newCollisionTracker(CategoryNamespace, func(original string) string {
		return aliaser.Alias(CategoryNamespace, original)
	})
	nodeTracker := newCollisionTracker(CategoryNode, func(original string) string {
		return aliaser.Alias(CategoryNode, original)
	})
	podTracker := newCollisionTracker(CategoryPod, func(original string) string {
		return aliaser.Alias(CategoryPod, original)
	})
	workloadTracker := newCollisionTracker(CategoryWorkload, func(original string) string {
		return aliaser.Alias(CategoryWorkload, original)
	})
	ipTracker := newCollisionTracker(CategoryIP, func(original string) string {
		return aliaser.Alias(CategoryIP, original)
	})
	urlTracker := newCollisionTracker(CategoryURL, func(original string) string {
		return aliaser.Alias(CategoryURL, original)
	})
	imageTracker := newCollisionTracker(CategoryImage, func(original string) string {
		return aliaser.Alias(CategoryImage, original)
	})
	namespaceAlias := namespaceTracker.Alias
	ipAlias := ipTracker.Alias
	urlAlias := urlTracker.Alias
	imageAlias := imageTracker.Alias
	// resourceAlias dispatches to the tracker for whichever category a
	// resourcename.go call site determined a given occurrence belongs to.
	// Only ever called for a category enabledResource already gated on, so
	// the default case is unreachable in practice, not a real error path.
	resourceAlias := func(cat Category, original string) string {
		switch cat {
		case CategoryNode:
			return nodeTracker.Alias(original)
		case CategoryPod:
			return podTracker.Alias(original)
		case CategoryWorkload:
			return workloadTracker.Alias(original)
		default:
			panic(fmt.Sprintf("anonymize: internal error: resourceAlias called with unexpected category %q", cat))
		}
	}
	// firstCollisionErr checks every tracker, not just the ones this run
	// actually enabled — an unused tracker's Err() is always nil, so this
	// stays correct without needing to be told which categories are active.
	firstCollisionErr := func() error {
		for _, t := range []*collisionTracker{namespaceTracker, nodeTracker, podTracker, workloadTracker, ipTracker, urlTracker, imageTracker} {
			if err := t.Err(); err != nil {
				return err
			}
		}
		return nil
	}

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
				if _, err := rewriteNamespaceInRecord(&rec, excluded, namespaceAlias); err != nil {
					return nil, fmt.Errorf("anonymizing record %s seq %d: %w", apiPath, seq, err)
				}
			}
			if doResourceName {
				if _, err := rewriteResourceNameInRecord(&rec, enabledResource, excluded, resourceAlias); err != nil {
					return nil, fmt.Errorf("anonymizing record %s seq %d: %w", apiPath, seq, err)
				}
			}
			if doIP {
				if _, err := rewriteIPInRecord(&rec, excluded, ipAlias); err != nil {
					return nil, fmt.Errorf("anonymizing record %s seq %d: %w", apiPath, seq, err)
				}
			}
			if doURL {
				if _, err := rewriteURLInRecord(&rec, excluded, urlAlias); err != nil {
					return nil, fmt.Errorf("anonymizing record %s seq %d: %w", apiPath, seq, err)
				}
			}
			if doImage {
				if _, err := rewriteImageRegistryInRecord(&rec, excluded, imageAlias); err != nil {
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
			// Check as soon as a collision could have been produced by the
			// *path* rewrite above, rather than waiting: the specific,
			// actionable error from the tracker (naming the two colliding
			// original values) is what a caller should see, not the generic
			// one below. This alone is not enough, though — see the second
			// check after rewriteEntryRecords.
			if err := firstCollisionErr(); err != nil {
				return Result{}, err
			}
		}
		if doResourceName {
			// Chained onto newAPIPath, not the original apiPath: the
			// namespace segment (if any) has already been rewritten above,
			// and this must combine with it into one final path, not
			// separately rewrite the pre-namespace-alias string.
			if rewritten, ok := rewriteResourceNameInPath(newAPIPath, enabledResource, resourceAlias); ok {
				newAPIPath = rewritten
			}
			if err := firstCollisionErr(); err != nil {
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
		// A second check, after the records under this path have had their
		// *bodies* rewritten too. The path-only checks above can't see a
		// collision that's only ever introduced by body content — e.g. two
		// Namespace objects' metadata.name values colliding inside a
		// /api/v1/namespaces NamespaceList response, which has no namespace
		// path segment at all for rewriteNamespaceInPath to have looked at.
		// Without this, that collision wouldn't be caught until the *next*
		// loop iteration, and not at all if this were the last one —
		// Archive would report success despite having detected a real
		// collision.
		if err := firstCollisionErr(); err != nil {
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
			if err := firstCollisionErr(); err != nil {
				return Result{}, err
			}
		}
		if doResourceName {
			if rewritten, ok := rewriteResourceNameInPath(newAPIPath, enabledResource, resourceAlias); ok {
				newAPIPath = rewritten
			}
			if err := firstCollisionErr(); err != nil {
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
		if err := firstCollisionErr(); err != nil {
			return Result{}, err
		}
		newWI[newAPIPath] = &capture.WatchIndexEntry{
			APIPath:    newAPIPath,
			Seqs:       newSeqs,
			Times:      wiEntry.Times,
			EventTypes: wiEntry.EventTypes,
		}
	}

	// A final, unconditional check before Finish, independent of exactly
	// where in the two loops above a collision happened to be introduced.
	// This is the guarantee that actually matters — "no successful run has a
	// collision" — and it should not depend on remembering to place a check
	// after every single site that can call an alias function. The
	// per-iteration checks above exist so a doomed run fails fast rather
	// than finishing all the remaining I/O first; this one exists so a gap
	// in those doesn't matter.
	if err := firstCollisionErr(); err != nil {
		return Result{}, err
	}

	// meta was populated by ar.ReadMetadata(), so left untouched its Encrypted
	// field still describes the *source* archive, not the one actually being
	// written here. redact.Archive sets this explicitly for the identical
	// reason (internal/redact/redact.go); anonymize needs the same
	// correction, or an anonymized-and-now-plaintext copy of an encrypted
	// source would misreport itself as encrypted, and vice versa for a
	// plaintext source anonymized straight into an encrypted output.
	meta.Encrypted = len(opts.Recipients) > 0

	// Provenance, mirroring Redacted/SecretsRedacted's identical pattern:
	// mark the output as anonymized and record which categories were
	// actually applied, so a later `kshrk inspect`/the UI's capture-info
	// card can say so without having to guess from the data itself.
	meta.Anonymized = true
	anonymizedCategories := make([]string, len(opts.Categories))
	for i, cat := range opts.Categories {
		anonymizedCategories[i] = string(cat)
	}
	meta.AnonymizedCategories = anonymizedCategories

	// CaptureMetadata.ServerAddress lives on the metadata, not inside any
	// Record body, so it needs its own rewrite here rather than going
	// through rewriteEntryRecords — but it's the exact same URL shape
	// (e.g. "https://127.0.0.1:6443") spliceURLHosts already handles, so it
	// shares urlTracker/urlAlias with every other URL-category occurrence
	// rather than getting a special-cased alias of its own.
	if doURL {
		if rewritten, ok := spliceURLHosts(meta.ServerAddress, urlAlias); ok {
			meta.ServerAddress = rewritten
		}
		if err := firstCollisionErr(); err != nil {
			return Result{}, err
		}
	}

	if err := sw.Finish(&meta, newIdx, newWI); err != nil {
		return Result{}, fmt.Errorf("finishing output archive: %w", err)
	}
	finished = true

	// Only the requested categories' trackers actually saw any values (an
	// unrequested category's tracker is wired up but never called), so its
	// Mapping() would just be an empty map — including it anyway would be
	// harmless but misleading, implying that category was processed.
	mapping := make(map[Category]map[string]string, len(opts.Categories))
	for _, cat := range opts.Categories {
		switch cat {
		case CategoryNamespace:
			mapping[cat] = namespaceTracker.Mapping()
		case CategoryNode:
			mapping[cat] = nodeTracker.Mapping()
		case CategoryPod:
			mapping[cat] = podTracker.Mapping()
		case CategoryWorkload:
			mapping[cat] = workloadTracker.Mapping()
		case CategoryIP:
			mapping[cat] = ipTracker.Mapping()
		case CategoryURL:
			mapping[cat] = urlTracker.Mapping()
		case CategoryImage:
			mapping[cat] = imageTracker.Mapping()
		}
	}

	return Result{
		SchemaVersion:     SchemaVersion,
		NamespacesRenamed: namespaceTracker.Count(),
		NodesRenamed:      nodeTracker.Count(),
		PodsRenamed:       podTracker.Count(),
		WorkloadsRenamed:  workloadTracker.Count(),
		IPsRenamed:        ipTracker.Count(),
		HostsRenamed:      urlTracker.Count(),
		RegistriesRenamed: imageTracker.Count(),
		Mapping:           mapping,
	}, nil
}
