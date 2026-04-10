package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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

func TestHandler_NotFound(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/namespaces/default/services", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rw.Code)
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
			// resourceVersion must be non-empty and not the placeholder "0".
			if obj, ok := ev["object"].(map[string]any); ok {
				if meta, ok := obj["metadata"].(map[string]any); ok {
					rv, _ := meta["resourceVersion"].(string)
					if rv == "" || rv == "0" {
						t.Errorf("BOOKMARK resourceVersion should be non-zero, got %q", rv)
					}
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
