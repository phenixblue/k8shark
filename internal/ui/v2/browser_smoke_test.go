//go:build browser

// Headless-browser coverage for static/app.js (#263).
//
// Go-side tests cover the JSON payloads; nothing exercised the 2,600-line
// app.js, which is where the UI's functional bugs have actually lived (the
// scrubber step-forward no-op in #257, the route-change race in #262). These
// tests load every route in real Chrome against a real example capture and fail
// on any console error or uncaught exception.
//
// Behind the `browser` build tag on purpose: the default `go test ./...` runs on
// three OSes in CI, and requiring Chrome there would triple the flake surface
// for no extra signal. The dedicated `ui-browser` job runs
// `go test -tags browser ./internal/ui/v2/` on ubuntu. The XSS *source*
// invariants in xss_invariant_test.go carry no tag and run everywhere.
//
// Run locally with:
//
//	go test -tags browser ./internal/ui/v2/ -v
package v2

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/store"
)

// exampleCapture is a capture committed to the repo, so CI has it without
// generating one. auto-discovery has the widest resource coverage of the
// examples (34 resource types), which is what makes the catalog and resource
// routes render something real rather than an empty state.
const exampleCapture = "../../../examples/auto-discovery/capture.kshrk"

// Route loads settle in ~50-110ms locally, so these bounds are pure headroom for
// a slower CI runner. They are kept tight-ish on purpose: they are also the cost
// of a *failing* route, and chromedp.Poll's 30s default meant a genuinely broken
// UI took 30s per route to report.
const (
	pollTimeout     = 20 * time.Second
	perRouteTimeout = 30 * time.Second
)

// pageProbe runs before app.js on every document. It gives the tests three
// things the CDP event stream alone can't: a count of in-flight fetches, so
// waiting is deterministic instead of a sleep; an in-page record of error
// events; and the __xss sentinel the injection payloads try to set.
//
// Two independent error channels (this and the CDP events below) is deliberate.
// The failure mode that matters for a smoke test is a *silent* one, where the
// harness reports green because it never observed anything.
const pageProbe = `
window.__kshrkPending = 0;
window.__kshrkErrors = [];
window.__xss = 0;
(function () {
  const origFetch = window.fetch;
  window.fetch = function (...args) {
    window.__kshrkPending++;
    return origFetch.apply(this, args).finally(() => { window.__kshrkPending--; });
  };
  window.addEventListener('error', (e) => {
    window.__kshrkErrors.push('error: ' + String((e && (e.message || e.error)) || e));
  });
  window.addEventListener('unhandledrejection', (e) => {
    window.__kshrkErrors.push('unhandledrejection: ' + String((e && e.reason) || e));
  });
})();
`

// readyExpr is true once the route has actually painted and no fetch is still
// outstanding. Checking childElementCount matters: without it a route that threw
// before rendering would look identical to one that rendered cleanly.
const readyExpr = `
(function () {
  const c = document.getElementById('content');
  return document.readyState === 'complete' &&
         window.__kshrkPending === 0 &&
         !!c && c.childElementCount > 0;
})()
`

// awaitPromise lets an Evaluate expression return a promise.
var awaitPromise = func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

// collector accumulates errors seen through the CDP event stream.
type collector struct {
	mu         sync.Mutex
	consoleErr []string
	exceptions []string
}

func (c *collector) add(kind, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if kind == "console" {
		c.consoleErr = append(c.consoleErr, msg)
		return
	}
	c.exceptions = append(c.exceptions, msg)
}

func (c *collector) snapshot() (console, exc []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.consoleErr...), append([]string(nil), c.exceptions...)
}

func (c *collector) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.consoleErr, c.exceptions = nil, nil
}

