package anonymize

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/config"
	"github.com/phenixblue/k8shark/internal/store"
)

// fixedNow is used for every timestamp in these fixtures (never time.Now()),
// so that TestArchive_Deterministic's byte-for-byte comparison isn't
// confounded by wall-clock drift between the two source-archive builds it
// diffs. Every ZIP entry's own on-disk mod-time is already fixed by
// internal/archive (epochModTime) for the same reason — this pins the other
// half: the JSON content's own timestamp fields.
var fixedNow = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

// buildAnonymizeTestArchive writes a minimal archive to a temp file and
// returns its path. Mirrors internal/redact/redact_test.go's
// buildTestArchive, with one deliberate difference: fixed timestamps
// throughout rather than time.Now(), for the reason fixedNow's doc explains.
func buildAnonymizeTestArchive(t *testing.T, records []*capture.Record) string {
	t.Helper()
	dir := t.TempDir()

	idx := capture.Index{}
	pathSeq := map[string]int{}
	for _, r := range records {
		seq := pathSeq[r.APIPath]
		pathSeq[r.APIPath] = seq + 1
		e := idx[r.APIPath]
		if e == nil {
			e = &capture.IndexEntry{APIPath: r.APIPath}
			idx[r.APIPath] = e
		}
		e.Seqs = append(e.Seqs, seq)
		e.Times = append(e.Times, r.CapturedAt)
	}

	meta := &capture.CaptureMetadata{
		CaptureID:         "anonymize-test",
		CapturedAt:        fixedNow.Add(-time.Minute),
		CapturedUntil:     fixedNow,
		KubernetesVersion: "v1.34.0",
		ServerAddress:     "https://127.0.0.1:6443",
		RecordCount:       len(records),
	}

	outPath := filepath.Join(dir, "test.kshrk")
	sw, err := archive.NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	for _, r := range records {
		if _, err := sw.WriteRecord(r); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return outPath
}

// namespaceFixtureRecords builds a small but representative set of records:
// a Namespace object, a Pod living in it, and an Event about that Pod — the
// three places a namespace name shows up that this milestone specifically
// set out to keep consistent (see namespace.go's doc comment).
func namespaceFixtureRecords() []*capture.Record {
	nsBody := `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"prod"}}`
	podListBody := `{"kind":"PodList","apiVersion":"v1","items":[
		{"metadata":{"name":"web-1","namespace":"prod"},"spec":{"containers":[{"name":"app"}]}}
	]}`
	eventListBody := `{"kind":"EventList","apiVersion":"v1","items":[
		{"metadata":{"name":"web-1.abc","namespace":"prod"},
		 "involvedObject":{"kind":"Pod","name":"web-1","namespace":"prod"},
		 "message":"Started container app"}
	]}`
	return []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nsBody)},
		{ID: "r2", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podListBody)},
		{ID: "r3", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/events", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(eventListBody)},
	}
}

// The central proof for this milestone: the same real namespace name, seen
// on three different Kinds via three different API paths (a Namespace's own
// identity, a Pod's membership, and an Event's own namespace plus its
// involvedObject reference), must come out as the exact same alias
// everywhere — and the archive's own index keys must use that same alias
// too, not just the record bodies.
func TestArchive_NamespaceConsistentAcrossKindsAndPaths(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	salt := []byte("integration-test-salt")
	result, err := Archive(src, dst, Options{Categories: []Category{CategoryNamespace}, Salt: salt})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.NamespacesRenamed != 1 {
		t.Errorf("NamespacesRenamed = %d, want 1 (one distinct namespace, seen on three records)", result.NamespacesRenamed)
	}

	wantAlias := NewAliaser(salt).Alias(CategoryNamespace, "prod")

	t.Run("Result.Mapping carries the same original->alias pair, only for requested categories", func(t *testing.T) {
		nsMapping, ok := result.Mapping[CategoryNamespace]
		if !ok {
			t.Fatal("Mapping has no entry for the requested namespace category")
		}
		if got, want := nsMapping["prod"], wantAlias; got != want {
			t.Errorf(`Mapping[namespace]["prod"] = %q, want %q`, got, want)
		}
		if len(nsMapping) != 1 {
			t.Errorf("Mapping[namespace] has %d entries, want 1", len(nsMapping))
		}
		if _, ok := result.Mapping[CategoryPod]; ok {
			t.Error("Mapping has an entry for CategoryPod, which was never requested")
		}
	})

	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("opening output archive: %v", err)
	}
	defer ar.Close()

	idx, err := ar.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}

	wantPaths := []string{
		"/api/v1/namespaces/" + wantAlias,
		"/api/v1/namespaces/" + wantAlias + "/pods",
		"/api/v1/namespaces/" + wantAlias + "/events",
	}
	for _, p := range wantPaths {
		entry, ok := idx[p]
		if !ok {
			t.Errorf("index has no entry for %q (index keys: %v) — the namespace segment was not rewritten consistently with the alias used in record bodies", p, indexKeys(idx))
			continue
		}
		if len(entry.Seqs) != 1 {
			t.Errorf("%s: got %d seqs, want 1", p, len(entry.Seqs))
		}
	}
	// And the original path must be gone — proves this is a rewrite, not an
	// addition alongside the original.
	if _, ok := idx["/api/v1/namespaces/prod/pods"]; ok {
		t.Error("the original, unaliased path is still present in the index")
	}

	// Now check every body.
	readBody := func(apiPath string) map[string]interface{} {
		data, err := ar.ReadRecord(apiPath, 0)
		if err != nil {
			t.Fatalf("ReadRecord(%q, 0): %v", apiPath, err)
		}
		var rec capture.Record
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("unmarshaling record: %v", err)
		}
		if rec.APIPath != apiPath {
			t.Errorf("record's own APIPath = %q, want %q — it must match the index key it's filed under", rec.APIPath, apiPath)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
			t.Fatalf("unmarshaling body: %v", err)
		}
		return obj
	}

	nsObj := readBody("/api/v1/namespaces/" + wantAlias)
	if got := nsObj["metadata"].(map[string]interface{})["name"]; got != wantAlias {
		t.Errorf("Namespace object metadata.name = %v, want %v", got, wantAlias)
	}

	podList := readBody("/api/v1/namespaces/" + wantAlias + "/pods")
	podItems := podList["items"].([]interface{})
	podNS := podItems[0].(map[string]interface{})["metadata"].(map[string]interface{})["namespace"]
	if podNS != wantAlias {
		t.Errorf("Pod metadata.namespace = %v, want %v", podNS, wantAlias)
	}

	eventList := readBody("/api/v1/namespaces/" + wantAlias + "/events")
	eventItem := eventList["items"].([]interface{})[0].(map[string]interface{})
	eventNS := eventItem["metadata"].(map[string]interface{})["namespace"]
	involvedNS := eventItem["involvedObject"].(map[string]interface{})["namespace"]
	if eventNS != wantAlias {
		t.Errorf("Event metadata.namespace = %v, want %v", eventNS, wantAlias)
	}
	if involvedNS != wantAlias {
		t.Errorf("Event involvedObject.namespace = %v, want %v", involvedNS, wantAlias)
	}
}

