package anonymize

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/config"
)

// Reproduces #358 against Archive() end-to-end: an unrecognized CRD (a
// Portworx-style StorageNode, standing in for any vendor's CRD) references a
// real Node name via a Kind kindCategories doesn't know about, so
// rewriteResourceNameInObject's schema-aware path can't recognize it — only
// the full sweep, which doesn't care about field names or Kinds at all, can
// catch it.
func TestArchive_FullSweepCatchesUnrecognizedCRDNameReference(t *testing.T) {
	nodeBody := `{"kind":"Node","apiVersion":"v1","metadata":{"name":"worker-1"}}`
	// StorageNode is not a Kind kindCategories recognizes (resourcename.go's
	// own doc comment documents this exact gap), so nothing schema-aware
	// ever looks at its "name" field.
	storageNodeBody := `{"kind":"StorageNode","apiVersion":"portworx.io/v1","metadata":{"name":"px-storagenode"},"nodeReference":"worker-1"}`
	records := []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/nodes/worker-1", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nodeBody)},
		{ID: "r2", CapturedAt: fixedNow, APIPath: "/apis/portworx.io/v1/storagenodes/px-storagenode", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(storageNodeBody)},
	}
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("crd-name-gap-test-salt")

	result, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNode},
		Salt:       salt,
		FullSweep:  true,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.SweepOccurrencesFound == 0 {
		t.Error("want SweepOccurrencesFound > 0 — the StorageNode's nodeReference should have been caught")
	}

	wantAlias := NewAliaser(salt).Alias(CategoryNode, "worker-1")
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	defer ar.Close()
	data, err := ar.ReadRecord("/apis/portworx.io/v1/storagenodes/px-storagenode", 0)
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
	if got := obj["nodeReference"]; got != wantAlias {
		t.Errorf("nodeReference = %v, want %v (the real node name %q must not leak)", got, wantAlias, "worker-1")
	}
}

// Reproduces #360 against Archive() end-to-end: a
// kubectl.kubernetes.io/last-applied-configuration-shaped annotation is an
// escaped JSON *string*, not real nested structure — invisible to every
// schema-aware matcher and to the full-tree IP matcher (which only ever
// replaces a whole leaf value, not a substring of one). Only the sweep,
// which scans the annotation's string content directly, can catch it.
func TestArchive_FullSweepCatchesLastAppliedConfigurationLeak(t *testing.T) {
	// The annotation value is itself a JSON-encoded string containing the
	// namespace "prod" and the IP "10.1.2.3" — exactly the shape kubectl
	// apply writes.
	lastApplied := `{"apiVersion":"v1","kind":"Pod","metadata":{"namespace":"prod"},"status":{"hostIP":"10.1.2.3"}}`
	lastAppliedJSON, err := json.Marshal(lastApplied)
	if err != nil {
		t.Fatal(err)
	}
	podBody := `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"web-1","namespace":"prod","annotations":{"kubectl.kubernetes.io/last-applied-configuration":` + string(lastAppliedJSON) + `}},"status":{"podIP":"10.1.2.3"}}`
	records := []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/pods/web-1", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podBody)},
	}
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("last-applied-config-test-salt")

	result, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNamespace, CategoryIP},
		Salt:       salt,
		FullSweep:  true,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.SweepOccurrencesFound == 0 {
		t.Error("want SweepOccurrencesFound > 0 — the annotation's embedded namespace and IP should have been caught")
	}

	nsAlias := NewAliaser(salt).Alias(CategoryNamespace, "prod")
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	defer ar.Close()
	data, err := ar.ReadRecord("/api/v1/namespaces/"+nsAlias+"/pods/web-1", 0)
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
	annotation := obj["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})["kubectl.kubernetes.io/last-applied-configuration"].(string)
	if annotation == lastApplied {
		t.Fatal("last-applied-configuration annotation was not touched at all")
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(annotation), &decoded); err != nil {
		t.Fatalf("annotation is no longer valid JSON after sweeping: %v (%q)", err, annotation)
	}
	if got := decoded["metadata"].(map[string]interface{})["namespace"]; got != nsAlias {
		t.Errorf("annotation's embedded namespace = %v, want %v — the real value %q must not leak", got, nsAlias, "prod")
	}
	if got := decoded["status"].(map[string]interface{})["hostIP"]; got == "10.1.2.3" {
		t.Error("annotation's embedded IP was not aliased — the real value must not leak")
	}
}