// serve mounts the real v2 mux over the given store.
func serve(t *testing.T, cs *store.CaptureStore, archivePath string) *httptest.Server {
	t.Helper()
	h := &Handler{Store: cs, ArchivePath: archivePath}
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newUIServer serves the real v2 mux over a real example capture.
func newUIServer(t *testing.T) *httptest.Server {
	t.Helper()
	abs, err := filepath.Abs(exampleCapture)
	if err != nil {
		t.Fatalf("resolving %s: %v", exampleCapture, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("example capture missing at %s: %v\n"+
			"    These tests deliberately run against a committed capture rather than a\n"+
			"    synthetic one, so they exercise real payload shapes.", abs, err)
	}
	ar, err := archive.Open(abs)
	if err != nil {
		t.Fatalf("archive.Open(%s): %v", abs, err)
	}
	// Registered before the store's cleanup so it runs *after* it: t.Cleanup is
	// LIFO, and LoadStore's background enrichment goroutine keeps reading from
	// the archive until Close returns (see the archive lifecycle notes in
	// CLAUDE.md). Closing the zip first would pull it out from under that
	// goroutine.
	t.Cleanup(func() { _ = ar.Close() })
	cs, err := store.LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	t.Cleanup(cs.Close)
	return serve(t, cs, abs)
}

// newBrowser starts headless Chrome and wires up error collection.
func newBrowser(t *testing.T) (context.Context, *collector) {
	t.Helper()
	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.DisableGPU,
		chromedp.NoSandbox, // required in most CI containers
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	t.Cleanup(cancelAlloc)

	ctx, cancel := chromedp.NewContext(allocCtx)
	t.Cleanup(cancel)

	col := &collector{}
	chromedp.ListenTarget(ctx, func(ev any) {
		switch e := ev.(type) {
		case *runtime.EventConsoleAPICalled:
			// console.error and console.assert are failures. Warnings and logs
			// are not: the app logs legitimately.
			if e.Type != "error" && e.Type != "assert" {
				return
			}
			parts := make([]string, 0, len(e.Args))
			for _, a := range e.Args {
				parts = append(parts, argText(a))
			}
			col.add("console", strings.Join(parts, " "))
		case *runtime.EventExceptionThrown:
			col.add("exception", e.ExceptionDetails.Error())
		}
	})

	// Fail with the actionable message rather than a raw exec error when Chrome
	// is missing. This is the one setup problem a contributor is likely to hit.
	if err := chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(pageProbe).Do(ctx)
			return err
		}),
		chromedp.Navigate("about:blank"),
	); err != nil {
		t.Fatalf("could not start headless Chrome: %v\n"+
			"    These tests need a Chrome/Chromium binary. They are gated behind the\n"+
			"    `browser` build tag so the normal `go test ./...` never requires one.\n"+
			"    Install Chrome, or drop -tags browser to skip them.", err)
	}
	return ctx, col
}

func argText(a *runtime.RemoteObject) string {
	if a == nil {
		return "<nil>"
	}
	if len(a.Value) > 0 {
		var s string
		if err := json.Unmarshal(a.Value, &s); err == nil {
			return s
		}
		return string(a.Value)
	}
	if a.Description != "" {
		return a.Description
	}
	return string(a.Type)
}

// loadCounter makes every navigation URL unique. Two reasons, both load-bearing:
// a URL that differs from the current one only in its fragment does NOT reload
// the document, so consecutive routes would have been asserted against the
// previous route's DOM — passing on stale content. And the unique token gives the
// freshness assertion below something to check.
var loadCounter atomic.Int64

// loadRoute does a full document load of a route, waits for it to settle, and
// returns what rendered plus any in-page errors.
//
// The page URL carries the token in its *query*, not its fragment: app.js parses
// every route and route param out of location.hash and never reads
// location.search, so the token cannot change what renders.
func loadRoute(t *testing.T, ctx context.Context, base, hash string) (children, textLen int, pageErrs []string) {
	t.Helper()
	rctx, cancel := context.WithTimeout(ctx, perRouteTimeout)
	defer cancel()

	token := fmt.Sprintf("load-%d", loadCounter.Add(1))
	pageURL := base + "/v2/?" + token + hash

	var gotSearch, gotHash string
	err := chromedp.Run(rctx,
		chromedp.Navigate(pageURL),
		// Binds to the freshly-created document before anything is evaluated
		// against it; without this the first Evaluate can race the navigation
		// and fail with "Cannot find context with specified id".
		chromedp.WaitReady("body", chromedp.ByQuery),
		chromedp.Poll(readyExpr, nil,
			chromedp.WithPollingInterval(50*time.Millisecond),
			chromedp.WithPollingTimeout(pollTimeout)),
		chromedp.Evaluate(`document.getElementById('content').childElementCount`, &children),
		chromedp.Evaluate(`(document.getElementById('content').textContent || '').trim().length`, &textLen),
		chromedp.Evaluate(`window.__kshrkErrors`, &pageErrs),
		chromedp.Evaluate(`location.search`, &gotSearch),
		chromedp.Evaluate(`location.hash`, &gotHash),
	)
	if err != nil {
		t.Errorf("%s: did not settle: %v\n"+
			"    Either the route threw before painting, or a fetch never resolved.", hash, err)
		return 0, 0, pageErrs
	}
	// Freshness: prove these numbers describe the document we just asked for and
	// not the one left over from the previous subtest.
	if !strings.Contains(gotSearch, token) {
		t.Errorf("%s: expected a fresh document carrying %q but location.search is %q — "+
			"the assertions for this route describe a stale page", hash, token, gotSearch)
	}
	if gotHash != hash {
		t.Errorf("%s: document settled on hash %q instead", hash, gotHash)
	}
	return children, textLen, pageErrs
}