func indexKeys(idx capture.Index) []string {
	out := make([]string, 0, len(idx))
	for k := range idx {
		out = append(out, k)
	}
	return out
}

// The output archive's own metadata must record that it was anonymized,
// and with which categories — mirroring Redacted/SecretsRedacted's
// identical provenance pattern (internal/redact) — so a later `kshrk
// inspect` or the UI's capture-info card can say so without guessing.
func TestArchive_SetsAnonymizedProvenanceMetadata(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	if _, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNamespace, CategoryNode},
		Salt:       []byte("provenance-test-salt"),
	}); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	defer ar.Close()
	meta, err := ar.ReadMetadata()
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !meta.Anonymized {
		t.Error("meta.Anonymized = false, want true")
	}
	want := []string{"namespace", "node"}
	if len(meta.AnonymizedCategories) != len(want) {
		t.Fatalf("AnonymizedCategories = %v, want %v", meta.AnonymizedCategories, want)
	}
	for i := range want {
		if meta.AnonymizedCategories[i] != want[i] {
			t.Errorf("AnonymizedCategories[%d] = %q, want %q", i, meta.AnonymizedCategories[i], want[i])
		}
	}
}

// The sharpest regression risk named in the design plan: proving the output
// archive is actually *readable* through the same store package the rest of
// the CLI and the UI use, not just that its JSON bodies look right in
// isolation. A bug that rewrote the body's metadata.namespace but missed the
// index key (or vice versa) would pass a body-only check and fail here,
// exactly as a real "kshrk open" on the output would.
func TestArchive_RoundTripReadableViaStore(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	salt := []byte("round-trip-salt")
	if _, err := Archive(src, dst, Options{Categories: []Category{CategoryNamespace}, Salt: salt}); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	defer ar.Close()
	cs, err := store.LoadStore(ar)
	if err != nil {
		t.Fatalf("store.LoadStore: %v", err)
	}
	defer cs.Close()

	wantAlias := NewAliaser(salt).Alias(CategoryNamespace, "prod")
	anonymizedPath := "/api/v1/namespaces/" + wantAlias + "/pods"

	body, code, _, err := cs.SnapshotAt(anonymizedPath, fixedNow.Add(time.Second))
	if err != nil {
		t.Fatalf("SnapshotAt(%q): %v", anonymizedPath, err)
	}
	if code != 200 {
		t.Fatalf("SnapshotAt(%q) returned code %d, want 200 — the store could not resolve the anonymized path", anonymizedPath, code)
	}
	var podList map[string]interface{}
	if err := json.Unmarshal(body, &podList); err != nil {
		t.Fatalf("unmarshaling snapshot body: %v", err)
	}
	items, _ := podList["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("got %d items via the store, want 1", len(items))
	}

	// And the pre-anonymization path must be genuinely gone, not just
	// shadowed — a 404 through the same store API a real reader would use.
	_, code, _, err = cs.SnapshotAt("/api/v1/namespaces/prod/pods", fixedNow.Add(time.Second))
	if err != nil {
		t.Fatalf("SnapshotAt(original path): %v", err)
	}
	if code != 404 {
		t.Errorf("SnapshotAt(original path) returned code %d, want 404 — the original namespace path should no longer resolve", code)
	}
}

// Found missing against a real cluster capture, not constructed from first
// principles: a namespace-lifecycle Event's involvedObject.kind is
// "Namespace", and namespace.go's Event handling previously only ever
// aliased a reference's "namespace" field, never its "name" field — the
// one a Namespace reference actually needs, since a Namespace has no
// membership of its own. This end-to-end-checks the fix through the full
// Archive() path, not just rewriteNamespaceInObject in isolation
// (namespace_test.go covers that).
func TestArchive_EventNamespaceReferenceAliasesName(t *testing.T) {
	eventListBody := `{"kind":"EventList","apiVersion":"v1","items":[
		{"metadata":{"name":"prod.abc","namespace":"prod"},
		 "involvedObject":{"kind":"Namespace","name":"prod"},
		 "message":"Namespace prod is active"}
	]}`
	// Appended under the *same* path as namespaceFixtureRecords' own events
	// record (real Kubernetes API paths never have an "events2" resource
	// type) — buildAnonymizeTestArchive assigns sequence numbers per path,
	// so this lands as seq 1 alongside the fixture's own seq-0 record, the
	// same way a real capture accumulates multiple polls of one endpoint.
	records := append(namespaceFixtureRecords(), &capture.Record{
		ID: "r5", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/events",
		HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(eventListBody),
	})
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("event-namespace-reference-test-salt")

	if _, err := Archive(src, dst, Options{Categories: []Category{CategoryNamespace}, Salt: salt}); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	wantAlias := NewAliaser(salt).Alias(CategoryNamespace, "prod")
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	defer ar.Close()
	data, err := ar.ReadRecord("/api/v1/namespaces/"+wantAlias+"/events", 1)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var eventList map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &eventList); err != nil {
		t.Fatal(err)
	}
	involved := eventList["items"].([]interface{})[0].(map[string]interface{})["involvedObject"].(map[string]interface{})
	if got := involved["name"]; got != wantAlias {
		t.Errorf("involvedObject.name = %v, want %v — the real bug: this leaked the unaliased namespace name", got, wantAlias)
	}
}

