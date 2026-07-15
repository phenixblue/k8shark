package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

func TestHandler_Version(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	assertJSON(t, rw, 200, "gitVersion", store.Metadata.KubernetesVersion)
}

func TestHandler_Healthz(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)
	for _, path := range []string{"/healthz", "/readyz", "/livez"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, rw.Code)
		}
	}
}

func TestHandler_GetPods(t *testing.T) {
	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(podList),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "PodList") {
		t.Errorf("expected PodList in body, got: %s", body)
	}
}

func TestHandler_APIVersions(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	assertJSON(t, rw, 200, "kind", "APIVersions")
}

func TestHandler_APIGroupList(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/apis/apps/v1/namespaces/default/deployments": []byte(`{"kind":"DeploymentList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/apis", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	assertJSON(t, rw, 200, "kind", "APIGroupList")
}

func TestHandler_CoreResourceList(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	assertJSON(t, rw, 200, "kind", "APIResourceList")
}

func TestHandler_GroupResourceList(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/apis/apps/v1/namespaces/default/deployments": []byte(`{"kind":"DeploymentList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/apis/apps/v1", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	assertJSON(t, rw, 200, "kind", "APIResourceList")
}

// TestHandler_NotFound_ListPath verifies that a list-level resource not in the capture
// returns 200 + empty list + Warning header rather than a 404 error.
func TestHandler_NotFound_ListPath(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/services", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	if w := rw.Header().Get("Warning"); w == "" {
		t.Error("expected Warning header, got none")
	}
	var body map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body: %v", err)
	}
	if body["kind"] != "ServiceList" {
		t.Errorf("expected kind ServiceList, got %v", body["kind"])
	}
}

// TestHandler_NotFound_ItemPath verifies that an item-level GET for a resource whose
// parent list is not in the capture still returns a 404.
func TestHandler_NotFound_ItemPath(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{})
	h := newHandler(store, time.Time{}, false)

	// 6-segment path (item-level): parseAPIPath returns empty resource → 404.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/services/my-svc", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
}

// TestHandler_NotFound_ItemPath_StandardStatus verifies an item-level GET's
// 404 body is a standard Kubernetes NotFound Status (reason, message,
// details) rather than the k8shark-specific "not found in capture" message —
// so client-go's apierrors.IsNotFound() recognizes it (#177).
func TestHandler_NotFound_ItemPath_StandardStatus(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/apis/networking.k8s.io/v1/namespaces/default/ingresses/nope", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
	var status struct {
		Kind    string `json:"kind"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
		Details struct {
			Name  string `json:"name"`
			Group string `json:"group"`
			Kind  string `json:"kind"`
		} `json:"details"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &status); err != nil {
		t.Fatalf("expected JSON body: %v", err)
	}
	if status.Kind != "Status" || status.Status != "Failure" || status.Reason != "NotFound" {
		t.Errorf("status = %+v, want kind=Status status=Failure reason=NotFound", status)
	}
	if status.Message != `ingresses.networking.k8s.io "nope" not found` {
		t.Errorf("message = %q, want %q", status.Message, `ingresses.networking.k8s.io "nope" not found`)
	}
	if status.Details.Name != "nope" || status.Details.Group != "networking.k8s.io" || status.Details.Kind != "ingresses" {
		t.Errorf("details = %+v, want name=nope group=networking.k8s.io kind=ingresses", status.Details)
	}
}