// assertClean reports every error channel for a route.
func assertClean(t *testing.T, label string, col *collector, pageErrs []string) {
	t.Helper()
	consoleErrs, exceptions := col.snapshot()
	for _, e := range consoleErrs {
		t.Errorf("%s: console error: %s", label, e)
	}
	for _, e := range exceptions {
		t.Errorf("%s: uncaught exception: %s", label, e)
	}
	for _, e := range pageErrs {
		t.Errorf("%s: page error event: %s", label, e)
	}
}

// TestBrowser_EveryRouteLoadsWithoutConsoleErrors is the first acceptance
// criterion of #263. The route list mirrors the dispatch table in app.js's
// render(); TestBrowser_RouteListMatchesRenderDispatch keeps the two in sync.
func TestBrowser_EveryRouteLoadsWithoutConsoleErrors(t *testing.T) {
	srv := newUIServer(t)
	ctx, col := newBrowser(t)

	ns, pod := firstNamespaceAndPod(t, srv.URL)
	podsPath := url.QueryEscape("/api/v1/namespaces/" + ns + "/pods")

	routes := []struct {
		name string
		hash string
	}{
		{"overview", "#/overview"},
		{"namespaces", "#/namespaces"},
		{"pods", "#/pods"},
		{"workloads", "#/workloads"},
		{"resources", "#/resources"},
		{"resource", "#/resource?path=" + podsPath},
		{"object", "#/object?path=" + podsPath + "&name=" + url.QueryEscape(pod)},
		{"namespace", "#/ns/" + url.PathEscape(ns)},
		{"pod", "#/ns/" + url.PathEscape(ns) + "/pod/" + url.PathEscape(pod)},
		{"diagnostics", "#/diagnostics"},
		{"timeline", "#/timeline"},
		{"logs", "#/logs?ns=" + url.QueryEscape(ns) + "&pod=" + url.QueryEscape(pod)},
		{"diff", "#/diff"},
		{"search", "#/search?q=widget&mode=all"},
		// Not in the dispatch table: parseRoute must fall back to overview
		// rather than throwing.
		{"unknown-route-falls-back", "#/does-not-exist"},
	}

	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			col.reset()
			children, textLen, pageErrs := loadRoute(t, ctx, srv.URL, r.hash)
			assertClean(t, r.hash, col, pageErrs)

			// Guard against a vacuous pass: a harness that navigated nowhere
			// would report no errors for every route.
			if children == 0 {
				t.Errorf("%s: #content has no child elements — nothing rendered, so the "+
					"no-errors result above is meaningless", r.hash)
			}
			if textLen < 10 {
				t.Errorf("%s: #content holds only %d characters of text; the route almost "+
					"certainly failed to render", r.hash, textLen)
			}
		})
	}
}

// routeCase matches render()'s dispatch arms in app.js.
var routeCase = regexp.MustCompile(`r\.name === '([a-z]+)'`)

