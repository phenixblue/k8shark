package capture

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/config"
)

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

	// Must have captured at least one record per resource.
	if len(eng.records) == 0 {
		t.Fatal("no records captured")
	}
	// Verify at least one pod record exists.
	foundPod := false
	for _, rec := range eng.records {
		if rec.APIPath == "/api/v1/namespaces/default/pods" && rec.ResponseCode == 200 {
			var podList map[string]any
			if err := json.Unmarshal(rec.ResponseBody, &podList); err != nil {
				t.Errorf("pod response not valid JSON: %v", err)
			}
			if podList["kind"] != "PodList" {
				t.Errorf("expected kind=PodList, got %v", podList["kind"])
			}
			foundPod = true
		}
	}
	if !foundPod {
		t.Error("no pod records found in capture")
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
	for _, rec := range eng.records {
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