// Running Archive twice over the identical source with the identical salt
// must produce byte-identical output — this is the property the whole
// single-pass, stateless design in alias.go exists to guarantee. Comparing
// the two output *archives'* raw bytes (not just re-deriving an alias and
// checking it matches) also exercises internal/archive's own
// reproducible-ZIP-entry-timestamp behavior (epochModTime) as a side effect,
// which is what makes this comparison meaningful rather than incidentally
// flaky.
func TestArchive_Deterministic(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	salt := []byte("determinism-salt")

	dst1 := filepath.Join(t.TempDir(), "out1.kshrk")
	dst2 := filepath.Join(t.TempDir(), "out2.kshrk")

	if _, err := Archive(src, dst1, Options{Categories: []Category{CategoryNamespace}, Salt: salt}); err != nil {
		t.Fatalf("first Archive run: %v", err)
	}
	if _, err := Archive(src, dst2, Options{Categories: []Category{CategoryNamespace}, Salt: salt}); err != nil {
		t.Fatalf("second Archive run: %v", err)
	}

	b1, err := os.ReadFile(dst1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(dst2)
	if err != nil {
		t.Fatal(err)
	}
	if len(b1) != len(b2) {
		t.Fatalf("outputs differ in length: %d vs %d bytes", len(b1), len(b2))
	}
	for i := range b1 {
		if b1[i] != b2[i] {
			t.Fatalf("outputs diverge at byte offset %d", i)
		}
	}
}

// A real, found-not-constructed collision: "ns-763" and "ns-880" both alias
// to "namespace-large-cheetah-coral" under this exact salt (found by
// brute-force search over the namespace category's real HMAC-based Aliaser —
// it took ~881 draws, consistent with collision.go's birthday-bound math for
// the namespace category's 64x64x64=262144-combination, 3-word encoding —
// see #359, which bumped namespace from 2 to 3 words). This exercises the
// real, wired-up collisionTracker end to end through Archive(), not just the
// tracker in isolation (collision_test.go) with an injected colliding
// function — a bug in how the tracker is *wired into* the two index loops
// would not necessarily show up there.
const (
	collidingNamespaceA    = "ns-763"
	collidingNamespaceB    = "ns-880"
	collidingNamespaceSalt = "fixed-test-salt-for-collision-search"
)

func TestArchive_DetectsRealNamespaceCollision(t *testing.T) {
	nsBody := func(name string) string {
		return fmt.Sprintf(`{"kind":"Namespace","apiVersion":"v1","metadata":{"name":%q}}`, name)
	}
	records := []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/" + collidingNamespaceA, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nsBody(collidingNamespaceA))},
		{ID: "r2", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/" + collidingNamespaceB, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nsBody(collidingNamespaceB))},
	}
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	_, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNamespace},
		Salt:       []byte(collidingNamespaceSalt),
	})
	if err == nil {
		t.Fatal("want a collision error; got none — a real archive with a genuine alias collision was silently accepted")
	}
	for _, want := range []string{collidingNamespaceA, collidingNamespaceB} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the colliding value %q", err.Error(), want)
		}
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("no (corrupt) output file should be left behind on a detected collision")
	}
}

// The same collision, but introduced purely by record *body* content, with
// no per-namespace URL segment anywhere for rewriteNamespaceInPath to have
// already caught it: a single /api/v1/namespaces NamespaceList response
// listing both colliding names in items[].metadata.name. This is also the
// only (hence last) apiPath in the archive.
//
// This is the specific gap a real review caught: the collision check
// originally sat only between the *path* rewrite and the *body* rewrite for
// each entry, so a collision introduced by body content wasn't observed
// until the next loop iteration — and not at all if, as here, there is no
// next iteration. Archive() would report success despite having detected a
// real collision. Confirmed against the code before fixing: reverting to
// only the path-adjacent check reproduces this exact false success.
func TestArchive_DetectsCollisionIntroducedOnlyByRecordBody(t *testing.T) {
	body := fmt.Sprintf(`{"kind":"NamespaceList","apiVersion":"v1","items":[
		{"metadata":{"name":%q}},
		{"metadata":{"name":%q}}
	]}`, collidingNamespaceA, collidingNamespaceB)
	rec := &capture.Record{
		ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces",
		HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body),
	}
	src := buildAnonymizeTestArchive(t, []*capture.Record{rec})
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	_, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNamespace},
		Salt:       []byte(collidingNamespaceSalt),
	})
	if err == nil {
		t.Fatal("want a collision error; got none — a collision introduced only by record body content, on the archive's only apiPath, was silently accepted")
	}
	for _, want := range []string{collidingNamespaceA, collidingNamespaceB} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the colliding value %q", err.Error(), want)
		}
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("no (corrupt) output file should be left behind on a detected collision")
	}
}

func TestArchive_RejectsEmptySalt(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	_, err := Archive(src, dst, Options{Categories: []Category{CategoryNamespace}, Salt: nil})
	if err == nil {
		t.Fatal("want an error for an empty salt")
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("no output file should be left behind on a rejected empty salt")
	}
}

// buildAnonymizeEncryptedTestArchive mirrors buildAnonymizeTestArchive but
// writes the source as an age-encrypted archive, its own metadata.Encrypted
// set true (as a real capture/redact/encrypt run would leave it) — needed to
// exercise the plaintext-source-to-encrypted-output direction, and the
// reverse, for the meta.Encrypted regression below.
func buildAnonymizeEncryptedTestArchive(t *testing.T, records []*capture.Record, recipients []age.Recipient) string {
	t.Helper()
	dir := t.TempDir()

	idx := capture.Index{}
	pathSeq := map[string]int{}
	for _, r := range records {
		seq := pathSeq[r.APIPath]
		pathSeq[r.APIPath] = seq + 1
		e := idx[r.APIPath]
		if e == nil {
			e = &capture.IndexEntry{APIPath: r.APIPath}
			idx[r.APIPath] = e
		}
		e.Seqs = append(e.Seqs, seq)
		e.Times = append(e.Times, r.CapturedAt)
	}

	meta := &capture.CaptureMetadata{
		CaptureID:     "anonymize-encrypted-test",
		CapturedAt:    fixedNow.Add(-time.Minute),
		CapturedUntil: fixedNow,
		RecordCount:   len(records),
		Encrypted:     true,
	}

	outPath := filepath.Join(dir, "test.kshrk")
	sw, err := archive.NewEncryptedStreamWriter(outPath, recipients)
	if err != nil {
		t.Fatalf("NewEncryptedStreamWriter: %v", err)
	}
	defer func() { _ = sw.Abort() }() // no-op after a successful Finish
	for _, r := range records {
		if _, err := sw.WriteRecord(r); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return outPath
}

// readOutputMetadataEncrypted opens dst (assumed plaintext) and returns its
// own metadata.Encrypted value.
func readOutputMetadataEncrypted(t *testing.T, dst string) bool {
	t.Helper()
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("archive.Open(%s): %v", dst, err)
	}
	defer ar.Close()
	meta, err := ar.ReadMetadata()
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	return meta.Encrypted
}