// A namespace and a pod that happen to share the same real name are
// genuinely ambiguous for the sweep (see buildSweepCandidates) — this must
// be left untouched by the sweep (not guessed at), while the schema-aware
// matchers still correctly alias each in its own known field.
func TestArchive_FullSweepSkipsAmbiguousCrossCategoryValue(t *testing.T) {
	nsBody := `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"shared"}}`
	podBody := `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"shared","namespace":"shared"},"status":{"message":"pod shared is running in namespace shared"}}`
	records := []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/shared", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nsBody)},
		{ID: "r2", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/shared/pods/shared", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podBody)},
	}
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("ambiguous-value-test-salt")

	result, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNamespace, CategoryPod},
		Salt:       salt,
		FullSweep:  true,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.SweepAmbiguousSkipped == 0 {
		t.Error("want SweepAmbiguousSkipped > 0 — \"shared\" is a candidate under both namespace and pod")
	}

	nsAlias := NewAliaser(salt).Alias(CategoryNamespace, "shared")
	podAlias := NewAliaser(salt).Alias(CategoryPod, "shared")
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	defer ar.Close()
	data, err := ar.ReadRecord("/api/v1/namespaces/"+nsAlias+"/pods/"+podAlias, 0)
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
	// The schema-aware matchers still correctly alias the pod's own
	// metadata.name/namespace (proven by the ReadRecord path above
	// resolving at all) — only the free-text status.message mention of the
	// ambiguous value "shared" must be left completely untouched by the
	// sweep.
	status := obj["status"].(map[string]interface{})
	if got := status["message"]; got != "pod shared is running in namespace shared" {
		t.Errorf("status.message = %q, want the ambiguous value left untouched by the sweep", got)
	}
}

// A field-path exclusion rule must still be honored by the sweep, exactly
// as it already is by every schema-aware/full-tree matcher.
func TestArchive_FullSweepRespectsExcludeRules(t *testing.T) {
	podBody := `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"web-1","namespace":"prod"},"status":{"message":"scheduled onto namespace prod"}}`
	records := []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/pods/web-1", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podBody)},
	}
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("exclude-rule-sweep-test-salt")

	result, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNamespace},
		Salt:       salt,
		FullSweep:  true,
		Rules: []config.AnonymizeRule{
			{Category: "namespace", FieldPath: "status.message", Kind: "Pod", Exclude: true},
		},
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.SweepOccurrencesFound != 0 {
		t.Errorf("SweepOccurrencesFound = %d, want 0 — the only sweep-eligible occurrence is excluded", result.SweepOccurrencesFound)
	}

	nsAlias := NewAliaser(salt).Alias(CategoryNamespace, "prod")
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	defer ar.Close()
	data, err := ar.ReadRecord("/api/v1/namespaces/"+nsAlias+"/pods/web-1", 0)
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
	status := obj["status"].(map[string]interface{})
	if got := status["message"]; got != "scheduled onto namespace prod" {
		t.Errorf("status.message = %q, want it left untouched by the excluded sweep rule", got)
	}
}

