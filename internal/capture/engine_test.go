package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/config"
)

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
func (s *sliceSink) Finish(_, _, _ any) error { return nil }
func (s *sliceSink) RecordCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}
func (s *sliceSink) UncompressedBytes() int64 { return 0 }

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
	outFile := filepath.Join(outDir, "capture.khsrk")

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

	// Open the archive and verify its contents.
	ar, err := archive.Open(outFile)
	if err != nil {
		t.Fatalf("failed to open archive: %v", err)
	}
	defer ar.Close()

	// metadata must be readable.
	var capMeta CaptureMetadata
	if err := ar.ReadMetadata(&capMeta); err != nil {
		t.Error("metadata.json missing from archive")
	}
	// index must be readable.
	var capIdx Index
	if err := ar.ReadIndex(&capIdx); err != nil {
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
	// Pod fixtures include spec.containers so the engine knows what to fetch
	// logs for. The redis pod also has an init container to verify init-
	// container logs are captured too.
	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[` +
		`{"metadata":{"name":"nginx","namespace":"default"},"spec":{"containers":[{"name":"web"}]}},` +
		`{"metadata":{"name":"redis","namespace":"default"},"spec":{"initContainers":[{"name":"init"}],"containers":[{"name":"redis"}]}}]}`
	logs := map[string]string{
		"web":   "nginx log line 1\nnginx log line 2\n",
		"redis": "redis log line 1\n",
		"init":  "init container ran\n",
	}

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
			return
		case "/api/v1/namespaces/default/pods":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, podList)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/log") {
			c := r.URL.Query().Get("container")
			body, ok := logs[c]
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `a container name must be specified for pod, choose one of: [...]`)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "1s",
		Duration:    1 * time.Second,
		Output:      filepath.Join(outDir, "capture.khsrk"),
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
	sum, err := eng.Run()
	if err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	// All three (pod, container) pairs must be present at their per-container keys.
	wantKeys := map[string]string{
		"/api/v1/namespaces/default/pods/nginx/log?container=web":   logs["web"],
		"/api/v1/namespaces/default/pods/redis/log?container=redis": logs["redis"],
		"/api/v1/namespaces/default/pods/redis/log?container=init":  logs["init"],
	}
	for key := range wantKeys {
		if _, ok := eng.index[key]; !ok {
			t.Errorf("log path %q missing from index", key)
		}
	}

	// Each record body decodes back to the matching log text.
	for _, rec := range ss.records {
		want, ok := wantKeys[rec.APIPath]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(rec.ResponseBody, &text); err != nil {
			t.Errorf("log record at %q has invalid JSON body: %v", rec.APIPath, err)
			continue
		}
		if text != want {
			t.Errorf("%q: got %q, want %q", rec.APIPath, text, want)
		}
	}

	if sum.PodLogs.Attempted != 3 || sum.PodLogs.Captured != 3 || sum.PodLogs.Skipped != 0 {
		t.Errorf("PodLogs summary = %+v, want Attempted=3 Captured=3 Skipped=0", sum.PodLogs)
	}
}

// TestEngine_FetchPodsLogs_SkippedFailuresInSummary verifies that when a
// container's log fetch fails (e.g. a 400 because no such container exists),
// the summary records the failure with an actionable reason instead of
// silently dropping it.
func TestEngine_FetchPodsLogs_SkippedFailuresInSummary(t *testing.T) {
	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[` +
		`{"metadata":{"name":"good","namespace":"default"},"spec":{"containers":[{"name":"app"}]}},` +
		`{"metadata":{"name":"bad","namespace":"default"},"spec":{"containers":[{"name":"crashed"}]}}]}`

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
			return
		case "/api/v1/namespaces/default/pods":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, podList)
			return
		case "/api/v1/namespaces/default/pods/good/log":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "good output\n")
			return
		case "/api/v1/namespaces/default/pods/bad/log":
			w.WriteHeader(http.StatusBadRequest)
			// Real K8s API server error format: a Status JSON envelope.
			// The engine should parse out the `message` field for the
			// summary instead of dumping the raw JSON.
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"container \"crashed\" in pod \"bad\" is waiting to start: PodInitializing","reason":"BadRequest","code":400}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(t.TempDir(), "capture.khsrk"),
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
	eng.sink = &sliceSink{}
	sum, err := eng.Run()
	if err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	if sum.PodLogs.Attempted != 2 || sum.PodLogs.Captured != 1 || sum.PodLogs.Skipped != 1 {
		t.Errorf("PodLogs summary = %+v, want Attempted=2 Captured=1 Skipped=1", sum.PodLogs)
	}
	if len(sum.PodLogs.Failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(sum.PodLogs.Failures))
	}
	f := sum.PodLogs.Failures[0]
	if f.Namespace != "default" || f.Pod != "bad" || f.Container != "crashed" {
		t.Errorf("failure identifies wrong pod/container: %+v", f)
	}
	if !strings.Contains(f.Reason, "HTTP 400") || !strings.Contains(f.Reason, "PodInitializing") {
		t.Errorf("failure reason should include HTTP code + parsed Status message, got %q", f.Reason)
	}
	// The raw JSON envelope should NOT appear — only the parsed message.
	if strings.Contains(f.Reason, `"kind"`) || strings.Contains(f.Reason, `apiVersion`) {
		t.Errorf("failure reason should show parsed Status message, not raw JSON: %q", f.Reason)
	}
}