// meta is populated straight from the source archive's own metadata
// (ar.ReadMetadata()), so left untouched it describes the *source*, not the
// archive actually being written. Both directions of the resulting mismatch
// are real: a plaintext source anonymized straight into an encrypted output
// would under-report itself as unencrypted, and — the more concerning
// direction, checked second — an encrypted source anonymized into a
// plaintext output (no --encrypt-* flags on the CLI) would over-report
// itself as still encrypted, which could give a consumer false confidence
// about a file that is, in fact, sitting on disk in the clear.
func TestArchive_MetadataEncryptedReflectsOutputNotSource(t *testing.T) {
	t.Run("plaintext source, encrypted output -> Encrypted true", func(t *testing.T) {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		src := buildAnonymizeTestArchive(t, namespaceFixtureRecords()) // plaintext, Encrypted: false
		dst := filepath.Join(t.TempDir(), "out.kshrk")

		_, err = Archive(src, dst, Options{
			Categories: []Category{CategoryNamespace},
			Salt:       []byte("salt"),
			Recipients: []age.Recipient{id.Recipient()},
		})
		if err != nil {
			t.Fatalf("Archive: %v", err)
		}

		// The output is encrypted, so open it directly rather than through
		// readOutputMetadataEncrypted (which assumes plaintext).
		ar, err := archive.OpenWithIdentities(dst, []age.Identity{id})
		if err != nil {
			t.Fatalf("OpenWithIdentities: %v", err)
		}
		defer ar.Close()
		meta, err := ar.ReadMetadata()
		if err != nil {
			t.Fatal(err)
		}
		if !meta.Encrypted {
			t.Error("output metadata.Encrypted = false, want true — the output actually is encrypted")
		}
	})

	t.Run("encrypted source, plaintext output -> Encrypted false", func(t *testing.T) {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		src := buildAnonymizeEncryptedTestArchive(t, namespaceFixtureRecords(), []age.Recipient{id.Recipient()})
		dst := filepath.Join(t.TempDir(), "out.kshrk")

		_, err = Archive(src, dst, Options{
			Categories: []Category{CategoryNamespace},
			Salt:       []byte("salt"),
			Identities: []age.Identity{id}, // decrypt the source
			// No Recipients: the output is written plaintext.
		})
		if err != nil {
			t.Fatalf("Archive: %v", err)
		}

		if got := readOutputMetadataEncrypted(t, dst); got {
			t.Error("output metadata.Encrypted = true, want false — the output is plaintext, only the source was encrypted")
		}
	})
}

// As of M4 (#137), every real Category constant is supported by Archive()'s
// archiveCategories — there's no longer a real, still-unsupported category
// to exercise this with (unlike the M2/M3 version of this test, which used
// CategoryIP before this milestone implemented it). A made-up category
// string still needs to be rejected, though — this is the same guard,
// tested against the only kind of value that can still trigger it.
func TestArchive_UnsupportedCategoryErrors(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	_, err := Archive(src, dst, Options{Categories: []Category{Category("bogus")}, Salt: []byte("s")})
	if err == nil {
		t.Fatal("want an error for a category not supported by archive rewriting")
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("no output file should be left behind on a rejected category")
	}
}

func TestArchive_NoCategoriesErrors(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	if _, err := Archive(src, dst, Options{Salt: []byte("s")}); err == nil {
		t.Fatal("want an error when no categories are requested")
	}
}

// Fields that have nothing to do with namespaces (a ConfigMap's own name and
// data) must survive with their values unchanged — proving the rewrite is
// surgical, not a side effect of decoding and re-marshaling every record
// through Go's map-based JSON representation. This checks semantic
// preservation after decode, not byte-for-byte identity of the record: the
// record's own api_path field is expected to change (its namespace segment
// is aliased, same as every other record's), and a decode/re-encode round
// trip doesn't preserve exact key order or number formatting anyway — this
// test would not (and should not) catch either of those.
func TestArchive_PreservesUnrelatedRecordUntouched(t *testing.T) {
	cmBody := `{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":"settings","namespace":"prod"},"data":{"key":"value","count":"42"}}`
	records := append(namespaceFixtureRecords(), &capture.Record{
		ID: "r4", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/configmaps/settings",
		HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(cmBody),
	})
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("preserve-salt")

	if _, err := Archive(src, dst, Options{Categories: []Category{CategoryNamespace}, Salt: salt}); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	wantAlias := NewAliaser(salt).Alias(CategoryNamespace, "prod")
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	data, err := ar.ReadRecord("/api/v1/namespaces/"+wantAlias+"/configmaps/settings", 0)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
		t.Fatal(err)
	}
	cmData := obj["data"].(map[string]interface{})
	if cmData["key"] != "value" || cmData["count"] != "42" {
		t.Errorf("ConfigMap data was modified: %v", cmData)
	}
	if obj["metadata"].(map[string]interface{})["name"] != "settings" {
		t.Errorf("ConfigMap's own name was modified: %v", obj["metadata"])
	}
}