// Running Archive twice with FullSweep and the same source+salt must
// produce byte-identical output — extends TestArchive_Deterministic's
// existing coverage to the sweep pass specifically.
func TestArchive_FullSweepDeterministic(t *testing.T) {
	records := append(namespaceFixtureRecords(), &capture.Record{
		ID: "r5", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/configmaps/notes",
		HTTPMethod: "GET", ResponseCode: 200,
		ResponseBody: json.RawMessage(`{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":"notes","namespace":"prod"},"data":{"notes":"reminder: prod namespace is scheduled for maintenance"}}`),
	})
	src := buildAnonymizeTestArchive(t, records)
	dst1 := filepath.Join(t.TempDir(), "out1.kshrk")
	dst2 := filepath.Join(t.TempDir(), "out2.kshrk")
	salt := []byte("full-sweep-determinism-test-salt")

	opts := Options{Categories: []Category{CategoryNamespace}, Salt: salt, FullSweep: true}
	if _, err := Archive(src, dst1, opts); err != nil {
		t.Fatalf("first Archive run: %v", err)
	}
	if _, err := Archive(src, dst2, opts); err != nil {
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

// FullSweep defaults to false, and must leave behavior byte-identical to
// before this feature existed: zero sweep counts, and a free-text mention
// of a value that only the sweep could catch stays untouched.
func TestArchive_WithoutFullSweepLeavesFreeTextUntouched(t *testing.T) {
	podBody := `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"web-1","namespace":"prod"},"status":{"message":"scheduled onto namespace prod"}}`
	records := []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/pods/web-1", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podBody)},
	}
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("no-full-sweep-test-salt")

	result, err := Archive(src, dst, Options{Categories: []Category{CategoryNamespace}, Salt: salt})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.SweepOccurrencesFound != 0 || result.SweepAmbiguousSkipped != 0 {
		t.Errorf("want both sweep counts 0 when FullSweep is false, got SweepOccurrencesFound=%d SweepAmbiguousSkipped=%d",
			result.SweepOccurrencesFound, result.SweepAmbiguousSkipped)
	}

	nsAlias := NewAliaser(salt).Alias(CategoryNamespace, "prod")
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	defer ar.Close()
	data, err := ar.ReadRecord("/api/v1/namespaces/"+nsAlias+"/pods/web-1", 0)
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
	status := obj["status"].(map[string]interface{})
	if got := status["message"]; got != "scheduled onto namespace prod" {
		t.Errorf("status.message = %q, want the free-text mention left untouched without FullSweep", got)
	}
}

// CategoryImage is the one archiveCategories entry that's never sweep-
// eligible (sweep.go's buildSweepCandidates never adds an image-category
// candidate — see its own package doc). Requesting --full-sweep with only
// --categories image must still succeed as a clean no-op: this is the
// Archive-level guarantee behind cmd/anonymize.go's own "--full-sweep has no
// effect" warning for exactly this case — the run must not pay for a wasted
// collection pass, and it must not error just because no category could
// ever contribute a sweep candidate.
func TestArchive_FullSweepNoOpWhenNoCategoryIsSweepEligible(t *testing.T) {
	podBody := `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"web-1","namespace":"prod"},
		"spec":{"containers":[{"name":"app","image":"registry.internal.corp/team/app:v1"}]},
		"status":{"message":"scheduled onto namespace prod"}}`
	records := []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/pods/web-1", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podBody)},
	}
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("image-only-full-sweep-test-salt")

	result, err := Archive(src, dst, Options{
		Categories: []Category{CategoryImage},
		Salt:       salt,
		FullSweep:  true,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.SweepOccurrencesFound != 0 || result.SweepAmbiguousSkipped != 0 {
		t.Errorf("want both sweep counts 0 — image is never sweep-eligible, got SweepOccurrencesFound=%d SweepAmbiguousSkipped=%d",
			result.SweepOccurrencesFound, result.SweepAmbiguousSkipped)
	}
	if result.RegistriesRenamed != 1 {
		t.Errorf("RegistriesRenamed = %d, want 1 — the image category itself must still run normally", result.RegistriesRenamed)
	}

	// The namespace mention in status.message is exactly the kind of
	// free-text occurrence only a sweep could catch — but namespace wasn't
	// requested at all, so this also confirms the run did real work (the
	// image rewrite) without the sweep's collection pass silently
	// populating a namespace candidate some other path never asked for.
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	defer ar.Close()
	data, err := ar.ReadRecord("/api/v1/namespaces/prod/pods/web-1", 0)
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
	status := obj["status"].(map[string]interface{})
	if got := status["message"]; got != "scheduled onto namespace prod" {
		t.Errorf("status.message = %q, want the free-text namespace mention left untouched", got)
	}
}

// End-to-end reproduction of a real gap found in manual testing, through
// Archive() itself rather than sweepRecord in isolation — and exactly
// docs/config.md's own documented exclude-rule example ("Leave a specific
// Node's own identity alone, but still alias its appearance elsewhere"):
// a captured Node GET and its corresponding "?as=Table" response (what
// `kubectl get nodes` actually renders from) both mention the same node
// name. Before sweepRecord resolved a Table row's real Kind from its
// record's own APIPath, the rule's `kind: Node` could never match an
// occurrence inside the Table response (sweepRecord always reported
// kind="Table" there), so --full-sweep silently re-aliased the Table's
// rows[*].object.metadata.name despite the user's explicit exclusion —
// even though the identical rule correctly protected the plain Node GET's
// own metadata.name the whole time.
func TestArchive_FullSweepRespectsKindScopedExcludeRuleInsideTableResponse(t *testing.T) {
	// The Node's own metadata.name is excluded, so nothing schema-aware ever
	// aliases *that* occurrence — but a Pod's spec.nodeName (not excluded)
	// is a recognized node-category field too, and is what actually gets
	// "worker-1" discovered as a sweep candidate in the first place.
	nodeBody := `{"kind":"Node","apiVersion":"v1","metadata":{"name":"worker-1","labels":{"kubernetes.io/hostname":"worker-1"}}}`
	podBody := `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"web-1","namespace":"default"},"spec":{"nodeName":"worker-1"}}`
	tableBody := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
		"columnDefinitions":[{"name":"Name","description":"the node's name"}],
		"rows":[{"cells":["worker-1"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",
			"metadata":{"name":"worker-1"}}}]}`
	records := []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/nodes/worker-1", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nodeBody)},
		{ID: "r2", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/default/pods/web-1", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podBody)},
		{ID: "r3", CapturedAt: fixedNow, APIPath: "/api/v1/nodes?as=Table", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(tableBody)},
	}
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("table-exclude-rule-test-salt")

	result, err := Archive(src, dst, Options{
		Categories: []Category{CategoryNode},
		Salt:       salt,
		FullSweep:  true,
		Rules: []config.AnonymizeRule{
			{Category: "node", Kind: "Node", FieldPath: "metadata.name", Exclude: true},
		},
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if result.SweepOccurrencesFound == 0 {
		t.Error("want SweepOccurrencesFound > 0 — the label and the Table's cells should still have been caught")
	}

	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	defer ar.Close()

	// The single-object GET path's own trailing name segment is aliased by
	// rewriteResourceNameInPath regardless of the body-field exclude rule
	// (the exclude rule is scoped to a body fieldPath, not the APIPath) —
	// same convention every other end-to-end test in this file follows.
	nodeAlias := NewAliaser(salt).Alias(CategoryNode, "worker-1")
	nodeData, err := ar.ReadRecord("/api/v1/nodes/"+nodeAlias, 0)
	if err != nil {
		t.Fatalf("ReadRecord (Node GET): %v", err)
	}
	var nodeRec capture.Record
	if err := json.Unmarshal(nodeData, &nodeRec); err != nil {
		t.Fatal(err)
	}
	var nodeObj map[string]interface{}
	if err := json.Unmarshal(nodeRec.ResponseBody, &nodeObj); err != nil {
		t.Fatal(err)
	}
	if got := nodeObj["metadata"].(map[string]interface{})["name"]; got != "worker-1" {
		t.Errorf("plain Node GET metadata.name = %v, want the real value left untouched by the exclude rule", got)
	}

	tableData, err := ar.ReadRecord("/api/v1/nodes?as=Table", 0)
	if err != nil {
		t.Fatalf("ReadRecord (Table): %v", err)
	}
	var tableRec capture.Record
	if err := json.Unmarshal(tableData, &tableRec); err != nil {
		t.Fatal(err)
	}
	var tableObj map[string]interface{}
	if err := json.Unmarshal(tableRec.ResponseBody, &tableObj); err != nil {
		t.Fatal(err)
	}
	row := tableObj["rows"].([]interface{})[0].(map[string]interface{})
	rowObjectName := row["object"].(map[string]interface{})["metadata"].(map[string]interface{})["name"]
	if rowObjectName != "worker-1" {
		t.Errorf("Table rows[0].object.metadata.name = %v, want the real value left untouched — this is the exact field the reported gap mangled", rowObjectName)
	}
	// Not covered by the rule (it only names fieldPath metadata.name): the
	// printed cell and the unrelated hostname label are still aliased,
	// proving the exclude rule is honored narrowly, not by accidentally
	// suppressing the whole sweep for this Kind/category.
	if cell := row["cells"].([]interface{})[0]; cell == "worker-1" {
		t.Error("Table rows[0].cells[0] was left untouched, want it still aliased — the rule doesn't cover fieldPath cells[*]")
	}
	if label := nodeObj["metadata"].(map[string]interface{})["labels"].(map[string]interface{})["kubernetes.io/hostname"]; label == "worker-1" {
		t.Error("Node metadata.labels[kubernetes.io/hostname] was left untouched, want it still aliased")
	}
}
