package v2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
)

// ── normalizeObjectBody (pure) ───────────────────────────────────────────────
// These cement the apiVersion/kind restoration and casing fixes: Kubernetes
// omits apiVersion/kind from objects nested in a List response, so the object
// view must put them back.

func TestNormalizeObjectBody_CoreResourceFromPath(t *testing.T) {
	raw := json.RawMessage(`{"metadata":{"name":"p"}}`)
	got := normalizeObjectBody(raw, "/api/v1/namespaces/default/pods", "", "")
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["apiVersion"] != "v1" {
		t.Errorf("apiVersion = %v, want v1", obj["apiVersion"])
	}
	if obj["kind"] != "Pod" {
		t.Errorf("kind = %v, want Pod", obj["kind"])
	}
}

func TestNormalizeObjectBody_GroupedResourceFromPath(t *testing.T) {
	raw := json.RawMessage(`{"metadata":{"name":"d"}}`)
	got := normalizeObjectBody(raw, "/apis/apps/v1/namespaces/default/deployments", "", "")
	var obj map[string]any
	_ = json.Unmarshal(got, &obj)
	if obj["apiVersion"] != "apps/v1" {
		t.Errorf("apiVersion = %v, want apps/v1", obj["apiVersion"])
	}
	if obj["kind"] != "Deployment" {
		t.Errorf("kind = %v, want Deployment", obj["kind"])
	}
}

func TestNormalizeObjectBody_KindHintPreservesCasing(t *testing.T) {
	// The path-based fallback would mangle this to "Mutatingwebhookconfiguration";
	// the List-envelope hint preserves the real casing.
	raw := json.RawMessage(`{"metadata":{"name":"catalogd"}}`)
	path := "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations"
	got := normalizeObjectBody(raw, path, "admissionregistration.k8s.io/v1", "MutatingWebhookConfiguration")
	var obj map[string]any
	_ = json.Unmarshal(got, &obj)
	if obj["kind"] != "MutatingWebhookConfiguration" {
		t.Errorf("kind = %v, want MutatingWebhookConfiguration", obj["kind"])
	}
	if obj["apiVersion"] != "admissionregistration.k8s.io/v1" {
		t.Errorf("apiVersion = %v, want admissionregistration.k8s.io/v1", obj["apiVersion"])
	}
}