// TestBrowser_RouteListMatchesRenderDispatch keeps the table above honest. A new
// route added to render() but not here would go untested while CI stayed green —
// the coverage gap this issue exists to close, reopened silently.
func TestBrowser_RouteListMatchesRenderDispatch(t *testing.T) {
	src := readStaticSources(t)["app.js"]
	if src == "" {
		t.Fatal("app.js not found in the embedded static FS")
	}
	dispatched := map[string]bool{}
	for _, m := range routeCase.FindAllStringSubmatch(src, -1) {
		dispatched[m[1]] = true
	}
	if len(dispatched) == 0 {
		t.Fatal("found no `r.name === '...'` arms in render(); this scan needs updating, " +
			"and until it is the check passes vacuously")
	}
	// Route names covered by TestBrowser_EveryRouteLoadsWithoutConsoleErrors.
	covered := map[string]bool{
		"overview": true, "namespaces": true, "pods": true, "workloads": true,
		"resources": true, "resource": true, "object": true, "namespace": true,
		"pod": true, "diagnostics": true, "timeline": true, "logs": true,
		"diff": true, "search": true,
	}
	for name := range dispatched {
		if !covered[name] {
			t.Errorf("render() dispatches route %q but the browser smoke test never loads "+
				"it; add it to the routes table", name)
		}
	}
	for name := range covered {
		if !dispatched[name] {
			t.Errorf("the browser smoke test loads route %q but render() no longer "+
				"dispatches it; the table is stale", name)
		}
	}
}

// xssPayloads are strings that execute if they reach an HTML parser.
var xssPayloads = []string{
	`<img src=x onerror="window.__xss=1">`,
	`<script>window.__xss=1</script>`,
	`"><svg onload="window.__xss=1">`,
	`<iframe src="javascript:window.__xss=1"></iframe>`,
	`<a href="javascript:window.__xss=1">click</a>`,
	`</div><img src=x onerror="window.__xss=1">`,
}

// hostileCaptureServer builds a capture whose object names, labels, annotations
// and container images are XSS payloads, then serves it. The point is to drive
// the payloads through the app's *own* render path — asserting that
// document.createTextNode doesn't parse HTML would test Chrome, not app.js.
func hostileCaptureServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	const ns = "default"
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	items := make([]string, 0, len(xssPayloads))
	for i, p := range xssPayloads {
		esc, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("marshaling payload: %v", err)
		}
		s := string(esc)
		// Same payload in the name, a label value, an annotation value and the
		// container image: four different render paths in the pod views.
		items = append(items, fmt.Sprintf(`{
          "metadata": {
            "name": %s,
            "namespace": %q,
            "uid": "uid-%d",
            "labels": {"app": %s},
            "annotations": {"kshrk.io/note": %s},
            "creationTimestamp": %q
          },
          "spec": {"nodeName": %s, "containers": [{"name": "c0", "image": %s}]},
          "status": {
            "phase": "Running",
            "containerStatuses": [{"name": "c0", "ready": true, "restartCount": 0, "image": %s}]
          }
        }`, s, ns, i, s, s, now.Format(time.RFC3339), s, s, s))
	}
	body := fmt.Sprintf(`{"apiVersion":"v1","kind":"PodList","items":[%s]}`,
		strings.Join(items, ","))

	path := "/api/v1/namespaces/" + ns + "/pods"
	recs := []*capture.Record{{
		ID: "hostile-0", CapturedAt: now, APIPath: path, HTTPMethod: "GET",
		ResponseCode: 200, ResponseBody: json.RawMessage(body),
	}}
	idx := capture.Index{path: {
		APIPath: path, Seqs: []int{0},
		Times: []time.Time{now}, Counts: []int{len(xssPayloads)},
	}}
	meta := &capture.CaptureMetadata{
		CaptureID: "hostile", CapturedAt: now.Add(-time.Minute),
		CapturedUntil: now, RecordCount: len(recs),
	}
	cs := buildV2TestStore(t, recs, idx, meta)
	return serve(t, cs, "hostile.kshrk"), ns
}