// TestEngine_FetchPodsLogs_Previous verifies that when PreviousLogs: true is
// set, the engine fetches `?previous=true` for each container, stores the
// result under a separate per-container index key, and counts successful
// previous-log captures via CapturedPrevious. "No previous container" 400s
// are silently dropped — they should not appear in the failure sample.
func TestEngine_FetchPodsLogs_Previous(t *testing.T) {
	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[` +
		`{"metadata":{"name":"web","namespace":"default"},"spec":{"containers":[{"name":"app"}]}}` +
		`]}`

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
			return
		case "/api/v1/namespaces/default/pods":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, podList)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/log") {
			if r.URL.Query().Get("previous") == "true" {
				w.Header().Set("Content-Type", "text/plain")
				fmt.Fprint(w, "old crashed log\n")
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "current log\n")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(t.TempDir(), "capture.khsrk"),
		Resources: []config.Resource{
			{
				Version: "v1", Resource: "pods",
				Namespaces:  []string{"default"},
				IntervalRaw: "500ms", Interval: 500 * time.Millisecond,
				Logs:         50,
				PreviousLogs: true,
			},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss
	sum, err := eng.Run()
	if err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	currentKey := "/api/v1/namespaces/default/pods/web/log?container=app"
	previousKey := "/api/v1/namespaces/default/pods/web/log?container=app&previous=true"
	if _, ok := eng.index[currentKey]; !ok {
		t.Errorf("missing current log at %q", currentKey)
	}
	if _, ok := eng.index[previousKey]; !ok {
		t.Errorf("missing previous log at %q", previousKey)
	}

	if sum.PodLogs.Captured != 1 {
		t.Errorf("expected Captured=1, got %d", sum.PodLogs.Captured)
	}
	if sum.PodLogs.CapturedPrevious != 1 {
		t.Errorf("expected CapturedPrevious=1, got %d", sum.PodLogs.CapturedPrevious)
	}
	if sum.PodLogs.Attempted != 1 {
		t.Errorf("expected Attempted=1 (previous fetches not counted), got %d", sum.PodLogs.Attempted)
	}
}

// TestEngine_FetchPodsLogs_PreviousFailureSilent verifies that when the
// previous-log fetch returns a 400 (the common "container has not been
// previously terminated" case), it is silently dropped from the failure
// sample so the summary stays focused on current-log issues.
func TestEngine_FetchPodsLogs_PreviousFailureSilent(t *testing.T) {
	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[` +
		`{"metadata":{"name":"healthy","namespace":"default"},"spec":{"containers":[{"name":"app"}]}}` +
		`]}`

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
			return
		case "/api/v1/namespaces/default/pods":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, podList)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/log") {
			if r.URL.Query().Get("previous") == "true" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"previous terminated container \"app\" in pod \"healthy\" not found","reason":"BadRequest","code":400}`)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "log\n")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(t.TempDir(), "capture.khsrk"),
		Resources: []config.Resource{
			{
				Version: "v1", Resource: "pods",
				Namespaces:  []string{"default"},
				IntervalRaw: "500ms", Interval: 500 * time.Millisecond,
				Logs:         50,
				PreviousLogs: true,
			},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	eng.sink = &sliceSink{}
	sum, err := eng.Run()
	if err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	if sum.PodLogs.Skipped != 0 {
		t.Errorf("previous-log failures must not count as Skipped, got %d", sum.PodLogs.Skipped)
	}
	if len(sum.PodLogs.Failures) != 0 {
		t.Errorf("previous-log failures must not appear in the failure sample, got %+v", sum.PodLogs.Failures)
	}
	if sum.PodLogs.Captured != 1 {
		t.Errorf("expected Captured=1, got %d", sum.PodLogs.Captured)
	}
	if sum.PodLogs.CapturedPrevious != 0 {
		t.Errorf("expected CapturedPrevious=0 (previous fetch failed), got %d", sum.PodLogs.CapturedPrevious)
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
		Output:      filepath.Join(outDir, "capture.khsrk"),
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

// wildcardServerPaginated serves /api/v1/namespaces in two pages of pageSize,
// simulating Kubernetes pagination for clusters with many namespaces.
func wildcardServerPaginated(t *testing.T, allNS []string, pageSize int) *httptest.Server {
	t.Helper()
	const tok = "page2token"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api/v1/namespaces":
			cont := r.URL.Query().Get("continue")
			if cont == "" {
				// First page
				page := allNS
				var continueVal string
				if len(allNS) > pageSize {
					page = allNS[:pageSize]
					continueVal = tok
				}
				b, _ := buildNsList(page, continueVal)
				fmt.Fprint(w, b)
			} else if cont == tok {
				// Second page
				page := allNS[pageSize:]
				b, _ := buildNsList(page, "")
				fmt.Fprint(w, b)
			} else {
				w.WriteHeader(http.StatusBadRequest)
			}
		default:
			fmt.Fprintf(w, `{"kind":"List","items":[]}`)
		}
	}))
	return srv
}

func buildNsList(names []string, continueToken string) (string, error) {
	type item struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	type list struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Continue string `json:"continue,omitempty"`
		} `json:"metadata"`
		Items []item `json:"items"`
	}
	l := list{Kind: "NamespaceList"}
	l.Metadata.Continue = continueToken
	for _, n := range names {
		var it item
		it.Metadata.Name = n
		l.Items = append(l.Items, it)
	}
	b, err := json.Marshal(l)
	return string(b), err
}

func TestExpandWildcard_Pagination(t *testing.T) {
	// Simulate a cluster with 3 namespaces split across two pages (pageSize=2).
	allNS := []string{"default", "kube-system", "portworx"}
	srv := wildcardServerPaginated(t, allNS, 2)
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(outDir, "capture.khsrk"),
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"*"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	got := cfg.Resources[0].Namespaces
	if len(got) != len(allNS) {
		t.Fatalf("expected %d namespaces (all pages), got %d: %v", len(allNS), len(got), got)
	}
	gotSet := make(map[string]bool, len(got))
	for _, ns := range got {
		gotSet[ns] = true
	}
	for _, want := range allNS {
		if !gotSet[want] {
			t.Errorf("namespace %q missing from expanded list %v", want, got)
		}
	}
}

func TestExpandWildcard_NoWildcard(t *testing.T) {
	paths := make(chan string, 100)
	srv := wildcardServer(t, []string{"default", "kube-system"}, paths)
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(outDir, "capture.khsrk"),
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
		Output:      filepath.Join(outDir, "capture.khsrk"),
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
		Output:      filepath.Join(outDir, "capture.khsrk"),
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
		Output:      filepath.Join(outDir, "capture.khsrk"),
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
		Output:      filepath.Join(outDir, "capture.khsrk"),
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

func TestExpandWildcard_DiscoveryCancelledByDuration(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces":
			// Ensure the run context times out before this response is returned.
			time.Sleep(100 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"kind":"NamespaceList","items":[]}`)
		case "/version":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		default:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "50ms",
		Duration:    50 * time.Millisecond,
		Output:      filepath.Join(outDir, "capture.khsrk"),
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"*"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	_, err := eng.Run()
	if err == nil {
		t.Fatal("expected timeout/cancellation error, got nil")
	}
	if !strings.Contains(err.Error(), "request canceled before completion") {
		t.Errorf("expected cancellation hint in error, got: %v", err)
	}
}