// TestHandler_NotFound_ItemPath_CoreGroupStatus verifies the core group
// (empty group string) omits ".group" from the message and "group" from
// details, matching the real apiserver's GroupResource.String() format.
func TestHandler_NotFound_ItemPath_CoreGroupStatus(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/nope", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
	}
	var status struct {
		Reason  string         `json:"reason"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &status); err != nil {
		t.Fatalf("expected JSON body: %v", err)
	}
	if status.Reason != "NotFound" {
		t.Errorf("reason = %q, want NotFound", status.Reason)
	}
	if status.Message != `pods "nope" not found` {
		t.Errorf("message = %q, want %q", status.Message, `pods "nope" not found`)
	}
	if _, hasGroup := status.Details["group"]; hasGroup {
		t.Errorf("details should omit \"group\" for the core group, got %v", status.Details)
	}
}

// TestHandler_NotFound_ListPath_KnownResourceNoWarning verifies a resource
// listed in a captured discovery document, but with zero captured objects,
// returns an empty 200 list with no Warning header — it behaves like a real
// cluster's empty live collection, not a misconfigured/unknown capture
// (#177). Contrast with TestHandler_NotFound_ListPath, where the resource is
// genuinely absent from discovery too.
func TestHandler_NotFound_ListPath_KnownResourceNoWarning(t *testing.T) {
	discoveryBody := `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"networking.k8s.io/v1","resources":[` +
		`{"name":"ingresses","singularName":"ingress","namespaced":true,"kind":"Ingress"}]}`
	store := buildTestStore(t, map[string][]byte{
		"/apis/networking.k8s.io/v1": []byte(discoveryBody),
	})
	store.discoveryEnrichmentDone.Wait()
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/apis/networking.k8s.io/v1/namespaces/default/ingresses", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	if w := rw.Header().Get("Warning"); w != "" {
		t.Errorf("expected no Warning header for a known resource, got %q", w)
	}
	var body map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body: %v", err)
	}
	if body["kind"] != "IngressList" {
		t.Errorf("expected kind IngressList, got %v", body["kind"])
	}
	items, _ := body["items"].([]any)
	if len(items) != 0 {
		t.Errorf("expected empty items, got %v", items)
	}
}

func TestHandler_SingleItemGet(t *testing.T) {
	podList := `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"nginx","namespace":"default"}},{"metadata":{"name":"redis","namespace":"default"}}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(podList),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/nginx", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 for single item GET, got %d\nbody: %s", rw.Code, rw.Body.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if meta, ok := obj["metadata"].(map[string]any); !ok || meta["name"] != "nginx" {
		t.Errorf("expected metadata.name=nginx, got: %v", obj)
	}
}