// TestBrowser_HostileCaptureDataRendersAsText is the second acceptance criterion
// of #263, from the opposite side of xss_invariant_test.go. That one asserts the
// source never reaches an HTML parser; this asserts the observable consequence
// with real payloads in real Chrome. Both matter: the source scan would miss a
// vector introduced through an attribute or a dependency, and this would miss a
// sink on a route it doesn't happen to render.
func TestBrowser_HostileCaptureDataRendersAsText(t *testing.T) {
	srv, ns := hostileCaptureServer(t)
	ctx, col := newBrowser(t)

	// The pods list and the pod drilldown are where names, labels, annotations
	// and images are rendered.
	for _, r := range []struct{ name, hash string }{
		{"pods-list", "#/pods"},
		{"namespace", "#/ns/" + url.PathEscape(ns)},
		{"pod-drilldown", "#/ns/" + url.PathEscape(ns) + "/pod/" + url.PathEscape(xssPayloads[0])},
	} {
		t.Run(r.name, func(t *testing.T) {
			col.reset()
			children, textLen, pageErrs := loadRoute(t, ctx, srv.URL, r.hash)
			assertClean(t, r.hash, col, pageErrs)
			if children == 0 || textLen < 10 {
				t.Fatalf("%s: nothing rendered (children=%d textLen=%d); the XSS "+
					"assertions below would be vacuous", r.hash, children, textLen)
			}

			var res struct {
				XSSFired     bool     `json:"xssFired"`
				InjectedTags []string `json:"injectedTags"`
				JSHrefs      []string `json:"jsHrefs"`
				Text         string   `json:"text"`
			}
			// app.js builds no svg/img/iframe/object/embed/script anywhere, so
			// any such element inside #content can only have come from a parsed
			// payload.
			const probe = `
(function () {
  const c = document.getElementById('content');
  const injected = Array.from(c.querySelectorAll('img,script,iframe,svg,object,embed,link,style'))
    .map((e) => e.tagName.toLowerCase());
  const jsHrefs = Array.from(c.querySelectorAll('[href],[src]'))
    .map((e) => (e.getAttribute('href') || e.getAttribute('src') || ''))
    .filter((v) => /^\s*javascript:/i.test(v));
  return {
    xssFired: window.__xss === 1,
    injectedTags: injected,
    jsHrefs: jsHrefs,
    text: c.textContent || '',
  };
})()
`
			if err := chromedp.Run(ctx, chromedp.Evaluate(probe, &res)); err != nil {
				t.Fatalf("evaluating the XSS probe: %v", err)
			}

			if res.XSSFired {
				t.Errorf("%s: a payload executed — window.__xss was set", r.hash)
			}
			if len(res.InjectedTags) > 0 {
				t.Errorf("%s: payloads were parsed into elements %v; capture data must "+
					"only ever become text", r.hash, res.InjectedTags)
			}
			if len(res.JSHrefs) > 0 {
				t.Errorf("%s: javascript: URL survived into a live attribute: %v",
					r.hash, res.JSHrefs)
			}
			// Non-vacuity: the payload must actually be on the page, as text.
			// Without this, a view that silently dropped the field would pass.
			if !strings.Contains(res.Text, xssPayloads[0]) {
				t.Errorf("%s: the payload never appeared as text, so this route may not "+
					"render the hostile fields at all — the assertions above prove nothing.\n"+
					"    wanted substring: %q", r.hash, xssPayloads[0])
			}
		})
	}

	// A control: the detector must be able to see an execution at all, or every
	// assertion above passes for the wrong reason.
	t.Run("positive-control", func(t *testing.T) {
		var fired bool
		const control = `
(function () {
  window.__xss = 0;
  const d = document.createElement('div');
  document.body.appendChild(d);
  d.innerHTML = '<img src="data:," onerror="window.__xss=1">';
  const img = d.querySelector('img');
  if (img) { img.src = 'x-does-not-exist'; }
  return new Promise((resolve) => setTimeout(() => {
    const fired = window.__xss === 1;
    d.remove();
    window.__xss = 0;
    resolve(fired);
  }, 250));
})()
`
		if err := chromedp.Run(ctx, chromedp.Evaluate(control, &fired, awaitPromise)); err != nil {
			t.Fatalf("running the positive control: %v", err)
		}
		if !fired {
			t.Error("the positive control did not fire: an onerror handler injected via " +
				"innerHTML failed to execute in this browser, so these tests cannot detect " +
				"a real XSS and their passes mean nothing")
		}
	})
}

// firstNamespaceAndPod pulls a real namespace/pod pair out of the served capture
// so the drilldown routes exercise real data instead of an empty state.
func firstNamespaceAndPod(t *testing.T, base string) (ns, pod string) {
	t.Helper()
	resp, err := http.Get(base + "/v2/api/pods")
	if err != nil {
		t.Fatalf("GET /v2/api/pods: %v", err)
	}
	defer resp.Body.Close()
	var payload struct {
		Pods []struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"pods"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decoding /v2/api/pods: %v", err)
	}
	if len(payload.Pods) == 0 {
		t.Fatal("the example capture has no pods; the drilldown routes would be tested " +
			"against an empty state")
	}
	return payload.Pods[0].Namespace, payload.Pods[0].Name
}