func TestRun_FailsWhenWatchConcurrencyTooHigh(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api":
			fmt.Fprint(w, `{"versions":["v1"]}`)
		case "/api/v1":
			fmt.Fprint(w, `{"kind":"APIResourceList","resources":[]}`)
		case "/apis":
			fmt.Fprint(w, `{"kind":"APIGroupList","groups":[]}`)
		case "/openapi/v2":
			fmt.Fprint(w, `{}`)
		case "/openapi/v3":
			fmt.Fprint(w, `{"paths":{}}`)
		default:
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	ns := make([]string, 0, maxConcurrentWatchStreams+1)
	for i := 0; i < maxConcurrentWatchStreams+1; i++ {
		ns = append(ns, fmt.Sprintf("ns-%d", i))
	}

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "30s",
		Duration:    30 * time.Second,
		Output:      filepath.Join(outDir, "capture.khsrk"),
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: ns, Watch: true, IntervalRaw: "30s", Interval: 30 * time.Second},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	_, err := eng.Run()
	if err == nil {
		t.Fatal("expected watch concurrency guard error, got nil")
	}
	if !strings.Contains(err.Error(), "concurrent watch streams") {
		t.Fatalf("expected watch concurrency guard error, got: %v", err)
	}
}

func TestRun_FailsPreflightOnUnauthorized(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"kind":"Status","message":"unauthorized"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"kind":"List","items":[]}`)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "capture.khsrk")
	cfg := &config.Config{
		DurationRaw: "2s",
		Duration:    2 * time.Second,
		Output:      out,
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "500ms", Interval: 500 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	_, err := eng.Run()
	if err == nil {
		t.Fatal("expected preflight error, got nil")
	}
	if !strings.Contains(err.Error(), "capture preflight failed") {
		t.Fatalf("expected preflight prefix in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "GET /version returned 401") {
		t.Fatalf("expected 401 detail in error, got: %v", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("expected no output archive on preflight failure, stat err: %v", statErr)
	}
}

func TestRun_FailsPreflightOnUnreachableServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
	}))
	serverURL := srv.URL
	client := srv.Client()
	srv.Close()

	out := filepath.Join(t.TempDir(), "capture.khsrk")
	cfg := &config.Config{
		DurationRaw: "2s",
		Duration:    2 * time.Second,
		Output:      out,
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "500ms", Interval: 500 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, client, serverURL, false)
	_, err := eng.Run()
	if err == nil {
		t.Fatal("expected preflight connectivity error, got nil")
	}
	if !strings.Contains(err.Error(), "capture preflight failed") {
		t.Fatalf("expected preflight prefix in error, got: %v", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("expected no output archive on preflight failure, stat err: %v", statErr)
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
			{"name":"meshconfigs","namespaced":false,"kind":"MeshConfig"},
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
		Output:       filepath.Join(outDir, "capture.khsrk"),
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
		Output:       filepath.Join(outDir, "capture.khsrk"),
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
		Output:                    filepath.Join(outDir, "capture.khsrk"),
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

func TestAutoDiscover_RetriesMissingGroupVersionDiscovery(t *testing.T) {
	t.Helper()

	var mu sync.Mutex
	gvCalls := 0

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api", "/api/v1":
			fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","resources":[]}`)
		case "/apis":
			fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"kubevirt.io","versions":[{"groupVersion":"kubevirt.io/v1","version":"v1"}]}]}`)
		case "/apis/kubevirt.io/v1":
			mu.Lock()
			gvCalls++
			call := gvCalls
			mu.Unlock()
			if call == 1 {
				// Simulate transient discovery failure during fetchDiscovery.
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"kind":"Status","status":"Failure","code":500}`)
				return
			}
			fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"kubevirt.io/v1","resources":[{"name":"virtualmachines","namespaced":true,"kind":"VirtualMachine"},{"name":"virtualmachineinstances","namespaced":true,"kind":"VirtualMachineInstance"}]}`)
		case "/apis/kubevirt.io/v1/virtualmachines":
			fmt.Fprint(w, `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachineList","items":[]}`)
		case "/apis/kubevirt.io/v1/virtualmachineinstances":
			fmt.Fprint(w, `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachineInstanceList","items":[]}`)
		default:
			fmt.Fprint(w, `{"kind":"List","apiVersion":"v1","items":[]}`)
		}
	}))
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw:  "500ms",
		Duration:     500 * time.Millisecond,
		Output:       filepath.Join(outDir, "capture.khsrk"),
		AutoDiscover: true,
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	eng.sink = &sliceSink{}
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	haveVM := false
	haveVMI := false
	for _, r := range cfg.Resources {
		if r.Group == "kubevirt.io" && r.Version == "v1" && r.Resource == "virtualmachines" {
			haveVM = true
		}
		if r.Group == "kubevirt.io" && r.Version == "v1" && r.Resource == "virtualmachineinstances" {
			haveVMI = true
		}
	}
	if !haveVM || !haveVMI {
		t.Fatalf("expected kubevirt virtualmachines and virtualmachineinstances to be auto-discovered, got: %+v", cfg.Resources)
	}
}