func TestHandler_Watch(t *testing.T) {
	podList := `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"nginx"}}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(podList),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods?watch=1", nil)
	// Use a context so the watch handler exits promptly.
	ctx := req.Context()
	ctx, cancel := cancelableContext(ctx)
	req = req.WithContext(ctx)
	rw := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rw, req)
	}()
	// Cancel after the handler has a chance to write.
	cancel()
	<-done

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 for watch, got %d", rw.Code)
	}
	lines := strings.Split(strings.TrimSpace(rw.Body.String()), "\n")
	var hasAdded, hasBookmark bool
	for _, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "ADDED":
			hasAdded = true
		case "BOOKMARK":
			hasBookmark = true
			if obj, ok := ev["object"].(map[string]any); ok {
				if meta, ok := obj["metadata"].(map[string]any); ok {
					rv, _ := meta["resourceVersion"].(string)
					if rv == "" || rv == "0" {
						t.Errorf("BOOKMARK resourceVersion should be non-zero, got %q", rv)
					}
				}
				// BOOKMARK kind must match the resource kind, not "Status"
				if kind, _ := obj["kind"].(string); kind == "Status" || kind == "" {
					t.Errorf("BOOKMARK object kind should be the resource kind, got %q", kind)
				}
			}
		}
	}
	if !hasAdded {
		t.Errorf("expected at least one ADDED event in watch response")
	}
	if !hasBookmark {
		t.Errorf("expected BOOKMARK event in watch response")
	}
}

// assertJSON decodes the response body and checks that result[key] == wantVal.
func assertJSON(t *testing.T, rw *httptest.ResponseRecorder, wantCode int, key, wantVal string) {
	t.Helper()
	if rw.Code != wantCode {
		t.Fatalf("expected status %d, got %d\nbody: %s", wantCode, rw.Code, rw.Body.String())
	}
	body, _ := io.ReadAll(rw.Body)
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("JSON decode failed: %v\nbody: %s", err, body)
	}
	if result[key] != wantVal {
		t.Errorf("expected %s=%q, got %v", key, wantVal, result[key])
	}
}

func TestHandler_LabelSelector(t *testing.T) {
	podList := listWithPods([]podSpec{
		{name: "nginx", labels: map[string]string{"app": "nginx"}},
		{name: "redis", labels: map[string]string{"app": "redis"}},
	})
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": podList,
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods?labelSelector=app%3Dnginx", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	names := itemNames(t, rw.Body.Bytes())
	if len(names) != 1 || names[0] != "nginx" {
		t.Errorf("expected [nginx], got %v", names)
	}
}

func TestHandler_FieldSelector(t *testing.T) {
	podList := listWithPods([]podSpec{
		{name: "nginx", namespace: "default", labels: nil},
		{name: "redis", namespace: "default", labels: nil},
	})
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": podList,
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods?fieldSelector=metadata.name%3Dnginx", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	names := itemNames(t, rw.Body.Bytes())
	if len(names) != 1 || names[0] != "nginx" {
		t.Errorf("expected [nginx], got %v", names)
	}
}

func TestHandler_ReplayAtTimestamp(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test.kshrk")

	path := "/api/v1/namespaces/default/pods"
	t1 := time.Date(2026, 4, 9, 10, 40, 0, 0, time.UTC)
	t2 := t1.Add(2 * time.Minute)
	records := []capture.Record{
		{ID: "rec-1", CapturedAt: t1, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(`{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"before"}}]}`)},
		{ID: "rec-2", CapturedAt: t2, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(`{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"after"}}]}`)},
	}

	sw, err := archive.NewStreamWriter(outPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		rcopy := rec
		if err := sw.WriteRecord(&rcopy); err != nil {
			t.Fatal(err)
		}
	}
	idx := capture.Index{
		path: {APIPath: path, Seqs: []int{0, 1}, Times: []time.Time{t1, t2}},
	}
	meta := capture.CaptureMetadata{CaptureID: "test-capture-id", CapturedAt: t1, CapturedUntil: t2, RecordCount: len(records)}
	if err := sw.Finish(&meta, idx, nil); err != nil {
		t.Fatal(err)
	}

	ar, err := archive.Open(outPath)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })

	store, err := LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	h := newHandler(store, t1.Add(time.Minute), false)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "before") {
		t.Fatalf("expected point-in-time replay to return first record, got %s", rw.Body.String())
	}
}

func TestHandler_Watch_TimeoutSeconds(t *testing.T) {
	podList := `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"nginx"}}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(podList),
	})
	h := newHandler(store, time.Time{}, false)
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/api/v1/namespaces/default/pods?watch=1&timeoutSeconds=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Read events until EOF; the server must close the stream after ~1 s.
	deadline := time.Now().Add(5 * time.Second)
	var hasAdded, hasBookmark bool
	dec := json.NewDecoder(resp.Body)
	for time.Now().Before(deadline) {
		var ev map[string]any
		if err := dec.Decode(&ev); err != nil {
			// io.EOF means the server closed the watch — that's the success case.
			break
		}
		switch ev["type"] {
		case "ADDED":
			hasAdded = true
		case "BOOKMARK":
			hasBookmark = true
		}
	}
	if time.Now().After(deadline) {
		t.Error("watch stream did not close within 5 s; timeoutSeconds not honored")
	}
	if !hasAdded {
		t.Error("expected at least one ADDED event")
	}
	if !hasBookmark {
		t.Error("expected a BOOKMARK event")
	}
}