// resourceNameFixtureRecords builds a small but representative set of
// records covering all three M3 categories at once, plus namespace,
// mirroring namespaceFixtureRecords' role for M2: a Node (its own identity
// plus its Hostname address), a Pod living on that node and owned by a
// ReplicaSet, the ReplicaSet itself, and an Event about the Pod that also
// names the node it ran on.
func resourceNameFixtureRecords() []*capture.Record {
	nodeBody := `{"kind":"Node","apiVersion":"v1","metadata":{"name":"worker-1"},
		"status":{"addresses":[{"type":"InternalIP","address":"10.0.0.5"},{"type":"Hostname","address":"worker-1"}]}}`
	podListBody := `{"kind":"PodList","apiVersion":"v1","items":[
		{"metadata":{"name":"web-1","namespace":"prod","ownerReferences":[{"kind":"ReplicaSet","name":"web-rs"}]},
		 "spec":{"nodeName":"worker-1","containers":[{"name":"app"}]}}
	]}`
	rsBody := `{"kind":"ReplicaSet","apiVersion":"apps/v1","metadata":{"name":"web-rs","namespace":"prod"}}`
	eventListBody := `{"kind":"EventList","apiVersion":"v1","items":[
		{"metadata":{"name":"web-1.abc","namespace":"prod"},
		 "involvedObject":{"kind":"Pod","name":"web-1","namespace":"prod"},
		 "source":{"component":"kubelet","host":"worker-1"},
		 "message":"Started container app"}
	]}`
	return []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/nodes/worker-1", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nodeBody)},
		{ID: "r2", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podListBody)},
		{ID: "r3", CapturedAt: fixedNow, APIPath: "/apis/apps/v1/namespaces/prod/replicasets/web-rs", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(rsBody)},
		{ID: "r4", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/events", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(eventListBody)},
	}
}

// The M3 analogue of TestArchive_NamespaceConsistentAcrossKindsAndPaths: the
// same real node name, seen on the Node's own identity, a Hostname address,
// a Pod's spec.nodeName, and an Event's source.host, must come out as the
// same alias everywhere — and likewise for the pod name (its own identity
// plus an Event's involvedObject.name) and the workload name (the
// ReplicaSet's own identity, its APIPath's object-name segment, plus the
// Pod's ownerReferences[0].name). Runs all four categories in one pass, as
// the CLI's own --categories example now does.
func TestArchive_ResourceNamesConsistentAcrossKindsAndPaths(t *testing.T) {
	src := buildAnonymizeTestArchive(t, resourceNameFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	salt := []byte("resourcename-integration-test-salt")
	result, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNamespace, CategoryNode, CategoryPod, CategoryWorkload},
		Salt:       salt,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.NamespacesRenamed != 1 || result.NodesRenamed != 1 || result.PodsRenamed != 1 || result.WorkloadsRenamed != 1 {
		t.Errorf("counts = %+v, want 1 distinct value renamed per category", result)
	}

	a := NewAliaser(salt)
	nsAlias := a.Alias(CategoryNamespace, "prod")
	nodeAlias := a.Alias(CategoryNode, "worker-1")
	podAlias := a.Alias(CategoryPod, "web-1")
	workloadAlias := a.Alias(CategoryWorkload, "web-rs")

	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("opening output archive: %v", err)
	}
	defer ar.Close()

	idx, err := ar.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	readBody := func(apiPath string) map[string]interface{} {
		t.Helper()
		if _, ok := idx[apiPath]; !ok {
			t.Fatalf("index has no entry for %q (index keys: %v)", apiPath, indexKeys(idx))
		}
		data, err := ar.ReadRecord(apiPath, 0)
		if err != nil {
			t.Fatalf("ReadRecord(%q, 0): %v", apiPath, err)
		}
		var rec capture.Record
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("unmarshaling record: %v", err)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
			t.Fatalf("unmarshaling body: %v", err)
		}
		return obj
	}

	nodeObj := readBody("/api/v1/nodes/" + nodeAlias)
	if got := nodeObj["metadata"].(map[string]interface{})["name"]; got != nodeAlias {
		t.Errorf("Node metadata.name = %v, want %v", got, nodeAlias)
	}
	addrs := nodeObj["status"].(map[string]interface{})["addresses"].([]interface{})
	hostAddr := addrs[1].(map[string]interface{})
	if got := hostAddr["address"]; got != nodeAlias {
		t.Errorf("Node Hostname address = %v, want %v", got, nodeAlias)
	}

	podList := readBody("/api/v1/namespaces/" + nsAlias + "/pods")
	podItem := podList["items"].([]interface{})[0].(map[string]interface{})
	podMeta := podItem["metadata"].(map[string]interface{})
	if got := podMeta["name"]; got != podAlias {
		t.Errorf("Pod metadata.name = %v, want %v", got, podAlias)
	}
	if got := podItem["spec"].(map[string]interface{})["nodeName"]; got != nodeAlias {
		t.Errorf("Pod spec.nodeName = %v, want %v", got, nodeAlias)
	}
	ownerRef := podMeta["ownerReferences"].([]interface{})[0].(map[string]interface{})
	if got := ownerRef["name"]; got != workloadAlias {
		t.Errorf("Pod ownerReferences[0].name = %v, want %v", got, workloadAlias)
	}

	rsObj := readBody("/apis/apps/v1/namespaces/" + nsAlias + "/replicasets/" + workloadAlias)
	if got := rsObj["metadata"].(map[string]interface{})["name"]; got != workloadAlias {
		t.Errorf("ReplicaSet metadata.name = %v, want %v", got, workloadAlias)
	}

	eventList := readBody("/api/v1/namespaces/" + nsAlias + "/events")
	eventItem := eventList["items"].([]interface{})[0].(map[string]interface{})
	if got := eventItem["involvedObject"].(map[string]interface{})["name"]; got != podAlias {
		t.Errorf("Event involvedObject.name = %v, want %v", got, podAlias)
	}
	if got := eventItem["source"].(map[string]interface{})["host"]; got != nodeAlias {
		t.Errorf("Event source.host = %v, want %v", got, nodeAlias)
	}
}