func TestAutoDiscover_AllDirectiveNamespacedScope(t *testing.T) {
	srv := autoDiscoverServer(t, nil)
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(outDir, "capture.khsrk"),
		Resources: []config.Resource{
			{All: true, Scope: "namespaced", Namespaces: []string{"team-a"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	seenMeshConfig := false
	seenVirtualService := false
	for _, r := range cfg.Resources {
		if r.All {
			continue
		}
		if r.Resource == "meshconfigs" {
			seenMeshConfig = true
		}
		if r.Resource == "virtualservices" {
			seenVirtualService = true
			if len(r.Namespaces) != 1 || r.Namespaces[0] != "team-a" {
				t.Fatalf("expected virtualservices namespaces from all directive, got %+v", r.Namespaces)
			}
		}
	}
	if seenMeshConfig {
		t.Fatal("cluster-scoped meshconfigs should not be discovered for scope=namespaced")
	}
	if !seenVirtualService {
		t.Fatal("expected namespaced resources to be discovered for scope=namespaced")
	}
}

func TestAutoDiscover_AllDirectiveClusterScope(t *testing.T) {
	srv := autoDiscoverServer(t, nil)
	defer srv.Close()

	outDir := t.TempDir()
	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(outDir, "capture.khsrk"),
		Resources: []config.Resource{
			{All: true, Scope: "cluster", IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	seenMeshConfig := false
	for _, r := range cfg.Resources {
		if r.All {
			continue
		}
		if r.Resource == "meshconfigs" {
			seenMeshConfig = true
			if len(r.Namespaces) != 0 {
				t.Fatalf("cluster-scoped discovered resource should not have namespaces, got %+v", r.Namespaces)
			}
		}
		if r.Resource == "virtualservices" || r.Resource == "gateways" {
			t.Fatalf("namespaced resource %q should not be discovered for scope=cluster", r.Resource)
		}
	}
	if !seenMeshConfig {
		t.Fatal("expected meshconfigs to be discovered for scope=cluster")
	}
}

func TestAutoDiscover_AllDirectiveExpandsWildcardNamespacesBeforePolling(t *testing.T) {
	paths := make(chan string, 256)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case paths <- r.URL.Path:
		default:
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api/v1/namespaces":
			fmt.Fprint(w, `{"kind":"NamespaceList","items":[{"metadata":{"name":"default"}},{"metadata":{"name":"team-a"}}]}`)
		case "/apis":
			fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"networking.istio.io","versions":[{"groupVersion":"networking.istio.io/v1beta1","version":"v1beta1"}]}]}`)
		case "/apis/networking.istio.io/v1beta1":
			fmt.Fprint(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"networking.istio.io/v1beta1","resources":[{"name":"virtualservices","namespaced":true,"kind":"VirtualService"}]}`)
		default:
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(t.TempDir(), "capture.khsrk"),
		Resources: []config.Resource{
			{All: true, Scope: "namespaced", IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	close(paths)
	sawWildcardPath := false
	sawClusterWidePath := false
	for p := range paths {
		if p == "/apis/networking.istio.io/v1beta1/namespaces/*/virtualservices" {
			sawWildcardPath = true
		}
		if p == "/apis/networking.istio.io/v1beta1/virtualservices" {
			sawClusterWidePath = true
		}
	}

	if sawWildcardPath {
		t.Fatal("found wildcard namespace API path for discovered namespaced resource; expansion must convert it before polling")
	}
	if !sawClusterWidePath {
		t.Fatal("expected cluster-wide poll for wildcard-namespaced discovered resource (demuxed into per-namespace records)")
	}
}

func TestAutoDiscover_AllDirectiveIncludesCorePods(t *testing.T) {
	paths := make(chan string, 256)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case paths <- r.URL.Path:
		default:
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api/v1/namespaces":
			fmt.Fprint(w, `{"kind":"NamespaceList","items":[{"metadata":{"name":"default"}}]}`)
		case "/api/v1":
			fmt.Fprint(w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"pods","namespaced":true},{"name":"nodes","namespaced":false},{"name":"pods/status","namespaced":true}]}`)
		case "/apis":
			fmt.Fprint(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`)
		default:
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      filepath.Join(t.TempDir(), "capture.khsrk"),
		Resources: []config.Resource{
			{All: true, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	hasPods := false
	for _, r := range cfg.Resources {
		if r.All {
			continue
		}
		if r.Group == "" && r.Version == "v1" && r.Resource == "pods" {
			hasPods = true
			if len(r.Namespaces) != 1 || r.Namespaces[0] != "default" {
				t.Fatalf("expected discovered core pods namespaces to expand to [default], got %v", r.Namespaces)
			}
		}
		if r.Resource == "pods/status" {
			t.Fatal("sub-resource pods/status should not be discovered")
		}
	}
	if !hasPods {
		t.Fatal("expected core/v1 pods to be discovered for all=true")
	}

	close(paths)
	sawClusterWidePodFetch := false
	for p := range paths {
		if p == "/api/v1/pods" {
			sawClusterWidePodFetch = true
			break
		}
	}
	if !sawClusterWidePodFetch {
		t.Fatal("expected cluster-wide poll for wildcard-namespaced discovered pods (demuxed into per-namespace records)")
	}
}

// TestFetchResource_WildcardUsesClusterWideAndDemuxes verifies that when a
// resource is marked WildcardNamespaces=true, the engine polls the cluster-
// wide endpoint exactly once (not one fetch per expanded namespace) and
// demuxes the response into per-namespace records.
func TestFetchResource_WildcardUsesClusterWideAndDemuxes(t *testing.T) {
	var clusterWideHits, perNamespaceHits int32
	clusterList := `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachineList","metadata":{"resourceVersion":"100"},"items":[` +
		`{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine","metadata":{"name":"vm-a","namespace":"team-a"}},` +
		`{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine","metadata":{"name":"vm-b","namespace":"team-b"}},` +
		`{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachine","metadata":{"name":"vm-c","namespace":"team-a"}}` +
		`]}`

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case r.URL.Path == "/apis/kubevirt.io/v1/virtualmachines":
			atomic.AddInt32(&clusterWideHits, 1)
			fmt.Fprint(w, clusterList)
		case strings.Contains(r.URL.Path, "/apis/kubevirt.io/v1/namespaces/") &&
			strings.HasSuffix(r.URL.Path, "/virtualmachines"):
			atomic.AddInt32(&perNamespaceHits, 1)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"kind":"VirtualMachineList","items":[]}`)
		default:
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		DurationRaw: "400ms",
		Duration:    400 * time.Millisecond,
		Output:      filepath.Join(t.TempDir(), "capture.khsrk"),
		Resources: []config.Resource{
			{
				Group:              "kubevirt.io",
				Version:            "v1",
				Resource:           "virtualmachines",
				Namespaces:         []string{"team-a", "team-b"},
				WildcardNamespaces: true,
				IntervalRaw:        "200ms", Interval: 200 * time.Millisecond,
			},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	eng.sink = &sliceSink{}
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	if got := atomic.LoadInt32(&perNamespaceHits); got != 0 {
		t.Errorf("expected zero per-namespace polls (cluster-wide path covers them), got %d", got)
	}
	if got := atomic.LoadInt32(&clusterWideHits); got == 0 {
		t.Fatal("expected at least one cluster-wide poll")
	}

	wantKeys := []string{
		"/apis/kubevirt.io/v1/namespaces/team-a/virtualmachines",
		"/apis/kubevirt.io/v1/namespaces/team-b/virtualmachines",
	}
	for _, k := range wantKeys {
		if _, ok := eng.index[k]; !ok {
			t.Errorf("expected demuxed record at %q", k)
		}
	}
	if _, ok := eng.index["/apis/kubevirt.io/v1/virtualmachines"]; ok {
		t.Error("did not expect the cluster-wide path to be stored — records must be demuxed per namespace")
	}
}

// TestFetchResource_WildcardCapturesZombieNamespaceItems is the regression
// test for the actual bug: a VirtualMachine whose namespace has been deleted
// (the namespace is not in /api/v1/namespaces) still appears in the cluster-
// wide VirtualMachine list. Per-namespace polling misses it; cluster-wide
// polling with demux captures it.
func TestFetchResource_WildcardCapturesZombieNamespaceItems(t *testing.T) {
	// /api/v1/namespaces only lists "live" — the wildcard expansion would
	// produce ["live"] only and per-namespace polling would never query the
	// deleted "ghost" namespace.
	clusterList := `{"apiVersion":"kubevirt.io/v1","kind":"VirtualMachineList","items":[` +
		`{"metadata":{"name":"live-vm","namespace":"live"}},` +
		`{"metadata":{"name":"zombie","namespace":"ghost"}}` +
		`]}`

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api/v1/namespaces":
			fmt.Fprint(w, `{"kind":"NamespaceList","items":[{"metadata":{"name":"live"}}]}`)
		case "/apis/kubevirt.io/v1/virtualmachines":
			fmt.Fprint(w, clusterList)
		default:
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		DurationRaw: "400ms",
		Duration:    400 * time.Millisecond,
		Output:      filepath.Join(t.TempDir(), "capture.khsrk"),
		Resources: []config.Resource{
			{
				Group:       "kubevirt.io",
				Version:     "v1",
				Resource:    "virtualmachines",
				Namespaces:  []string{"*"},
				IntervalRaw: "200ms", Interval: 200 * time.Millisecond,
			},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	eng.sink = &sliceSink{}
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	ghostKey := "/apis/kubevirt.io/v1/namespaces/ghost/virtualmachines"
	if _, ok := eng.index[ghostKey]; !ok {
		t.Fatalf("expected zombie-namespace VM to be captured at %q; got index keys: %v",
			ghostKey, indexKeys(eng.index))
	}
}

func indexKeys(idx Index) []string {
	keys := make([]string, 0, len(idx))
	for k := range idx {
		keys = append(keys, k)
	}
	return keys
}

// TestFetchResource_WildcardTableDemuxedPerNamespace verifies that the
// Table-format response (kubectl get -o wide) is demuxed by reading
// rows[].object.metadata.namespace.
func TestFetchResource_WildcardTableDemuxedPerNamespace(t *testing.T) {
	tableBody := `{"apiVersion":"meta.k8s.io/v1","kind":"Table","columnDefinitions":[{"name":"Name","type":"string"}],"rows":[` +
		`{"cells":["vm-a"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"vm-a","namespace":"team-a"}}},` +
		`{"cells":["vm-b"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"vm-b","namespace":"team-b"}}},` +
		`{"cells":["vm-c"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"vm-c","namespace":"team-a"}}}` +
		`]}`

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/apis/kubevirt.io/v1/virtualmachines":
			if strings.Contains(r.Header.Get("Accept"), "as=Table") {
				fmt.Fprint(w, tableBody)
				return
			}
			fmt.Fprint(w, `{"kind":"VirtualMachineList","items":[]}`)
		default:
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		DurationRaw: "400ms",
		Duration:    400 * time.Millisecond,
		Output:      filepath.Join(t.TempDir(), "capture.khsrk"),
		Resources: []config.Resource{
			{
				Group:              "kubevirt.io",
				Version:            "v1",
				Resource:           "virtualmachines",
				Namespaces:         []string{"team-a", "team-b"},
				WildcardNamespaces: true,
				IntervalRaw:        "200ms", Interval: 200 * time.Millisecond,
			},
		},
	}

	ss := &sliceSink{}
	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	eng.sink = ss
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	wantTableKeys := []string{
		"/apis/kubevirt.io/v1/namespaces/team-a/virtualmachines?as=Table",
		"/apis/kubevirt.io/v1/namespaces/team-b/virtualmachines?as=Table",
	}
	for _, k := range wantTableKeys {
		if _, ok := eng.index[k]; !ok {
			t.Errorf("expected demuxed Table record at %q", k)
		}
	}

	// Verify team-a's Table contains both vm-a and vm-c but NOT vm-b.
	var teamATable *Record
	for _, rec := range ss.records {
		if rec.APIPath == "/apis/kubevirt.io/v1/namespaces/team-a/virtualmachines?as=Table" {
			teamATable = rec
			break
		}
	}
	if teamATable == nil {
		t.Fatal("missing team-a Table record")
	}
	body := string(teamATable.ResponseBody)
	if !strings.Contains(body, "vm-a") || !strings.Contains(body, "vm-c") {
		t.Errorf("team-a Table missing expected rows: %s", body)
	}
	if strings.Contains(body, "vm-b") {
		t.Errorf("team-a Table leaked team-b row: %s", body)
	}
}

// TestStoreRecord_PopulatesItemCounts verifies that storeRecord counts the
// items[] (or rows[] for Table) in each stored body and appends to
// IndexEntry.Counts in lockstep with Seqs and Times. The UI namespace-card
// chips depend on these counts being correct.
func TestStoreRecord_PopulatesItemCounts(t *testing.T) {
	eng := newEngineWith(&config.Config{}, nil, "", false)
	ss := &sliceSink{}
	eng.sink = ss

	threeItems := []byte(`{"kind":"PodList","items":[{"metadata":{"name":"a"}},{"metadata":{"name":"b"}},{"metadata":{"name":"c"}}]}`)
	zeroItems := []byte(`{"kind":"PodList","items":[]}`)
	tableTwo := []byte(`{"kind":"Table","columnDefinitions":[],"rows":[{"object":{}},{"object":{}}]}`)
	singleObj := []byte(`{"kind":"Pod","metadata":{"name":"x"}}`)

	eng.storeRecord("/api/v1/namespaces/default/pods", threeItems, 200, false)
	eng.storeRecord("/api/v1/namespaces/default/pods", zeroItems, 200, false)
	eng.storeRecord("/api/v1/namespaces/default/pods?as=Table", tableTwo, 200, false)
	eng.storeRecord("/api/v1/namespaces/default/pods/foo", singleObj, 200, false)

	plain := eng.index["/api/v1/namespaces/default/pods"]
	if got := plain.Counts; len(got) != 2 || got[0] != 3 || got[1] != 0 {
		t.Errorf("plain Counts = %v, want [3 0]", got)
	}
	table := eng.index["/api/v1/namespaces/default/pods?as=Table"]
	if got := table.Counts; len(got) != 1 || got[0] != 2 {
		t.Errorf("table Counts = %v, want [2]", got)
	}
	single := eng.index["/api/v1/namespaces/default/pods/foo"]
	if got := single.Counts; len(got) != 1 || got[0] != 0 {
		t.Errorf("single-object Counts = %v, want [0]", got)
	}
}

func TestDoFetch_DedupAllSame_FirstAlwaysWritten(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"kind":"List","items":[]}`)
	}))
	defer srv.Close()

	eng := newEngineWith(&config.Config{}, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss

	for i := 0; i < 3; i++ {
		if _, code := eng.doFetch(context.Background(), "/api/v1/pods", "", true); code != http.StatusOK {
			t.Fatalf("doFetch status = %d, want %d", code, http.StatusOK)
		}
	}

	if got := ss.RecordCount(); got != 1 {
		t.Fatalf("record count = %d, want 1", got)
	}
	if got := eng.dedupSkipped; got != 2 {
		t.Fatalf("dedup skipped = %d, want 2", got)
	}
	entry := eng.index["/api/v1/pods"]
	if entry == nil || len(entry.Seqs) != 1 || len(entry.Times) != 1 {
		t.Fatalf("index entry should have exactly one written record, got %+v", entry)
	}
}

