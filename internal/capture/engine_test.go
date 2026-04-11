package capture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/config"
)

// sliceSink is a test RecordSink that accumulates records in memory.
type sliceSink struct {
	mu      sync.Mutex
	records []*Record
}

func (s *sliceSink) WriteRecord(rec any) error {
	r, ok := rec.(*Record)
	if !ok {
		return nil
	}
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
	return nil
}
func (s *sliceSink) Finish(_, _ any) error { return nil }
func (s *sliceSink) RecordCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// fakeK8sServer returns an httptest.TLSServer that responds to the paths used by
// a minimal capture config (pods in default, nodes cluster-scoped).
func fakeK8sServer(t *testing.T) *httptest.Server {
	t.Helper()
	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"apiVersion":"v1","kind":"Pod","metadata":{"name":"nginx","namespace":"default"}}]}`
	nodeList := `{"apiVersion":"v1","kind":"NodeList","metadata":{},"items":[{"apiVersion":"v1","kind":"Node","metadata":{"name":"node1"}}]}`

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api/v1/namespaces/default/pods":
			fmt.Fprint(w, podList)
		case "/api/v1/nodes":
			fmt.Fprint(w, nodeList)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":404}`)
		}
	}))
	return srv
}

func TestEngine_CaptureToArchive(t *testing.T) {
	fake := fakeK8sServer(t)
	defer fake.Close()

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "capture.tar.gz")

	cfg := &config.Config{
		DurationRaw: "2s",
		Duration:    2 * time.Second,
		Output:      outFile,
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "500ms", Interval: 500 * time.Millisecond},
			{Version: "v1", Resource: "nodes", IntervalRaw: "500ms", Interval: 500 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, fake.Client(), fake.URL, false)
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	// Archive file must exist and be non-empty.
	fi, err := os.Stat(outFile)
	if err != nil {
		t.Fatalf("output archive not found: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("output archive is empty")
	}

	// Extract the archive and verify its contents.
	extractDir := t.TempDir()
	if err := archive.Open(outFile, extractDir); err != nil {
		t.Fatalf("failed to open archive: %v", err)
	}

	// metadata.json must exist.
	if _, err := os.Stat(filepath.Join(extractDir, "k8shark-capture", "metadata.json")); err != nil {
		t.Error("metadata.json missing from archive")
	}
	// index.json must exist.
	if _, err := os.Stat(filepath.Join(extractDir, "k8shark-capture", "index.json")); err != nil {
		t.Error("index.json missing from archive")
	}

	// Verify index contains the captured paths.
	if _, ok := eng.index["/api/v1/namespaces/default/pods"]; !ok {
		t.Error("pod path missing from index")
	}
	if _, ok := eng.index["/api/v1/nodes"]; !ok {
		t.Error("nodes path missing from index")
	}
}