// Requesting only --categories pod must not also alias node-category fields
// it happens to recognize (a Pod's spec.nodeName, a Node's own identity) —
// the enabled-gating unit-tested directly in resourcename_test.go, exercised
// here end to end through Archive().
func TestArchive_CategoryGatingRestrictsWhichFieldsChange(t *testing.T) {
	src := buildAnonymizeTestArchive(t, resourceNameFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("gating-test-salt")

	result, err := Archive(src, dst, Options{Categories: []Category{CategoryPod}, Salt: salt})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.NodesRenamed != 0 || result.WorkloadsRenamed != 0 || result.NamespacesRenamed != 0 {
		t.Errorf("counts = %+v, want only PodsRenamed set", result)
	}

	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()

	// The node's own path and identity must be untouched.
	data, err := ar.ReadRecord("/api/v1/nodes/worker-1", 0)
	if err != nil {
		t.Fatalf("ReadRecord(node): %v — the node path should not have been rewritten", err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var nodeObj map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &nodeObj); err != nil {
		t.Fatal(err)
	}
	if got := nodeObj["metadata"].(map[string]interface{})["name"]; got != "worker-1" {
		t.Errorf("Node metadata.name = %v, want unchanged worker-1", got)
	}

	// The Pod's own name is aliased, but its spec.nodeName (a node-category
	// occurrence) is not.
	podAlias := NewAliaser(salt).Alias(CategoryPod, "web-1")
	data, err = ar.ReadRecord("/api/v1/namespaces/prod/pods", 0)
	if err != nil {
		t.Fatalf("ReadRecord(pods): %v", err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var podList map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &podList); err != nil {
		t.Fatal(err)
	}
	podItem := podList["items"].([]interface{})[0].(map[string]interface{})
	if got := podItem["metadata"].(map[string]interface{})["name"]; got != podAlias {
		t.Errorf("Pod metadata.name = %v, want %v", got, podAlias)
	}
	if got := podItem["spec"].(map[string]interface{})["nodeName"]; got != "worker-1" {
		t.Errorf("Pod spec.nodeName = %v, want unchanged worker-1 — node category was not requested", got)
	}
}

// A real, found-not-constructed collision for the node category (the same
// approach as collidingNamespaceA/B, using CategoryNode's own 2-word
// encoding): "node-95" and "node-130" both alias to "node-noble-gull" under
// this exact salt. Proves the collisionTracker wiring generalizes to a
// category beyond namespace, not just that namespace's own wiring works.
const (
	collidingNodeA    = "node-95"
	collidingNodeB    = "node-130"
	collidingNodeSalt = "fixed-test-salt-for-collision-search"
)

func TestArchive_DetectsRealNodeCollision(t *testing.T) {
	nodeBody := func(name string) string {
		return fmt.Sprintf(`{"kind":"Node","apiVersion":"v1","metadata":{"name":%q}}`, name)
	}
	records := []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/nodes/" + collidingNodeA, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nodeBody(collidingNodeA))},
		{ID: "r2", CapturedAt: fixedNow, APIPath: "/api/v1/nodes/" + collidingNodeB, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nodeBody(collidingNodeB))},
	}
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	_, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNode},
		Salt:       []byte(collidingNodeSalt),
	})
	if err == nil {
		t.Fatal("want a collision error; got none — a real archive with a genuine alias collision was silently accepted")
	}
	for _, want := range []string{collidingNodeA, collidingNodeB} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the colliding value %q", err.Error(), want)
		}
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("no (corrupt) output file should be left behind on a detected collision")
	}
}

// The node-category analogue of
// TestArchive_DetectsCollisionIntroducedOnlyByRecordBody: both colliding
// names appear only inside a NodeList response's items[].metadata.name, on
// the archive's only (cluster-scoped, no per-object segment) apiPath —
// proving the shared firstCollisionErr() checkpoint placement catches a
// body-only collision for this category too, not just namespace's.
func TestArchive_DetectsNodeCollisionIntroducedOnlyByRecordBody(t *testing.T) {
	body := fmt.Sprintf(`{"kind":"NodeList","apiVersion":"v1","items":[
		{"metadata":{"name":%q}},
		{"metadata":{"name":%q}}
	]}`, collidingNodeA, collidingNodeB)
	rec := &capture.Record{
		ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/nodes",
		HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body),
	}
	src := buildAnonymizeTestArchive(t, []*capture.Record{rec})
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	_, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNode},
		Salt:       []byte(collidingNodeSalt),
	})
	if err == nil {
		t.Fatal("want a collision error; got none — a collision introduced only by record body content, on the archive's only apiPath, was silently accepted")
	}
	for _, want := range []string{collidingNodeA, collidingNodeB} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the colliding value %q", err.Error(), want)
		}
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("no (corrupt) output file should be left behind on a detected collision")
	}
}

// ipURLImageFixtureRecords covers all three M4 categories at once, plus a
// deliberate cross-record consistency case: the Node's own InternalIP and
// the Pod's status.podIP are the same real address, and the Ingress's own
// bare spec.rules[*].host and its annotation's embedded
// https://app.example.com/webhook are the same real hostname — one via the
// schema-aware whole-value path, the other via the full-tree scheme-splice
// path. Both pairs must come out as the same alias.
func ipURLImageFixtureRecords() []*capture.Record {
	nodeBody := `{"kind":"Node","apiVersion":"v1","metadata":{"name":"worker-1"},
		"status":{"addresses":[{"type":"InternalIP","address":"10.1.2.3"}]}}`
	podListBody := `{"kind":"PodList","apiVersion":"v1","items":[
		{"metadata":{"name":"web-1","namespace":"prod"},
		 "spec":{"containers":[{"name":"app","image":"registry.internal.corp/team/app:v1.2.3"}]},
		 "status":{"podIP":"10.1.2.3"}}
	]}`
	ingressBody := `{"kind":"Ingress","apiVersion":"networking.k8s.io/v1","metadata":{"name":"web","namespace":"prod",
		"annotations":{"webhook":"https://app.example.com/webhook"}},
		"spec":{"rules":[{"host":"app.example.com"}]}}`
	serviceBody := `{"kind":"Service","apiVersion":"v1","metadata":{"name":"db","namespace":"prod"},
		"spec":{"type":"ExternalName","externalName":"db.upstream.example.com"}}`
	return []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/nodes/worker-1", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nodeBody)},
		{ID: "r2", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podListBody)},
		{ID: "r3", CapturedAt: fixedNow, APIPath: "/apis/networking.k8s.io/v1/namespaces/prod/ingresses/web", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(ingressBody)},
		{ID: "r4", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/services/db", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(serviceBody)},
	}
}