// TestRawFetch_TimesOutOnStalledBody guards the per-fetch timeout: a server
// that returns headers but never the body (a stalled HTTP/2 stream in the
// wild) must not hang the fetch indefinitely. The fetch must return promptly,
// and its fetchSem slot must be released so later fetches still succeed —
// otherwise one stuck read starves every other fetch and stalls the capture.
func TestRawFetch_TimesOutOnStalledBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/hang" {
			// Send headers, then block without ever writing a body.
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, `{"kind":"List","items":[]}`)
	}))
	defer srv.Close()

	eng := newEngineWith(&config.Config{}, srv.Client(), srv.URL, false)
	eng.sink = &sliceSink{}
	eng.fetchTimeout = 200 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		eng.doFetch(context.Background(), "/api/v1/hang", "", true)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("doFetch on a stalled endpoint did not return within 5s; per-fetch timeout not enforced")
	}

	// The stalled fetch must have released its read slot.
	if _, code := eng.doFetch(context.Background(), "/api/v1/ok", "", true); code != http.StatusOK {
		t.Fatalf("follow-up doFetch status = %d, want %d (fetchSem slot leaked?)", code, http.StatusOK)
	}
}

func TestDoFetch_DedupAllDifferent(t *testing.T) {
	count := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"kind":"List","metadata":{"rv":%q},"items":[]}`, fmt.Sprintf("%d", count))
	}))
	defer srv.Close()

	eng := newEngineWith(&config.Config{}, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss

	for i := 0; i < 3; i++ {
		if _, code := eng.doFetch(context.Background(), "/api/v1/pods", "", true); code != http.StatusOK {
			t.Fatalf("doFetch status = %d, want %d", code, http.StatusOK)
		}
	}

	if got := ss.RecordCount(); got != 3 {
		t.Fatalf("record count = %d, want 3", got)
	}
	if got := eng.dedupSkipped; got != 0 {
		t.Fatalf("dedup skipped = %d, want 0", got)
	}
}

func TestDoFetch_DedupOptOutWritesEveryPoll(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"kind":"List","items":[]}`)
	}))
	defer srv.Close()

	eng := newEngineWith(&config.Config{}, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss

	for i := 0; i < 3; i++ {
		if _, code := eng.doFetch(context.Background(), "/api/v1/events", "", false); code != http.StatusOK {
			t.Fatalf("doFetch status = %d, want %d", code, http.StatusOK)
		}
	}

	if got := ss.RecordCount(); got != 3 {
		t.Fatalf("record count = %d, want 3", got)
	}
	if got := eng.dedupSkipped; got != 0 {
		t.Fatalf("dedup skipped = %d, want 0", got)
	}
}