func TestNormalizeObjectBody_DoesNotOverwriteExisting(t *testing.T) {
	raw := json.RawMessage(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p"}}`)
	got := normalizeObjectBody(raw, "/api/v1/namespaces/default/pods", "wrong/v9", "WrongKind")
	var obj map[string]any
	_ = json.Unmarshal(got, &obj)
	if obj["apiVersion"] != "v1" || obj["kind"] != "Pod" {
		t.Errorf("existing fields overwritten: apiVersion=%v kind=%v", obj["apiVersion"], obj["kind"])
	}
}

func TestNormalizeObjectBody_LeavesListAndTableEnvelopes(t *testing.T) {
	list := json.RawMessage(`{"kind":"PodList","items":[{"metadata":{"name":"p"}}]}`)
	if got := normalizeObjectBody(list, "/api/v1/namespaces/default/pods", "", ""); string(got) != string(list) {
		t.Errorf("List envelope modified: %s", got)
	}
	table := json.RawMessage(`{"kind":"Table","rows":[]}`)
	if got := normalizeObjectBody(table, "/api/v1/namespaces/default/pods", "", ""); string(got) != string(table) {
		t.Errorf("Table envelope modified: %s", got)
	}
}

// ── serveObject (HTTP handler) ───────────────────────────────────────────────

func TestServeObject_ExtractsItemAndRestoresTypeMeta(t *testing.T) {
	h := newObjectTestHandler(t)
	rec := doObject(t, h, "/api/v1/namespaces/default/pods", "demo-pod")

	if !rec.Found {
		t.Fatalf("expected Found=true, got %+v", rec)
	}
	if rec.Kind != "Pod" {
		t.Errorf("Kind = %q, want Pod", rec.Kind)
	}
	if rec.Namespace != "default" {
		t.Errorf("Namespace = %q, want default", rec.Namespace)
	}
	if !strings.Contains(rec.YAML, "apiVersion: v1") || !strings.Contains(rec.YAML, "kind: Pod") {
		t.Errorf("YAML missing restored typemeta:\n%s", rec.YAML)
	}
}

func TestServeObject_PreservesKindCasingFromListEnvelope(t *testing.T) {
	h := newObjectTestHandler(t)
	// ReplicaSet would be mangled to "Replicaset" by name-based inference; the
	// "ReplicaSetList" envelope kind preserves the correct casing.
	rec := doObject(t, h, "/apis/apps/v1/namespaces/default/replicasets", "demo-rs")
	if !rec.Found {
		t.Fatalf("expected Found=true")
	}
	if rec.Kind != "ReplicaSet" {
		t.Errorf("Kind = %q, want ReplicaSet (casing from List envelope)", rec.Kind)
	}
	if !strings.Contains(rec.YAML, "kind: ReplicaSet") {
		t.Errorf("YAML kind not correctly cased:\n%s", rec.YAML)
	}
}

func TestServeObject_NotFound(t *testing.T) {
	h := newObjectTestHandler(t)
	rec := doObject(t, h, "/api/v1/namespaces/default/pods", "does-not-exist")
	if rec.Found {
		t.Errorf("expected Found=false for missing name")
	}
}

func TestServeObject_MissingPath(t *testing.T) {
	h := newObjectTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v2/api/object?name=x", nil)
	w := httptest.NewRecorder()
	h.serveObject(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func doObject(t *testing.T, h *Handler, path, name string) ObjectDetail {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v2/api/object", nil)
	q := req.URL.Query()
	q.Set("path", path)
	q.Set("name", name)
	req.URL.RawQuery = q.Encode()
	w := httptest.NewRecorder()
	h.serveObject(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var rec ObjectDetail
	if err := json.Unmarshal(w.Body.Bytes(), &rec); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return rec
}

func newObjectTestHandler(t *testing.T) *Handler {
	t.Helper()
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)

	// Items intentionally omit top-level apiVersion/kind, as Kubernetes does
	// inside List responses.
	podList := `{"apiVersion":"v1","kind":"PodList","items":[` +
		`{"metadata":{"name":"demo-pod","namespace":"default"},"spec":{"containers":[{"name":"main"}]},"status":{"phase":"Running"}}]}`
	rsList := `{"apiVersion":"apps/v1","kind":"ReplicaSetList","items":[` +
		`{"metadata":{"name":"demo-rs","namespace":"default"},"spec":{"replicas":1}}]}`

	recs := []*capture.Record{
		{ID: "r1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podList)},
		{ID: "r2", CapturedAt: now, APIPath: "/apis/apps/v1/namespaces/default/replicasets", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(rsList)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/pods":              {APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}, Times: []time.Time{now}},
		"/apis/apps/v1/namespaces/default/replicasets": {APIPath: "/apis/apps/v1/namespaces/default/replicasets", Seqs: []int{0}, Times: []time.Time{now}},
	}
	meta := &capture.CaptureMetadata{CaptureID: "v2-obj-test", CapturedAt: now.Add(-5 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	store := buildV2TestStore(t, recs, idx, meta)
	return &Handler{Store: store, At: now}
}

// buildV2TestStore writes records to a temp archive and loads a CaptureStore,
// mirroring the helper used by the legacy UI tests.
func buildV2TestStore(t *testing.T, recs []*capture.Record, idx capture.Index, meta *capture.CaptureMetadata) *server.CaptureStore {
	t.Helper()
	out := filepath.Join(t.TempDir(), "capture.kshrk")
	sw, err := archive.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	for _, r := range recs {
		if err := sw.WriteRecord(r); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ar, err := archive.Open(out)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })
	store, err := server.LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}