func TestHandler_Watch_AggregateAcrossNamespacesPath(t *testing.T) {
	svcList := `{"apiVersion":"v1","kind":"ServiceList","metadata":{"resourceVersion":"11"},"items":[{"apiVersion":"v1","kind":"Service","metadata":{"name":"nginx","namespace":"default"}}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/services": []byte(svcList),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/services?watch=1", nil)
	ctx, cancel := cancelableContext(req.Context())
	req = req.WithContext(ctx)
	rw := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rw, req)
	}()
	cancel()
	<-done

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 for aggregated watch, got %d", rw.Code)
	}
	lines := strings.Split(strings.TrimSpace(rw.Body.String()), "\n")
	var hasAdded, hasBookmark bool
	for _, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "ADDED":
			hasAdded = true
		case "BOOKMARK":
			hasBookmark = true
		}
	}
	if !hasAdded {
		t.Error("expected ADDED event for aggregated services watch")
	}
	if !hasBookmark {
		t.Error("expected BOOKMARK event for aggregated services watch")
	}
}

func TestHandler_Watch_MissingListPathReturnsEmptyStream(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/secrets?watch=1", nil)
	ctx, cancel := cancelableContext(req.Context())
	req = req.WithContext(ctx)
	rw := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rw, req)
	}()
	cancel()
	<-done

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200 for missing list path watch fallback, got %d", rw.Code)
	}
	lines := strings.Split(strings.TrimSpace(rw.Body.String()), "\n")
	var hasAdded, hasBookmark bool
	for _, line := range lines {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "ADDED":
			hasAdded = true
		case "BOOKMARK":
			hasBookmark = true
		}
	}
	if hasAdded {
		t.Error("did not expect ADDED events for missing list path watch fallback")
	}
	if !hasBookmark {
		t.Error("expected BOOKMARK event for missing list path watch fallback")
	}
}

// TestHandler_WriteMethodsReturn405 verifies that POST, PUT, PATCH, and DELETE
// all receive a 405 Method Not Allowed response with a Status JSON body, while
// a regular GET to the same path is not blocked.
func TestHandler_WriteMethodsReturn405(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	writeMethods := []string{
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
	}
	path := "/api/v1/namespaces/default/pods"

	for _, method := range writeMethods {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, path, nil)
			rw := httptest.NewRecorder()
			h.ServeHTTP(rw, req)

			if rw.Code != http.StatusMethodNotAllowed {
				t.Fatalf("expected 405, got %d", rw.Code)
			}
			// RFC 7231 §6.5.5 requires Allow header on 405 responses.
			if allow := rw.Header().Get("Allow"); allow == "" {
				t.Error("expected Allow header, got none")
			}
			var body map[string]any
			if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
				t.Fatalf("expected JSON body: %v", err)
			}
			if code, _ := body["code"].(float64); int(code) != http.StatusMethodNotAllowed {
				t.Errorf("expected body.code=405, got %v", body["code"])
			}
		})
	}

	// GET must still work normally.
	t.Run("GET_allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code == http.StatusMethodNotAllowed {
			t.Fatalf("GET should not return 405")
		}
	})
}

func TestHandler_SelfSubjectAccessReview_Compatibility(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", strings.NewReader(`{"kind":"SelfSubjectAccessReview"}`))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rw.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body: %v", err)
	}
	if body["kind"] != "SelfSubjectAccessReview" {
		t.Fatalf("expected kind SelfSubjectAccessReview, got %v", body["kind"])
	}
	status, _ := body["status"].(map[string]any)
	if allowed, _ := status["allowed"].(bool); !allowed {
		t.Fatalf("expected status.allowed=true, got %v", status["allowed"])
	}
}

func TestHandler_SelfSubjectRulesReview_Compatibility(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectrulesreviews", strings.NewReader(`{"kind":"SelfSubjectRulesReview"}`))
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rw.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body: %v", err)
	}
	if body["kind"] != "SelfSubjectRulesReview" {
		t.Fatalf("expected kind SelfSubjectRulesReview, got %v", body["kind"])
	}
	status, _ := body["status"].(map[string]any)
	if incomplete, _ := status["incomplete"].(bool); incomplete {
		t.Fatalf("expected status.incomplete=false, got true")
	}
}

func TestHandler_InteractiveSubResourcesReturn405(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	// These sub-resource paths must always return 405 regardless of method,
	// with a message that explicitly references k8shark capture replay.
	subResources := []string{
		"/api/v1/namespaces/default/pods/mypod/exec",
		"/api/v1/namespaces/default/pods/mypod/portforward",
		"/api/v1/namespaces/default/pods/mypod/attach",
	}
	for _, sr := range subResources {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(method+"_"+sr, func(t *testing.T) {
				req := httptest.NewRequest(method, sr, nil)
				rw := httptest.NewRecorder()
				h.ServeHTTP(rw, req)

				if rw.Code != http.StatusMethodNotAllowed {
					t.Fatalf("%s %s: expected 405, got %d", method, sr, rw.Code)
				}
				body := rw.Body.String()
				if !strings.Contains(body, "k8shark") {
					t.Errorf("%s %s: expected error message to mention k8shark, got: %s", method, sr, body)
				}
			})
		}
	}
}

func TestHandler_ServeLog_Captured(t *testing.T) {
	// Log bodies are stored as JSON strings in the archive.
	logContent := "line1\nline2\nline3\n"
	logJSON, _ := json.Marshal(logContent)
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods/mypod/log": logJSON,
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/mypod/log", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	if ct := rw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("expected text/plain Content-Type, got %q", ct)
	}
	if got := rw.Body.String(); got != logContent {
		t.Errorf("expected log body %q, got %q", logContent, got)
	}
}

func TestHandler_ServeLog_NotCaptured(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/mypod/log", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	// Should return 200 with a stub message — not 404 or an error.
	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	body := rw.Body.String()
	if !strings.Contains(body, "k8shark") {
		t.Errorf("expected stub message to mention k8shark, got: %q", body)
	}
	if !strings.Contains(body, "not captured") {
		t.Errorf("expected stub to mention 'not captured', got: %q", body)
	}
}

// TestHandler_ServeLog_PerContainer verifies that `kubectl logs <pod> -c <c>`
// (which the API client encodes as ?container=<c>) is routed to the
// matching per-container record.
func TestHandler_ServeLog_PerContainer(t *testing.T) {
	webLog, _ := json.Marshal("web container log\n")
	sidecarLog, _ := json.Marshal("sidecar container log\n")
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods/mypod/log?container=web":     webLog,
		"/api/v1/namespaces/default/pods/mypod/log?container=sidecar": sidecarLog,
	})
	h := newHandler(store, time.Time{}, false)

	for container, want := range map[string]string{
		"web":     "web container log\n",
		"sidecar": "sidecar container log\n",
	} {
		req := httptest.NewRequest(http.MethodGet,
			"/api/v1/namespaces/default/pods/mypod/log?container="+container, nil)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)

		if rw.Code != http.StatusOK {
			t.Errorf("container=%s: expected 200, got %d", container, rw.Code)
			continue
		}
		if got := rw.Body.String(); got != want {
			t.Errorf("container=%s: expected %q, got %q", container, want, got)
		}
	}
}

// TestHandler_ServeLog_NoContainerPicksAvailable verifies that when the client
// asks for `/log` without a ?container= param and the archive only has
// per-container records (the normal case for new captures), the server falls
// back to the first one it finds rather than returning the "not captured" stub.
func TestHandler_ServeLog_NoContainerPicksAvailable(t *testing.T) {
	webLog, _ := json.Marshal("web container log\n")
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods/mypod/log?container=web": webLog,
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/mypod/log", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rw.Code)
	}
	if got := rw.Body.String(); got != "web container log\n" {
		t.Errorf("expected per-container fallback to serve web log, got %q", got)
	}
}

// TestHandler_ServeLog_Previous verifies that `kubectl logs <pod> -c <c> --previous`
// (encoded as ?container=<c>&previous=true) is routed to the previous-container
// record stored under the corresponding index key.
func TestHandler_ServeLog_Previous(t *testing.T) {
	currentLog, _ := json.Marshal("current\n")
	previousLog, _ := json.Marshal("previous\n")
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods/mypod/log?container=app":               currentLog,
		"/api/v1/namespaces/default/pods/mypod/log?container=app&previous=true": previousLog,
	})
	h := newHandler(store, time.Time{}, false)

	// Without previous=true, returns current.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/namespaces/default/pods/mypod/log?container=app", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if got := rw.Body.String(); got != "current\n" {
		t.Errorf("expected current log, got %q", got)
	}

	// With previous=true, returns previous.
	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/namespaces/default/pods/mypod/log?container=app&previous=true", nil)
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if got := rw.Body.String(); got != "previous\n" {
		t.Errorf("expected previous log, got %q", got)
	}
}

// TestHandler_ServeLog_NoContainerDoesNotPickPrevious verifies that a bare
// /log request never accidentally serves a previous-log record. Previous logs
// must be opted into explicitly via ?previous=true.
func TestHandler_ServeLog_NoContainerDoesNotPickPrevious(t *testing.T) {
	previousLog, _ := json.Marshal("previous only\n")
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods/mypod/log?container=app&previous=true": previousLog,
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/mypod/log", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	body := rw.Body.String()
	if strings.Contains(body, "previous only") {
		t.Errorf("bare /log served a previous-log record by accident: %q", body)
	}
	if !strings.Contains(body, "not captured") {
		t.Errorf("expected stub since only previous record exists, got %q", body)
	}
}

// TestHandler_GroupResourceList_FallbackFromIndex verifies that the mock server
// returns a valid APIResourceList for a CRD API group even when the
// /apis/<group>/<version> discovery document was never captured — as long as
// resource records for that group exist in the archive index.
//
// This covers the case where an older capture missed the discovery call but
// has real VirtualService/Gateway records.
func TestHandler_GroupResourceList_FallbackFromIndex(t *testing.T) {
	// Store only resource-level records — no discovery doc for the group.
	store := buildTestStore(t, map[string][]byte{
		"/apis/networking.istio.io/v1beta1/namespaces/default/virtualservices": []byte(
			`{"kind":"VirtualServiceList","apiVersion":"networking.istio.io/v1beta1","items":[]}`),
		"/apis/networking.istio.io/v1beta1/namespaces/default/gateways": []byte(
			`{"kind":"GatewayList","apiVersion":"networking.istio.io/v1beta1","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/apis/networking.istio.io/v1beta1", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", rw.Code, rw.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &result); err != nil {
		t.Fatalf("expected JSON body: %v", err)
	}
	if result["kind"] != "APIResourceList" {
		t.Errorf("expected kind=APIResourceList, got %v", result["kind"])
	}
	if result["groupVersion"] != "networking.istio.io/v1beta1" {
		t.Errorf("expected groupVersion=networking.istio.io/v1beta1, got %v", result["groupVersion"])
	}
	resources, _ := result["resources"].([]any)
	if len(resources) == 0 {
		t.Fatalf("expected non-empty resources list in synthesised APIResourceList")
	}
	// Both virtualservices and gateways should appear.
	names := make(map[string]bool)
	for _, r := range resources {
		if rm, ok := r.(map[string]any); ok {
			if name, ok := rm["name"].(string); ok {
				names[name] = true
			}
		}
	}
	for _, want := range []string{"virtualservices", "gateways"} {
		if !names[want] {
			t.Errorf("expected %q in synthesised resource list, got: %v", want, names)
		}
	}
}