func TestEngine_MetadataIncludesDeduplicatedCount(t *testing.T) {
	podList := `{"apiVersion":"v1","kind":"PodList","items":[]}`
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api", "/api/v1", "/apis", "/openapi/v2", "/openapi/v3":
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		case "/api/v1/namespaces/default/pods":
			fmt.Fprint(w, podList)
		default:
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "capture.khsrk")
	cfg := &config.Config{
		DurationRaw: "1200ms",
		Duration:    1200 * time.Millisecond,
		Output:      outFile,
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	ar2, err := archive.Open(outFile)
	if err != nil {
		t.Fatalf("failed to open archive: %v", err)
	}
	defer ar2.Close()
	var meta map[string]any
	if err := ar2.ReadMetadata(&meta); err != nil {
		t.Fatalf("failed to read metadata: %v", err)
	}

	v, ok := meta["deduplicated_count"]
	if !ok {
		t.Fatal("metadata missing deduplicated_count")
	}
	count, ok := v.(float64)
	if !ok {
		t.Fatalf("deduplicated_count has unexpected type %T", v)
	}
	if count < 1 {
		t.Fatalf("deduplicated_count = %v, want >= 1", count)
	}
}

func TestEngine_DedupPerResourceOptOut(t *testing.T) {
	falseVal := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api", "/api/v1", "/apis", "/openapi/v2", "/openapi/v3":
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		case "/api/v1/namespaces/default/pods":
			fmt.Fprint(w, `{"kind":"PodList","items":[]}`)
		case "/api/v1/namespaces/default/events":
			fmt.Fprint(w, `{"kind":"EventList","items":[]}`)
		default:
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		DurationRaw: "1200ms",
		Duration:    1200 * time.Millisecond,
		Output:      filepath.Join(t.TempDir(), "capture.khsrk"),
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
			{Version: "v1", Resource: "events", Namespaces: []string{"default"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond, Dedup: &falseVal},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	podsEntry := eng.index["/api/v1/namespaces/default/pods"]
	eventsEntry := eng.index["/api/v1/namespaces/default/events"]
	if podsEntry == nil || eventsEntry == nil {
		t.Fatalf("missing index entries: pods=%v events=%v", podsEntry != nil, eventsEntry != nil)
	}
	if got := len(podsEntry.Seqs); got != 1 {
		t.Fatalf("pods should be deduplicated to one record, got %d", got)
	}
	if got := len(eventsEntry.Seqs); got <= 1 {
		t.Fatalf("events with dedup:false should keep multiple polls, got %d", got)
	}
}

func TestWatchResource_RecordsEvents(t *testing.T) {
	var watchHits int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/default/pods":
			if r.URL.Query().Get("watch") != "" {
				atomic.AddInt32(&watchHits, 1)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, `{"type":"ADDED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p1"}}}`+"\n")
				_, _ = io.WriteString(w, `{"type":"DELETED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p2"}}}`+"\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				return
			}
			fmt.Fprint(w, `{"kind":"PodList","metadata":{"resourceVersion":"10"},"items":[]}`)
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		default:
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	eng := newEngineWith(&config.Config{}, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res := config.Resource{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, Watch: true}

	done := make(chan struct{})
	go func() {
		defer close(done)
		eng.watchResource(ctx, res)
	}()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		ss.mu.Lock()
		count := len(ss.records)
		ss.mu.Unlock()
		if count >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if atomic.LoadInt32(&watchHits) == 0 {
		t.Fatal("expected at least one watch connection")
	}

	seen := map[string]bool{}
	for _, rec := range ss.records {
		if rec.APIPath != "/api/v1/namespaces/default/pods" {
			continue
		}
		if rec.EventType != "" {
			seen[rec.EventType] = true
		}
	}
	if !seen["ADDED"] || !seen["DELETED"] {
		t.Fatalf("expected ADDED and DELETED watch event types, got %v", seen)
	}
}

func TestWatchResource_Reconnects(t *testing.T) {
	var watchConn int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/default/pods":
			if r.URL.Query().Get("watch") != "" {
				n := atomic.AddInt32(&watchConn, 1)
				w.WriteHeader(http.StatusOK)
				if n == 1 {
					_, _ = io.WriteString(w, `{"type":"ADDED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"first"}}}`+"\n")
				} else {
					_, _ = io.WriteString(w, `{"type":"MODIFIED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"second"}}}`+"\n")
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				return
			}
			fmt.Fprint(w, `{"kind":"PodList","metadata":{"resourceVersion":"22"},"items":[]}`)
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		default:
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	eng := newEngineWith(&config.Config{}, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	res := config.Resource{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, Watch: true}

	done := make(chan struct{})
	go func() {
		defer close(done)
		eng.watchResource(ctx, res)
	}()

	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&watchConn) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if atomic.LoadInt32(&watchConn) < 2 {
		t.Fatalf("expected watch reconnect (>=2 watch connections), got %d", watchConn)
	}

	seen := map[string]bool{}
	for _, rec := range ss.records {
		if rec.EventType != "" {
			seen[rec.EventType] = true
		}
	}
	if !seen["ADDED"] || !seen["MODIFIED"] {
		t.Fatalf("expected ADDED and MODIFIED watch events across reconnects, got %v", seen)
	}
}

// TestWatchResource_WildcardOpensSingleClusterWideStream verifies that when a
// resource is marked WildcardNamespaces=true (originally configured with
// namespaces: ["*"]), watchResource opens a single cluster-wide watch stream
// instead of one stream per expanded namespace, and each emitted event is
// stored under its per-namespace API path (read from metadata.namespace) so
// the replay server's per-namespace reconstruction works unchanged.
func TestWatchResource_WildcardOpensSingleClusterWideStream(t *testing.T) {
	var clusterWideHits int32
	var perNamespaceHits int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
			return
		case "/api/v1/pods":
			if r.URL.Query().Get("watch") != "" {
				atomic.AddInt32(&clusterWideHits, 1)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w,
					`{"type":"ADDED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p1","namespace":"ns-a"}}}`+"\n"+
						`{"type":"ADDED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p2","namespace":"ns-b"}}}`+"\n"+
						`{"type":"MODIFIED","object":{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p1","namespace":"ns-a"}}}`+"\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				// Hold the connection open until the test cancels.
				<-r.Context().Done()
				return
			}
			fmt.Fprint(w, `{"kind":"PodList","metadata":{"resourceVersion":"10"},"items":[]}`)
			return
		}
		if strings.Contains(r.URL.Path, "/namespaces/") && strings.HasSuffix(r.URL.Path, "/pods") {
			atomic.AddInt32(&perNamespaceHits, 1)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	eng := newEngineWith(&config.Config{}, srv.Client(), srv.URL, false)
	ss := &sliceSink{}
	eng.sink = ss

	res := config.Resource{
		Version:            "v1",
		Resource:           "pods",
		Namespaces:         []string{"ns-a", "ns-b"},
		Watch:              true,
		WildcardNamespaces: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		eng.watchResource(ctx, res)
	}()

	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		ss.mu.Lock()
		n := len(ss.records)
		ss.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	if got := atomic.LoadInt32(&perNamespaceHits); got != 0 {
		t.Fatalf("expected zero per-namespace watch requests, got %d", got)
	}
	if got := atomic.LoadInt32(&clusterWideHits); got == 0 {
		t.Fatal("expected at least one cluster-wide watch connection")
	}

	eng.mu.Lock()
	defer eng.mu.Unlock()

	if _, ok := eng.watchIndex["/api/v1/pods"]; ok {
		t.Errorf("expected no events stored under cluster-wide path /api/v1/pods (events should be demuxed per namespace)")
	}
	for _, want := range []string{"/api/v1/namespaces/ns-a/pods", "/api/v1/namespaces/ns-b/pods"} {
		wi, ok := eng.watchIndex[want]
		if !ok {
			t.Errorf("expected watchIndex entry for %s", want)
			continue
		}
		if len(wi.Seqs) == 0 {
			t.Errorf("expected at least one event recorded for %s", want)
		}
	}
}

// TestRun_WildcardWatchBypassesConcurrencyCap verifies that a config using
// namespaces: ["*"] with watch: true does not trip the watch concurrency
// guard even on clusters with more than maxConcurrentWatchStreams namespaces,
// because wildcard watches collapse to a single cluster-wide stream.
func TestRun_WildcardWatchBypassesConcurrencyCap(t *testing.T) {
	nsCount := maxConcurrentWatchStreams + 50
	var nsItems strings.Builder
	for i := 0; i < nsCount; i++ {
		if i > 0 {
			nsItems.WriteString(",")
		}
		fmt.Fprintf(&nsItems, `{"metadata":{"name":"ns-%d"}}`, i)
	}
	nsList := fmt.Sprintf(`{"kind":"NamespaceList","metadata":{},"items":[%s]}`, nsItems.String())

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case "/api":
			fmt.Fprint(w, `{"versions":["v1"]}`)
		case "/api/v1":
			fmt.Fprint(w, `{"kind":"APIResourceList","resources":[]}`)
		case "/apis":
			fmt.Fprint(w, `{"kind":"APIGroupList","groups":[]}`)
		case "/openapi/v2":
			fmt.Fprint(w, `{}`)
		case "/openapi/v3":
			fmt.Fprint(w, `{"paths":{}}`)
		case "/api/v1/namespaces":
			fmt.Fprint(w, nsList)
		case "/api/v1/pods":
			if r.URL.Query().Get("watch") == "1" {
				w.WriteHeader(http.StatusOK)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				<-r.Context().Done()
				return
			}
			fmt.Fprint(w, `{"kind":"PodList","metadata":{"resourceVersion":"1"},"items":[]}`)
		default:
			fmt.Fprint(w, `{"kind":"List","items":[]}`)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		DurationRaw: "300ms",
		Duration:    300 * time.Millisecond,
		Output:      filepath.Join(t.TempDir(), "capture.khsrk"),
		Resources: []config.Resource{
			{
				Version:     "v1",
				Resource:    "pods",
				Namespaces:  []string{"*"},
				Watch:       true,
				IntervalRaw: "1s",
				Interval:    1 * time.Second,
			},
		},
	}

	eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
	if _, err := eng.Run(); err != nil {
		t.Fatalf("expected wildcard watch to bypass concurrency cap, got error: %v", err)
	}
	if !cfg.Resources[0].WildcardNamespaces {
		t.Fatal("expected wildcard expansion to flag resource as WildcardNamespaces=true")
	}
}

// TestFetchResource_AutoDiscoveredSilentFallback verifies that when a resource
// was added via auto-discovery (AutoDiscovered=true) and every namespace-scoped
// fetch returns 404, the engine falls back to the cluster-scoped path silently
// — no warning is written to stderr.
func TestFetchResource_AutoDiscoveredSilentFallback(t *testing.T) {
	clusterFetched := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
		case strings.Contains(r.URL.Path, "/namespaces/"):
			// All namespace-scoped fetches return 404.
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","code":404}`)
		case r.URL.Path == "/apis/image.openshift.io/v1/imagestreamimages":
			clusterFetched = true
			fmt.Fprint(w, `{"kind":"ImageStreamImageList","apiVersion":"image.openshift.io/v1","items":[]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Capture stderr to assert no warning is emitted.
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	eng := newEngineWith(&config.Config{}, srv.Client(), srv.URL, false)
	eng.sink = &sliceSink{}

	res := config.Resource{
		Group:          "image.openshift.io",
		Version:        "v1",
		Resource:       "imagestreamimages",
		Namespaces:     []string{"default", "production"},
		IntervalRaw:    "500ms",
		Interval:       500 * time.Millisecond,
		AutoDiscovered: true,
	}
	eng.fetchResource(context.Background(), res)

	w.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	stderrOut := buf.String()

	if strings.Contains(stderrOut, "[warn]") {
		t.Errorf("expected no [warn] on stderr for auto-discovered resource, got: %s", stderrOut)
	}
	if !clusterFetched {
		t.Error("expected engine to fall back to cluster-scoped path, but it was not fetched")
	}
}

// TestFetchResource_ExplicitNamespaceWarnOnAllNotFound verifies that when a
// manually-configured resource (AutoDiscovered=false) has all namespace-scoped
// fetches return 404, the warning IS printed to stderr when --verbose is set,
// and is suppressed when --verbose is not set.
func TestFetchResource_ExplicitNamespaceWarnOnAllNotFound(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"kind":"Status","status":"Failure","code":404}`)
	}))
	defer srv.Close()

	oldStderr := os.Stderr
	r, wPipe, _ := os.Pipe()
	os.Stderr = wPipe

	// verbose=true: warnings for explicit (non-auto-discovered) resources must
	// appear when --verbose is set.
	eng := newEngineWith(&config.Config{}, srv.Client(), srv.URL, true)
	eng.sink = &sliceSink{}

	res := config.Resource{
		Version:        "v1",
		Resource:       "widgets",
		Namespaces:     []string{"default"},
		IntervalRaw:    "500ms",
		Interval:       500 * time.Millisecond,
		AutoDiscovered: false,
	}
	eng.fetchResource(context.Background(), res)

	wPipe.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)

	if !strings.Contains(buf.String(), "[warn]") {
		t.Errorf("expected [warn] on stderr for explicit resource with all-404 namespaces, got: %s", buf.String())
	}
}