// The M4 analogue of TestArchive_NamespaceConsistentAcrossKindsAndPaths and
// TestArchive_ResourceNamesConsistentAcrossKindsAndPaths: the same IP
// appearing on a Node's own address and a Pod's status.podIP, the same
// hostname appearing bare (Ingress spec.rules[*].host) and spliced out of a
// URL (an annotation), and the registry host inside a container image, must
// all come out as their category's consistent alias — and
// CaptureMetadata.ServerAddress, which lives outside any record body
// entirely, must be spliced through the same URL alias space.
func TestArchive_IPURLImageConsistentAcrossKindsAndPaths(t *testing.T) {
	src := buildAnonymizeTestArchive(t, ipURLImageFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	salt := []byte("ip-url-image-integration-test-salt")
	result, err := Archive(src, dst, Options{
		Categories: []Category{CategoryIP, CategoryURL, CategoryImage},
		Salt:       salt,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.IPsRenamed != 1 {
		t.Errorf("IPsRenamed = %d, want 1 (one distinct IP, seen on two records)", result.IPsRenamed)
	}
	// app.example.com, db.upstream.example.com, and the ServerAddress host
	// 127.0.0.1 (see buildAnonymizeTestArchive) — three distinct hosts.
	if result.HostsRenamed != 3 {
		t.Errorf("HostsRenamed = %d, want 3", result.HostsRenamed)
	}
	if result.RegistriesRenamed != 1 {
		t.Errorf("RegistriesRenamed = %d, want 1", result.RegistriesRenamed)
	}

	a := NewAliaser(salt)
	ipAlias := a.Alias(CategoryIP, "10.1.2.3")
	hostAlias := a.Alias(CategoryURL, "app.example.com")
	registryAlias := a.Alias(CategoryImage, "registry.internal.corp")
	serverAddrHostAlias := a.Alias(CategoryURL, "127.0.0.1")

	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("opening output archive: %v", err)
	}
	defer ar.Close()

	meta, err := ar.ReadMetadata()
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	wantServerAddress := "https://" + serverAddrHostAlias + ":6443"
	if meta.ServerAddress != wantServerAddress {
		t.Errorf("ServerAddress = %q, want %q", meta.ServerAddress, wantServerAddress)
	}

	readBody := func(apiPath string) map[string]interface{} {
		t.Helper()
		data, err := ar.ReadRecord(apiPath, 0)
		if err != nil {
			t.Fatalf("ReadRecord(%q, 0): %v", apiPath, err)
		}
		var rec capture.Record
		if err := json.Unmarshal(data, &rec); err != nil {
			t.Fatalf("unmarshaling record: %v", err)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
			t.Fatalf("unmarshaling body: %v", err)
		}
		return obj
	}

	nodeObj := readBody("/api/v1/nodes/worker-1")
	nodeAddr := nodeObj["status"].(map[string]interface{})["addresses"].([]interface{})[0].(map[string]interface{})
	if got := nodeAddr["address"]; got != ipAlias {
		t.Errorf("Node InternalIP = %v, want %v", got, ipAlias)
	}

	podList := readBody("/api/v1/namespaces/prod/pods")
	podItem := podList["items"].([]interface{})[0].(map[string]interface{})
	if got := podItem["status"].(map[string]interface{})["podIP"]; got != ipAlias {
		t.Errorf("Pod status.podIP = %v, want %v — must match the Node's own address alias", got, ipAlias)
	}
	image := podItem["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["image"]
	if got, want := image, registryAlias+"/team/app:v1.2.3"; got != want {
		t.Errorf("container image = %v, want %v", got, want)
	}

	ingressObj := readBody("/apis/networking.k8s.io/v1/namespaces/prod/ingresses/web")
	if got := ingressObj["spec"].(map[string]interface{})["rules"].([]interface{})[0].(map[string]interface{})["host"]; got != hostAlias {
		t.Errorf("Ingress spec.rules[0].host = %v, want %v", got, hostAlias)
	}
	annotation := ingressObj["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})["webhook"]
	if got, want := annotation, "https://"+hostAlias+"/webhook"; got != want {
		t.Errorf("Ingress webhook annotation = %v, want %v — the spliced host must match the bare host field's own alias", got, want)
	}
}

// Requesting only --categories ip must not also alias URL/image-category
// occurrences it happens to be structurally adjacent to.
func TestArchive_IPURLImageCategoryGatingRestrictsWhichFieldsChange(t *testing.T) {
	src := buildAnonymizeTestArchive(t, ipURLImageFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("ip-url-image-gating-test-salt")

	result, err := Archive(src, dst, Options{Categories: []Category{CategoryIP}, Salt: salt})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.HostsRenamed != 0 || result.RegistriesRenamed != 0 {
		t.Errorf("counts = %+v, want only IPsRenamed set", result)
	}

	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()

	meta, err := ar.ReadMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if meta.ServerAddress != "https://127.0.0.1:6443" {
		t.Errorf("ServerAddress = %q, want unchanged — URL category was not requested", meta.ServerAddress)
	}

	data, err := ar.ReadRecord("/apis/networking.k8s.io/v1/namespaces/prod/ingresses/web", 0)
	if err != nil {
		t.Fatal(err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var ingressObj map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &ingressObj); err != nil {
		t.Fatal(err)
	}
	if got := ingressObj["spec"].(map[string]interface{})["rules"].([]interface{})[0].(map[string]interface{})["host"]; got != "app.example.com" {
		t.Errorf("Ingress host = %v, want unchanged app.example.com", got)
	}
}

// A real, found-not-constructed collision for the url category (proving the
// tracker wiring works for the substring-splicing category, not just the
// whole-value-swap categories already covered): "host-21.example.com" and
// "host-133.example.com" both alias to "url-clean-lemur" under this exact
// salt.
const (
	collidingURLA    = "host-21.example.com"
	collidingURLB    = "host-133.example.com"
	collidingURLSalt = "fixed-test-salt-for-collision-search"
)

func TestArchive_DetectsRealURLCollision(t *testing.T) {
	svcBody := func(host string) string {
		return fmt.Sprintf(`{"kind":"Service","apiVersion":"v1","metadata":{"name":"svc"},"spec":{"type":"ExternalName","externalName":%q}}`, host)
	}
	records := []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/a/services/svc", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(svcBody(collidingURLA))},
		{ID: "r2", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/b/services/svc", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(svcBody(collidingURLB))},
	}
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	_, err := Archive(src, dst, Options{
		Categories: []Category{CategoryURL},
		Salt:       []byte(collidingURLSalt),
	})
	if err == nil {
		t.Fatal("want a collision error; got none — a real archive with a genuine alias collision was silently accepted")
	}
	for _, want := range []string{collidingURLA, collidingURLB} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the colliding value %q", err.Error(), want)
		}
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("no (corrupt) output file should be left behind on a detected collision")
	}
}

// An AnonymizeRule excluding a Namespace object's own metadata.name must
// leave that specific occurrence alone while every other namespace
// occurrence in the same archive still gets aliased normally — proving the
// exclude wiring reaches all the way from Options.Rules through
// newExcludeMatcher into rewriteNamespaceInObject's actual field-write
// site, not just that the matcher itself works in isolation
// (rules_test.go already covers that). The path rewrite
// (rewriteNamespaceInPath) is a separate, unconditional mechanism not
// gated by rules at all — only the body's own metadata.name field is
// excluded here.
func TestArchive_RuleExcludesSpecificFieldPath(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("rule-exclude-test-salt")

	result, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNamespace},
		Salt:       salt,
		Rules: []config.AnonymizeRule{
			{Category: "namespace", Kind: "Namespace", FieldPath: "metadata.name", Exclude: true},
		},
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// The Pod's metadata.namespace and the Event's namespace occurrences are
	// NOT excluded (different fieldPath/kind), so "prod" is still seen and
	// counted there — only the Namespace object's own identity is skipped.
	if result.NamespacesRenamed != 1 {
		t.Errorf("NamespacesRenamed = %d, want 1 (still counted via the Pod/Event occurrences)", result.NamespacesRenamed)
	}

	wantAlias := NewAliaser(salt).Alias(CategoryNamespace, "prod")
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("opening output archive: %v", err)
	}
	defer ar.Close()

	data, err := ar.ReadRecord("/api/v1/namespaces/"+wantAlias, 0)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var nsObj map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &nsObj); err != nil {
		t.Fatal(err)
	}
	if got := nsObj["metadata"].(map[string]interface{})["name"]; got != "prod" {
		t.Errorf("Namespace metadata.name = %v, want unchanged prod — excluded by rule", got)
	}

	// The Pod's metadata.namespace, a different fieldPath, is still aliased.
	data, err = ar.ReadRecord("/api/v1/namespaces/"+wantAlias+"/pods", 0)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var podList map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &podList); err != nil {
		t.Fatal(err)
	}
	podNS := podList["items"].([]interface{})[0].(map[string]interface{})["metadata"].(map[string]interface{})["namespace"]
	if podNS != wantAlias {
		t.Errorf("Pod metadata.namespace = %v, want %v — not excluded by this rule", podNS, wantAlias)
	}
}

