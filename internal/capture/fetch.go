package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

// fetchDiscovery captures the Kubernetes API discovery endpoints so the mock
// server can replay them with real resource lists rather than inferring them
// from the captured resource paths. Called once at the start of a capture run.
// Bodies for /apis and each /apis/<group>/<version> are saved into
// e.discoveryCache so that autoDiscoverResources can use them without issuing
// a second round of HTTP requests to the live cluster.
func (e *Engine) fetchDiscovery(ctx context.Context) {
	// Core discovery paths.
	e.doFetch(ctx, "/api", "", true)
	apiV1Body, _ := e.doFetch(ctx, "/api/v1", "", true)
	if apiV1Body != nil {
		e.discoveryCache["/api/v1"] = apiV1Body
	}
	apisBody, _ := e.doFetch(ctx, "/apis", "", true)
	if apisBody != nil {
		e.discoveryCache["/apis"] = apisBody
	}

	// OpenAPI specs for kubectl explain.
	e.doFetch(ctx, "/openapi/v2", "", true)
	openapiV3Body, _ := e.doFetch(ctx, "/openapi/v3", "", true)
	if openapiV3Body != nil {
		// Parse the v3 path index and fetch each per-group spec. These are
		// numerous (100+ on OpenShift) and independent, so fetch them
		// concurrently — done sequentially they can dominate (or exhaust) the
		// whole capture window before polling/watching even begins.
		var v3Index struct {
			Paths map[string]json.RawMessage `json:"paths"`
		}
		if err := json.Unmarshal(openapiV3Body, &v3Index); err == nil {
			paths := make([]string, 0, len(v3Index.Paths))
			for p := range v3Index.Paths {
				paths = append(paths, "/openapi/v3/"+p)
			}
			e.fetchDiscoveryPaths(ctx, paths, false)
		}
	}

	// Parse /apis to discover all non-core group-versions and capture each.
	if apisBody == nil {
		return
	}
	var groupList struct {
		Groups []struct {
			Versions []struct {
				GroupVersion string `json:"groupVersion"`
			} `json:"versions"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(apisBody, &groupList); err != nil {
		return
	}
	gvPaths := make([]string, 0, len(groupList.Groups))
	for _, g := range groupList.Groups {
		for _, v := range g.Versions {
			gvPaths = append(gvPaths, "/apis/"+v.GroupVersion)
		}
	}
	e.fetchDiscoveryPaths(ctx, gvPaths, true)
}

// fetchDiscoveryPaths GETs each path concurrently (bounded), recording every
// response via doFetch. When cache is true, successful bodies are also stored
// in discoveryCache keyed by path so autoDiscoverResources can reuse them
// without re-fetching. Concurrency keeps discovery from dominating short
// captures on clusters with many API groups.
func (e *Engine) fetchDiscoveryPaths(ctx context.Context, paths []string, cache bool) {
	const discoveryWorkers = 16
	sem := make(chan struct{}, discoveryWorkers)
	var wg sync.WaitGroup
	for _, p := range paths {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			body, _ := e.doFetch(ctx, path, "", true)
			if cache && body != nil {
				e.mu.Lock()
				e.discoveryCache[path] = body
				e.mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
}

// doFetch issues one GET for apiPath. When tableKeySuffix is non-empty the
// request uses a Table Accept header and the response is stored under
// apiPath+tableKeySuffix in the index. Returns the response body and HTTP
// status code, or (nil, 0) when the request could not be completed.
func (e *Engine) doFetch(ctx context.Context, apiPath, tableKeySuffix string, dedupEnabled bool) ([]byte, int) {
	body, code := e.rawFetch(ctx, apiPath, tableKeySuffix)
	if body == nil {
		return nil, code
	}
	e.storeRecord(apiPath+tableKeySuffix, body, code, dedupEnabled)
	return body, code
}

// rawFetch issues one GET against apiPath and returns the response body and
// status code without writing any record. Callers compose this with
// storeRecord when they want a single fetch persisted as-is, or with the
// cluster-wide demux when they want to split one response into per-namespace
// records. Returns (nil, statusCode) on transport/read errors, empty bodies,
// or cancellation.
func (e *Engine) rawFetch(ctx context.Context, apiPath, tableKeySuffix string) ([]byte, int) {
	url := e.baseURL + apiPath

	// Bound this fetch independently so a stalled HTTP/2 stream can't hang
	// io.ReadAll forever (see perFetchTimeout). The deadline covers both the
	// request and the body read because it rides on the request context.
	timeout := e.fetchTimeout
	if timeout <= 0 {
		timeout = perFetchTimeout
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, url, nil)
	if err != nil {
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] build request %s: %v\n", apiPath, err)
		}
		return nil, 0
	}

	if tableKeySuffix != "" {
		req.Header.Set("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1")
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0
		}
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] GET %s: %v\n", apiPath, err)
		}
		return nil, 0
	}
	defer resp.Body.Close()

	// Acquire a read slot without blocking past this fetch's deadline, so a
	// saturated semaphore can't pin a timed-out fetch indefinitely.
	select {
	case e.fetchSem <- struct{}{}:
	case <-fetchCtx.Done():
		return nil, 0
	}
	body, err := io.ReadAll(resp.Body)
	<-e.fetchSem
	if err != nil {
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] read body %s: %v\n", apiPath, err)
		}
		return nil, 0
	}

	if tableKeySuffix == "" && resp.StatusCode == http.StatusForbidden {
		fmt.Fprintf(os.Stderr, "  [warn] RBAC denied: %s (check cluster permissions)\n", apiPath)
	}

	// Skip empty bodies — storing json.RawMessage("") would corrupt the archive.
	if len(body) == 0 {
		return nil, resp.StatusCode
	}

	// Skip non-JSON bodies — a proxying error page (HTML 502, a plaintext
	// error from an aggregated APIService) is a non-empty body that would
	// otherwise reach storeRecord and fail to marshal as a record.
	if !json.Valid(body) {
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] non-JSON body %s (status %d)\n", apiPath, resp.StatusCode)
		}
		return nil, resp.StatusCode
	}

	if e.verbose {
		label := apiPath
		if tableKeySuffix != "" {
			label += tableKeySuffix
		}
		fmt.Fprintf(os.Stdout, "  [capture] %s -> %d\n", label, resp.StatusCode)
	}
	return body, resp.StatusCode
}