// TestHandler_ProxySubResource405 verifies that requests to pod proxy
// sub-resources (/pods/<name>/proxy/...) return 405 with a clear message,
// rather than silently returning 404.
func TestHandler_ProxySubResource405(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/istio-system/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	proxyPaths := []string{
		"/api/v1/namespaces/istio-system/services/istiod:15014/proxy/debug/syncz",
		"/api/v1/namespaces/istio-system/pods/istiod-abc/proxy/stats/prometheus",
	}
	for _, path := range proxyPaths {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(method+"_"+path, func(t *testing.T) {
				req := httptest.NewRequest(method, path, nil)
				rw := httptest.NewRecorder()
				h.ServeHTTP(rw, req)

				if rw.Code != http.StatusMethodNotAllowed {
					t.Fatalf("%s %s: expected 405, got %d\nbody: %s", method, path, rw.Code, rw.Body.String())
				}
				body := rw.Body.String()
				if !strings.Contains(body, "k8shark") {
					t.Errorf("expected k8shark mention in error message, got: %s", body)
				}
			})
		}
	}
}

// TestHandler_NamespaceQueryFallsBackToClusterPath verifies that a request for
// namespace-scoped pods (e.g. kubectl get pods -n default) returns the correct
// items even when the capture only stored the cluster-scoped /api/v1/pods path
// (no per-namespace paths in the index). This matches the behavior when a
// config entry for pods omits namespaces: or when the allNotFound fallback fires
// during auto-discovery.
func TestHandler_NamespaceQueryFallsBackToClusterPath(t *testing.T) {
	// Cluster-scoped pod list with pods in two different namespaces.
	podList := listWithPods([]podSpec{
		{name: "nginx", namespace: "default"},
		{name: "redis", namespace: "kube-system"},
	})
	// Only the cluster-scoped path is stored — no per-namespace paths.
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/pods": podList,
	})
	h := newHandler(store, time.Time{}, false)

	// Querying by namespace should still return only pods from that namespace.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", rw.Code, rw.Body.String())
	}

	names := itemNames(t, rw.Body.Bytes())
	if len(names) != 1 || names[0] != "nginx" {
		t.Errorf("expected [nginx], got %v", names)
	}
}

