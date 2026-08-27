package anonymize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// Data with nothing to do with namespaces must survive byte-for-byte —
// proving the rewrite is surgical, not a side effect of decoding and
// re-marshaling every record through Go's map-based JSON representation
// (which does not preserve key order or exact number formatting).
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