// A rule scoped to a Kind that never occurs in the archive must be a
// complete no-op — proving Kind scoping is exact, not accidentally broad.
func TestArchive_RuleScopedToWrongKindDoesNotExclude(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("rule-wrong-kind-test-salt")

	_, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNamespace},
		Salt:       salt,
		Rules: []config.AnonymizeRule{
			// Same fieldPath, but scoped to a Kind that never appears —
			// must not accidentally suppress the real Namespace occurrence.
			{Category: "namespace", Kind: "ConfigMap", FieldPath: "metadata.name", Exclude: true},
		},
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	wantAlias := NewAliaser(salt).Alias(CategoryNamespace, "prod")
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("opening output archive: %v", err)
	}
	defer ar.Close()
	data, err := ar.ReadRecord("/api/v1/namespaces/"+wantAlias, 0)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var nsObj map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &nsObj); err != nil {
		t.Fatal(err)
	}
	if got := nsObj["metadata"].(map[string]interface{})["name"]; got != wantAlias {
		t.Errorf("Namespace metadata.name = %v, want %v — the rule's Kind doesn't match, so it must not exclude", got, wantAlias)
	}
}

// A rule excluding a full-tree-scanned category's occurrence (IP) by its
// computed field path, proving the walkStrings path-tracking added for
// exclusion actually reaches the IP/URL full-tree scans, not just the
// schema-aware categories' fixed field-write sites.
func TestArchive_RuleExcludesFullTreeIPOccurrence(t *testing.T) {
	body := `{"kind":"Pod","metadata":{"name":"web-1"},"status":{"podIP":"10.1.2.3","hostIP":"10.9.9.9"}}`
	rec := &capture.Record{
		ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/pods/web-1",
		HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body),
	}
	src := buildAnonymizeTestArchive(t, []*capture.Record{rec})
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("rule-exclude-ip-test-salt")

	_, err := Archive(src, dst, Options{
		Categories: []Category{CategoryIP},
		Salt:       salt,
		Rules: []config.AnonymizeRule{
			{Category: "ip", Kind: "Pod", FieldPath: "status.podIP", Exclude: true},
		},
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("opening output archive: %v", err)
	}
	defer ar.Close()
	data, err := ar.ReadRecord("/api/v1/namespaces/prod/pods/web-1", 0)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	var outRec capture.Record
	if err := json.Unmarshal(data, &outRec); err != nil {
		t.Fatal(err)
	}
	var podObj map[string]interface{}
	if err := json.Unmarshal(outRec.ResponseBody, &podObj); err != nil {
		t.Fatal(err)
	}
	status := podObj["status"].(map[string]interface{})
	if got := status["podIP"]; got != "10.1.2.3" {
		t.Errorf("status.podIP = %v, want unchanged 10.1.2.3 — excluded by rule", got)
	}
	if got := status["hostIP"]; got == "10.9.9.9" {
		t.Error("status.hostIP was not aliased — the exclude rule for status.podIP must not also suppress a different field path")
	}
}

func TestArchive_RejectsInvalidRule(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	_, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNamespace},
		Salt:       []byte("s"),
		Rules: []config.AnonymizeRule{
			{Category: "namespace", FieldPath: "metadata.name", Exclude: false},
		},
	})
	if err == nil {
		t.Fatal("want an error for a rule with Exclude: false")
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("no output file should be left behind on a rejected rule")
	}
}
