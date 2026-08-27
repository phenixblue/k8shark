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

// A real, found-not-constructed collision: "ns-66" and "ns-106" both alias to
// "namespace-green-lynx" under this exact salt (found by brute-force search
// over the namespace category's real HMAC-based Aliaser — it took only ~106
// draws, consistent with collision.go's birthday-bound math for a
// 64x64=4096-combination space). This exercises the real, wired-up
// collisionTracker end to end through Archive(), not just the tracker in
// isolation (collision_test.go) with an injected colliding function — a bug
// in how the tracker is *wired into* the two index loops would not
// necessarily show up there.
const (
	collidingNamespaceA    = "ns-66"
	collidingNamespaceB    = "ns-106"
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

func TestArchive_UnsupportedCategoryErrors(t *testing.T) {
	src := buildAnonymizeTestArchive(t, namespaceFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")

	_, err := Archive(src, dst, Options{Categories: []Category{CategoryIP}, Salt: []byte("s")})
	if err == nil {
		t.Fatal("want an error for a category not yet supported by archive rewriting")
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