// TestHandler_SingleItemGetFallsBackToClusterPath verifies that a single-item
// GET (e.g. kubectl get pod nginx -n default) works when the capture only
// stored the cluster-scoped /api/v1/pods path.
func TestHandler_SingleItemGetFallsBackToClusterPath(t *testing.T) {
	podList := listWithPods([]podSpec{
		{name: "nginx", namespace: "default"},
		{name: "redis", namespace: "kube-system"},
	})
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/pods": podList,
	})
	h := newHandler(store, time.Time{}, false)

	// Single-item GET for the nginx pod in default — must resolve from cluster path.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/nginx", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", rw.Code, rw.Body.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &obj); err != nil {
		t.Fatalf("expected JSON body: %v", err)
	}
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil || meta["name"] != "nginx" {
		t.Errorf("expected metadata.name=nginx, got: %v", obj)
	}

	// redis is in kube-system — looking it up in default must return 404.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/pods/redis", nil)
	rw2 := httptest.NewRecorder()
	h.ServeHTTP(rw2, req2)
	if rw2.Code == http.StatusOK {
		t.Errorf("expected non-200 for redis in default, got 200: %s", rw2.Body.String())
	}
}

// TestHandler_WatchFallsBackToClusterPath verifies that a namespace-scoped
// watch (e.g. from k9s pod/container watchers) returns the correct items when
// the capture only stored the cluster-scoped list path.
func TestHandler_WatchFallsBackToClusterPath(t *testing.T) {
	podList := listWithPods([]podSpec{
		{name: "nginx", namespace: "default"},
		{name: "redis", namespace: "kube-system"},
	})
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/pods": podList,
	})
	h := newHandler(store, time.Time{}, false)

	// Watch for pods in default — must receive an ADDED event for nginx only.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/namespaces/default/pods?watch=1&timeoutSeconds=1", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	body := rw.Body.String()
	if !strings.Contains(body, `"nginx"`) {
		t.Errorf("expected nginx in watch stream, got: %s", body)
	}
	if strings.Contains(body, `"redis"`) {
		t.Errorf("redis (kube-system) must not appear in default namespace watch")
	}
}
