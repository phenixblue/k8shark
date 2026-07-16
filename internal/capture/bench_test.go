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

// inProcessTransport serves requests by invoking an http.Handler directly,
// with no real TCP dial or TLS handshake. Benchmarks use it instead of a real
// httptest.Server (see fakeCaptureClient) because handshake cost and socket
// scheduling vary run-to-run enough to dominate B/op measurements; what these
// benchmarks measure is the capture engine's poll/decode/archive-write path,
// not the network transport.
type inProcessTransport struct {
	handler http.Handler
}

func (t *inProcessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// fakeCaptureClient returns an HTTP client wired to an in-process handler
// serving pods, nodes, and a /version endpoint, plus the base URL to pass to
// newEngineWith.
func fakeCaptureClient() (*http.Client, string) {
	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[` +
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"nginx","namespace":"default"}},` +
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"redis","namespace":"default"}}]}`
	nodeList := `{"apiVersion":"v1","kind":"NodeList","metadata":{},"items":[` +
		`{"apiVersion":"v1","kind":"Node","metadata":{"name":"node1"}},` +
		`{"apiVersion":"v1","kind":"Node","metadata":{"name":"node2"}}]}`

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})

	client := &http.Client{Transport: &inProcessTransport{handler: handler}}
	return client, "http://fake-api-server"
}

// benchPollPasses fixes the number of times each resource is fetched per
// capture run, matching what a 500ms window at a 200ms poll interval would
// have produced (immediate fetch + 2 ticks). Engine.pollPasses bypasses the
// real time.Ticker so each benchmark iteration completes as fast as the fake
// handler can respond rather than blocking for the configured wall-clock
// window, letting b.N scale into the thousands.
const benchPollPasses = 3

func benchConfig(output string) *config.Config {
	return &config.Config{
		DurationRaw: "500ms",
		Duration:    500 * time.Millisecond,
		Output:      output,
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
			{Version: "v1", Resource: "nodes", IntervalRaw: "200ms", Interval: 200 * time.Millisecond},
		},
	}
}

// BenchmarkEngine_CaptureToArchive measures a full capture run writing to a
// tar.gz archive.
func BenchmarkEngine_CaptureToArchive(b *testing.B) {
	client, baseURL := fakeCaptureClient()

	for i := 0; i < b.N; i++ {
		cfg := benchConfig(filepath.Join(b.TempDir(), "capture.kshrk"))
		eng := newEngineWith(cfg, client, baseURL, false)
		eng.pollPasses = benchPollPasses
		if _, err := eng.Run(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEngine_CaptureToNDJSON measures a full capture run writing to NDJSON
// (no I/O bottleneck from gzip compression).
func BenchmarkEngine_CaptureToNDJSON(b *testing.B) {
	client, baseURL := fakeCaptureClient()

	for i := 0; i < b.N; i++ {
		cfg := benchConfig("-")
		eng := newEngineWith(cfg, client, baseURL, false)
		eng.sink = archive.NewNDJSONWriter(&bytes.Buffer{})
		eng.pollPasses = benchPollPasses
		if _, err := eng.Run(); err != nil {
			b.Fatal(err)
		}
	}
}