func TestEngine_FetchPodsLogs(t *testing.T) {
	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[` +
		`{"metadata":{"name":"nginx","namespace":"default"}},` +
		`{"metadata":{"name":"redis","namespace":"default"}}]}`
	nginxLog := "nginx log line 1\nnginx log line 2\n"
	redisLog := "redis log line 1\n"

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api/v1/namespaces/default/pods":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, podList)
		case "/api/v1/namespaces/default/pods/nginx/log":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, nginxLog)
		case "/api/v1/namespaces/default/pods/redis/log":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, redisLog)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "1s",
		Duration:    1 * time.Second,
		Output:      filepath.Join(outDir, "capture.tar.gz"),
		Resources: []config.Resource{
			{
				Version: "v1", Resource: "pods",
				Namespaces:  []string{"default"},
				IntervalRaw: "500ms", Interval: 500 * time.Millisecond,
				Logs: 50,
			},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	// Both log paths must be in the index.
	nginxLogPath := "/api/v1/namespaces/default/pods/nginx/log"
	redisLogPath := "/api/v1/namespaces/default/pods/redis/log"
	if _, ok := eng.index[nginxLogPath]; !ok {
		t.Errorf("nginx log path %q missing from index", nginxLogPath)
	}
	if _, ok := eng.index[redisLogPath]; !ok {
		t.Errorf("redis log path %q missing from index", redisLogPath)
	}

	// Log records must be stored as JSON strings encoding the plain-text body.
	for _, rec := range ss.records {
		if rec.APIPath != nginxLogPath && rec.APIPath != redisLogPath {
			continue
		}
		var text string
		if err := json.Unmarshal(rec.ResponseBody, &text); err != nil {
			t.Errorf("log record at %q has invalid JSON body: %v", rec.APIPath, err)
			continue
		}
		want := nginxLog
		if rec.APIPath == redisLogPath {
			want = redisLog
		}
		if text != want {
			t.Errorf("%q: got %q, want %q", rec.APIPath, text, want)
		}
	}
}

func TestEngine_NoLogsWhenDisabled(t *testing.T) {
	logCalled := false
	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			logCalled = true
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "some log")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api/v1/namespaces/default/pods":
			fmt.Fprint(w, podList)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "1s",
		Duration:    1 * time.Second,
		Output:      filepath.Join(outDir, "capture.tar.gz"),
		Resources: []config.Resource{
			// Logs: 0 (default) — log capture disabled.
			{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "500ms", Interval: 500 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}
	if logCalled {
		t.Error("log endpoint was called even though Logs=0")
	}
}

func TestEngine_NDJSONOutput(t *testing.T) {
	srv := fakeK8sServer(t)
	defer srv.Close()

	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      "-",
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	var buf bytes.Buffer
	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	eng.sink = archive.NewNDJSONWriter(&buf)
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	output := strings.TrimSpace(buf.String())
	if output == "" {
		t.Fatal("expected NDJSON output, got empty buffer")
	}
	for i, line := range strings.Split(output, "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Errorf("line %d is not valid JSON: %v\nline: %s", i, err, line)
		}
	}
}

// ── Wildcard namespace expansion tests ──────────────────────────────────────

func nsList(namespaces ...string) string {
	items := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		items = append(items, fmt.Sprintf(`{"metadata":{"name":%q}}`, ns))
	}
	return `{"kind":"NamespaceList","items":[` + strings.Join(items, ",") + `]}`
}

func wildcardServer(t *testing.T, discoveredNS []string, reqPaths chan<- string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqPaths != nil {
			select {
			case reqPaths <- r.URL.Path:
			default:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api/v1/namespaces":
			fmt.Fprint(w, nsList(discoveredNS...))
		default:
			// Return a minimal list for any resource path so engine doesn't warn.
			fmt.Fprintf(w, `{"kind":"List","items":[]}`)
		}
	}))
	return srv
}

func TestExpandWildcard_NoWildcard(t *testing.T) {
	paths := make(chan string, 100)
	srv := wildcardServer(t, []string{"default", "kube-system"}, paths)
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(outDir, "capture.tar.gz"),
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	// /api/v1/namespaces (discovery) must NOT have been called.
	close(paths)
	for p := range paths {
		if p == "/api/v1/namespaces" {
			t.Error("namespace discovery endpoint was called even though no '*' was configured")
		}
	}

	// Namespaces must be unchanged.
	if got := cfg.Resources[0].Namespaces; len(got) != 1 || got[0] != "default" {
		t.Errorf("expected namespaces unchanged, got %v", got)
	}
}

func TestExpandWildcard_AllNamespaces(t *testing.T) {
	discovered := []string{"default", "kube-system", "production"}
	srv := wildcardServer(t, discovered, nil)
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(outDir, "capture.tar.gz"),
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"*"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	got := cfg.Resources[0].Namespaces
	if len(got) != len(discovered) {
		t.Fatalf("expected %d namespaces after expansion, got %d: %v", len(discovered), len(got), got)
	}
	for i, want := range discovered {
		if got[i] != want {
			t.Errorf("namespace[%d]: want %q, got %q", i, want, got[i])
		}
	}
}

func TestExpandWildcard_Mixed(t *testing.T) {
	discovered := []string{"default", "kube-system", "production"}
	srv := wildcardServer(t, discovered, nil)
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(outDir, "capture.tar.gz"),
		Resources: []config.Resource{
			// "production" explicit first, then wildcard — production must not be duplicated.
			{Version: "v1", Resource: "pods", Namespaces: []string{"production", "*"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	got := cfg.Resources[0].Namespaces
	// Expect: production (explicit), default, kube-system — no duplicate production.
	wantLen := len(discovered) // 3 unique namespaces
	if len(got) != wantLen {
		t.Fatalf("expected %d namespaces (deduped), got %d: %v", wantLen, len(got), got)
	}
	if got[0] != "production" {
		t.Errorf("expected 'production' first (explicit), got %q", got[0])
	}
	seen := make(map[string]bool)
	for _, ns := range got {
		if seen[ns] {
			t.Errorf("duplicate namespace %q in expanded list %v", ns, got)
		}
		seen[ns] = true
	}
}

func TestExpandWildcard_ClusterScoped(t *testing.T) {
	srv := wildcardServer(t, []string{"default"}, nil)
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(outDir, "capture.tar.gz"),
		Resources: []config.Resource{
			{Version: "v1", Resource: "nodes", Namespaces: []string{"*"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	// Namespaces must be cleared (cluster-scoped fetch).
	if got := cfg.Resources[0].Namespaces; len(got) != 0 {
		t.Errorf("expected nil/empty Namespaces for cluster-scoped resource, got %v", got)
	}
}

func TestExpandWildcard_DiscoveryFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces" {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"kind":"Status","code":403}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
	}))
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(outDir, "capture.tar.gz"),
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"*"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	_, err := eng.Run()
	if err == nil {
		t.Fatal("expected error from discovery failure, got nil")
	}
	if !strings.Contains(err.Error(), "namespace discovery failed") {
		t.Errorf("expected 'namespace discovery failed' in error, got: %v", err)
	}
}

// ── Auto-discover CRD resource tests ────────────────────────────────────────

// autoDiscoverServer returns a fake API server that serves minimal /apis and
// /apis/<group>/<version> responses for one CRD group (networking.istio.io)
// with two resources: virtualservices (namespaced) and gateways (namespaced).
// It also serves a metrics.k8s.io group which should be excluded by default.
func autoDiscoverServer(t *testing.T, reqPaths chan<- string) *httptest.Server {
	t.Helper()
	apisBody := `{
		"kind":"APIGroupList","apiVersion":"v1",
		"groups":[
			{"name":"networking.istio.io","versions":[{"groupVersion":"networking.istio.io/v1beta1","version":"v1beta1"}]},
			{"name":"metrics.k8s.io","versions":[{"groupVersion":"metrics.k8s.io/v1beta1","version":"v1beta1"}]}
		]
	}`
	istioGVBody := `{
		"kind":"APIResourceList","apiVersion":"v1",
		"groupVersion":"networking.istio.io/v1beta1",
		"resources":[
			{"name":"virtualservices","namespaced":true,"kind":"VirtualService"},
			{"name":"gateways","namespaced":true,"kind":"Gateway"},
			{"name":"virtualservices/status","namespaced":true,"kind":"VirtualService"}
		]
	}`
	metricsGVBody := `{
		"kind":"APIResourceList","apiVersion":"v1",
		"groupVersion":"metrics.k8s.io/v1beta1",
		"resources":[{"name":"pods","namespaced":true,"kind":"PodMetrics"}]
	}`

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqPaths != nil {
			select {
			case reqPaths <- r.URL.Path:
			default:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/apis":
			fmt.Fprint(w, apisBody)
		case "/apis/networking.istio.io/v1beta1":
			fmt.Fprint(w, istioGVBody)
		case "/apis/metrics.k8s.io/v1beta1":
			fmt.Fprint(w, metricsGVBody)
		default:
			fmt.Fprint(w, `{"kind":"List","apiVersion":"v1","items":[]}`)
		}
	}))
	return srv
}

func TestAutoDiscover_AddsResources(t *testing.T) {
	srv := autoDiscoverServer(t, nil)
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw:  "500ms",
		Duration:     500 * time.Millisecond,
		Output:       filepath.Join(outDir, "capture.tar.gz"),
		AutoDiscover: true,
		// No explicit resources — auto-discover should populate them.
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	// virtualservices and gateways should have been added; metrics.k8s.io excluded.
	resourceNames := make(map[string]bool)
	for _, r := range cfg.Resources {
		resourceNames[r.Resource] = true
		if r.Group == "metrics.k8s.io" {
			t.Errorf("metrics.k8s.io/%s should be excluded by default", r.Resource)
		}
	}
	for _, want := range []string{"virtualservices", "gateways"} {
		if !resourceNames[want] {
			t.Errorf("expected resource %q to be auto-discovered, got resources: %v", want, cfg.Resources)
		}
	}
	// Sub-resources (virtualservices/status) must NOT be added.
	if resourceNames["virtualservices/status"] {
		t.Error("sub-resource 'virtualservices/status' should not be added as a resource entry")
	}
}

func TestAutoDiscover_SkipsAlreadyConfigured(t *testing.T) {
	srv := autoDiscoverServer(t, nil)
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw:  "500ms",
		Duration:     500 * time.Millisecond,
		Output:       filepath.Join(outDir, "capture.tar.gz"),
		AutoDiscover: true,
		Resources: []config.Resource{
			// Pre-configured virtualservices — must not be duplicated.
			{Group: "networking.istio.io", Version: "v1beta1", Resource: "virtualservices",
				Namespaces: []string{"default"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	// Count virtualservices entries — must be exactly 1.
	count := 0
	for _, r := range cfg.Resources {
		if r.Resource == "virtualservices" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 virtualservices entry, got %d", count)
	}
}

func TestAutoDiscover_ExcludeGroupsOverride(t *testing.T) {
	srv := autoDiscoverServer(t, nil)
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw:               "500ms",
		Duration:                  500 * time.Millisecond,
		Output:                    filepath.Join(outDir, "capture.tar.gz"),
		AutoDiscover:              true,
		AutoDiscoverExcludeGroups: []string{"networking.istio.io"},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	for _, r := range cfg.Resources {
		if r.Group == "networking.istio.io" {
			t.Errorf("networking.istio.io should be excluded via AutoDiscoverExcludeGroups, but found %v", r)
		}
	}
}