// TestFetchResource_ExplicitNamespaceNoWarnWithoutVerbose verifies that the
// allNotFound fallback warning is suppressed when verbose=false.
func TestFetchResource_ExplicitNamespaceNoWarnWithoutVerbose(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"kind":"Status","status":"Failure","code":404}`)
	}))
	defer srv.Close()

	oldStderr := os.Stderr
	r, wPipe, _ := os.Pipe()
	os.Stderr = wPipe

	eng := newEngineWith(&config.Config{}, srv.Client(), srv.URL, false)
	eng.sink = &sliceSink{}

	res := config.Resource{
		Version:        "v1",
		Resource:       "widgets",
		Namespaces:     []string{"default"},
		IntervalRaw:    "500ms",
		Interval:       500 * time.Millisecond,
		AutoDiscovered: false,
	}
	eng.fetchResource(context.Background(), res)

	wPipe.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)

	if strings.Contains(buf.String(), "[warn]") {
		t.Errorf("expected NO [warn] on stderr when verbose=false, got: %s", buf.String())
	}
}

// TestFetchResource_ExplicitNamespaceWarnDedup verifies that the same
// allNotFound warning is emitted at most once per unique cluster-scoped path
// within a single engine run.
func TestFetchResource_ExplicitNamespaceWarnDedup(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			fmt.Fprint(w, `{"gitVersion":"v1.29.0"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"kind":"Status","status":"Failure","code":404}`)
	}))
	defer srv.Close()

	oldStderr := os.Stderr
	r, wPipe, _ := os.Pipe()
	os.Stderr = wPipe

	eng := newEngineWith(&config.Config{}, srv.Client(), srv.URL, true)
	eng.sink = &sliceSink{}

	res := config.Resource{
		Version:        "v1",
		Resource:       "widgets",
		Namespaces:     []string{"default"},
		IntervalRaw:    "500ms",
		Interval:       500 * time.Millisecond,
		AutoDiscovered: false,
	}
	// Call fetchResource twice for the same resource — warning must appear only once.
	eng.fetchResource(context.Background(), res)
	eng.fetchResource(context.Background(), res)

	wPipe.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)

	count := strings.Count(buf.String(), "[warn]")
	if count != 1 {
		t.Errorf("expected exactly 1 [warn] for duplicate resource, got %d:\n%s", count, buf.String())
	}
}
