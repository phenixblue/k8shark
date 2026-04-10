package capture

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/config"
)

// fakeCaptureServer returns a TLS server with realistic responses for
// benchmark use. It serves pods, nodes, and a /version endpoint.
func fakeCaptureServer(b *testing.B) *httptest.Server {
	b.Helper()
	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[` +
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"nginx","namespace":"default"}},` +
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"redis","namespace":"default"}}]}`
	nodeList := `{"apiVersion":"v1","kind":"NodeList","metadata":{},"items":[` +
		`{"apiVersion":"v1","kind":"Node","metadata":{"name":"node1"}},` +
		`{"apiVersion":"v1","kind":"Node","metadata":{"name":"node2"}}]}`

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
			fmt.Fprint(w, `{"kind":"Status","code":404}`)
		}
	}))
	return srv
}

// BenchmarkEngine_CaptureToArchive measures a full capture run writing to a
// tar.gz archive.
func BenchmarkEngine_CaptureToArchive(b *testing.B) {
	srv := fakeCaptureServer(b)
	defer srv.Close()

	for i := 0; i < b.N; i++ {
		cfg := &config.Config{
			DurationRaw: "500ms",
			Duration:    500 * time.Millisecond,
			Output:      filepath.Join(b.TempDir(), "capture.tar.gz"),
			Resources: []config.Resource{
				{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
				{Version: "v1", Resource: "nodes", IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
			},
		}
		eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
		if _, err := eng.Run(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEngine_CaptureToNDJSON measures a full capture run writing to NDJSON
// (no I/O bottleneck from gzip compression).
func BenchmarkEngine_CaptureToNDJSON(b *testing.B) {
	srv := fakeCaptureServer(b)
	defer srv.Close()

	for i := 0; i < b.N; i++ {
		cfg := &config.Config{
			DurationRaw: "500ms",
			Duration:    500 * time.Millisecond,
			Output:      "-",
			Resources: []config.Resource{
				{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
				{Version: "v1", Resource: "nodes", IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
			},
		}
		eng := newEngineWith(cfg, srv.Client(), srv.URL, false)
		eng.sink = archive.NewNDJSONWriter(&bytes.Buffer{})
		if _, err := eng.Run(); err != nil {
			b.Fatal(err)
		}
	}
}
