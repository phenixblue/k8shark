package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/config"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// CaptureSummary holds statistics about a completed capture run.
type CaptureSummary struct {
	OutputPath    string
	OutputSize    int64
	RecordCount   int
	ResourceCount int // distinct API paths captured
	Duration      time.Duration
}

// Engine orchestrates the capture loop.
type Engine struct {
	cfg        *config.Config
	verbose    bool
	httpClient *http.Client
	baseURL    string
	mu         sync.Mutex
	index      Index
	sink       archive.RecordSink // set by Run(); exposed for tests
}

// NewEngine creates a capture Engine from validated config.
func NewEngine(cfg *config.Config, verbose bool) (*Engine, error) {
	var restCfg *rest.Config
	var err error

	if cfg.Kubeconfig != "" {
		restCfg, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	} else {
		restCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("building kubeconfig: %w", err)
	}

	httpClient, err := rest.HTTPClientFor(restCfg)
	if err != nil {
		return nil, fmt.Errorf("building HTTP client: %w", err)
	}

	return &Engine{
		cfg:        cfg,
		verbose:    verbose,
		httpClient: httpClient,
		baseURL:    restCfg.Host,
		index:      make(Index),
	}, nil
}

// newEngineWith constructs an Engine with a pre-built HTTP client and base URL.
// Used in tests to inject a fake API server.
func newEngineWith(cfg *config.Config, client *http.Client, baseURL string, verbose bool) *Engine {
	return &Engine{
		cfg:        cfg,
		verbose:    verbose,
		httpClient: client,
		baseURL:    baseURL,
		index:      make(Index),
	}
}

// Run executes the capture and writes the output archive.
func (e *Engine) Run() (*CaptureSummary, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.Duration)
	defer cancel()

	// Install SIGTERM/SIGINT handler so the capture can be wound down gracefully:
	// the context is cancelled, polling stops, and Finish() still writes a valid
	// (partial) archive.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Create the record sink (only if not pre-set by tests).
	var err error
	if e.sink == nil {
		if e.cfg.Output == "-" {
			e.sink = archive.NewNDJSONWriter(os.Stdout)
		} else {
			e.sink, err = archive.NewStreamWriter(e.cfg.Output)
			if err != nil {
				return nil, err
			}
		}
	}

	// Expand wildcard namespaces before polling begins.
	if err := e.expandWildcardNamespaces(ctx); err != nil {
		return nil, err
	}

	// Collect server version for metadata.
	kVersion, serverAddr := e.fetchServerVersion(ctx)

	// Capture API discovery endpoints so the mock server can replay them faithfully.
	e.fetchDiscovery(ctx)

	var wg sync.WaitGroup
	for _, res := range e.cfg.Resources {
		wg.Add(1)
		go func(r config.Resource) {
			defer wg.Done()
			e.pollResource(ctx, r)
		}(res)
	}
	wg.Wait()

	// Fetch pod logs for any pods resource entry with logs > 0. This runs after
	// all polling so we capture the most recent log state. A short background
	// context is used because the main capture context has already expired.
	for _, res := range e.cfg.Resources {
		if res.Logs > 0 && res.Resource == "pods" {
			logCtx, logCancel := context.WithTimeout(context.Background(), 30*time.Second)
			e.fetchPodsLogs(logCtx, res)
			logCancel()
		}
	}

	meta := &CaptureMetadata{
		CaptureID:         uuid.New().String(),
		CapturedAt:        time.Now().UTC().Add(-e.cfg.Duration),
		CapturedUntil:     time.Now().UTC(),
		KubernetesVersion: kVersion,
		ServerAddress:     serverAddr,
		RecordCount:       e.sink.RecordCount(),
	}

	if e.verbose {
		fmt.Fprintf(os.Stdout, "  captured %d records\n", e.sink.RecordCount())
	}

	if err := e.sink.Finish(meta, e.index); err != nil {
		return nil, err
	}

	var outputSize int64
	if e.cfg.Output != "-" {
		if fi, err := os.Stat(e.cfg.Output); err == nil {
			outputSize = fi.Size()
		}
	}

	return &CaptureSummary{
		OutputPath:    e.cfg.Output,
		OutputSize:    outputSize,
		RecordCount:   e.sink.RecordCount(),
		ResourceCount: len(e.index),
		Duration:      time.Since(start).Truncate(time.Second),
	}, nil
}

// pollResource polls a single resource kind at its configured interval until ctx is done.
func (e *Engine) pollResource(ctx context.Context, res config.Resource) {
	ticker := time.NewTicker(res.Interval)
	defer ticker.Stop()

	// Poll immediately, then on each tick.
	e.fetchResource(ctx, res)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.fetchResource(ctx, res)
		}
	}
}

// tableIndexKey is the virtual index key used to store Table-format responses
// alongside regular list responses. The sentinel "?as=Table" cannot appear in
// real API paths captured by the engine.
const tableIndexKeySuffix = "?as=Table"

// fetchResource issues one GET for res and stores the record. It also fetches
// the Table-format response so the mock server can replay rich column definitions.
func (e *Engine) fetchResource(ctx context.Context, res config.Resource) {
	namespaces := res.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}

	// Track whether every explicitly-namespaced fetch returned 404. If so, the
	// resource is likely cluster-scoped and the config has 'namespaces:' set by
	// mistake — warn and also capture the cluster-scoped path as a fallback.
	allNotFound := len(res.Namespaces) > 0
	for _, ns := range namespaces {
		apiPath := buildAPIPath(res.Group, res.Version, res.Resource, ns)
		_, code := e.doFetch(ctx, apiPath, "")
		if code != 0 && code != http.StatusNotFound {
			allNotFound = false
		}
		e.doFetch(ctx, apiPath, tableIndexKeySuffix)
	}

	if allNotFound {
		clusterPath := buildAPIPath(res.Group, res.Version, res.Resource, "")
		fmt.Fprintf(os.Stderr,
			"  [warn] %s: all namespace-scoped fetches returned 404 — "+
				"this is likely a cluster-scoped resource; remove 'namespaces:' "+
				"from its config entry. Fetching cluster-scoped path %s as fallback.\n",
			res.Resource, clusterPath)
		e.doFetch(ctx, clusterPath, "")
		e.doFetch(ctx, clusterPath, tableIndexKeySuffix)
	}
}

// fetchDiscovery captures the Kubernetes API discovery endpoints so the mock
// server can replay them with real resource lists rather than inferring them
// from the captured resource paths. Called once at the start of a capture run.
func (e *Engine) fetchDiscovery(ctx context.Context) {
	// Core discovery paths.
	e.doFetch(ctx, "/api", "")
	e.doFetch(ctx, "/api/v1", "")
	apisBody, _ := e.doFetch(ctx, "/apis", "")

	// OpenAPI specs for kubectl explain.
	e.doFetch(ctx, "/openapi/v2", "")
	openapiV3Body, _ := e.doFetch(ctx, "/openapi/v3", "")
	if openapiV3Body != nil {
		// Parse the v3 path index and fetch each per-group spec.
		var v3Index struct {
			Paths map[string]json.RawMessage `json:"paths"`
		}
		if err := json.Unmarshal(openapiV3Body, &v3Index); err == nil {
			for p := range v3Index.Paths {
				e.doFetch(ctx, "/openapi/v3/"+p, "")
			}
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
	for _, g := range groupList.Groups {
		for _, v := range g.Versions {
			e.doFetch(ctx, "/apis/"+v.GroupVersion, "")
		}
	}
}

// doFetch issues one GET for apiPath. When tableKeySuffix is non-empty the
// request uses a Table Accept header and the response is stored under
// apiPath+tableKeySuffix in the index. Returns the response body and HTTP
// status code, or (nil, 0) when the request could not be completed.
func (e *Engine) doFetch(ctx context.Context, apiPath, tableKeySuffix string) ([]byte, int) {
	url := e.baseURL + apiPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
			return nil, 0 // context cancelled, not a real error
		}
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] GET %s: %v\n", apiPath, err)
		}
		return nil, 0
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] read body %s: %v\n", apiPath, err)
		}
		return nil, 0
	}

	if tableKeySuffix == "" && resp.StatusCode == http.StatusForbidden {
		fmt.Fprintf(os.Stderr, "  [warn] RBAC denied: %s (check cluster permissions)\n", apiPath)
	}

	// Skip records with an empty body — storing json.RawMessage("") would
	// produce invalid JSON in the archive and corrupt serialisation.
	if len(body) == 0 {
		return nil, resp.StatusCode
	}

	if e.verbose {
		label := apiPath
		if tableKeySuffix != "" {
			label += tableKeySuffix
		}
		fmt.Fprintf(os.Stdout, "  [capture] %s -> %d\n", label, resp.StatusCode)
	}

	indexKey := apiPath + tableKeySuffix
	rec := &Record{
		ID:           uuid.New().String(),
		CapturedAt:   time.Now().UTC(),
		APIPath:      indexKey,
		HTTPMethod:   http.MethodGet,
		ResponseCode: resp.StatusCode,
		ResponseBody: json.RawMessage(body),
	}

	// Stream the record to the sink immediately — no in-memory buffer.
	if e.sink != nil {
		if err := e.sink.WriteRecord(rec); err != nil && e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] writing record %s: %v\n", indexKey, err)
		}
	}

	e.mu.Lock()
	if _, ok := e.index[indexKey]; !ok {
		e.index[indexKey] = &IndexEntry{APIPath: indexKey}
	}
	e.index[indexKey].RecordIDs = append(e.index[indexKey].RecordIDs, rec.ID)
	e.index[indexKey].Times = append(e.index[indexKey].Times, rec.CapturedAt)
	e.mu.Unlock()
	return body, resp.StatusCode
}

// fetchServerVersion attempts to retrieve the server version string.
// Returns safe defaults on failure.
func (e *Engine) fetchServerVersion(ctx context.Context) (version, address string) {
	address = e.baseURL
	url := e.baseURL + "/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "unknown", address
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return "unknown", address
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "unknown", address
	}
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.GitVersion == "" {
		return "unknown", address
	}
	return v.GitVersion, address
}

// buildAPIPath constructs the canonical REST path for a resource.
func buildAPIPath(group, version, resource, namespace string) string {
	var base string
	if group == "" {
		base = "/api/" + version
	} else {
		base = "/apis/" + group + "/" + version
	}
	if namespace == "" {
		return base + "/" + resource
	}
	return base + "/namespaces/" + namespace + "/" + resource
}

// fetchPodsLogs fetches the tail log for each pod found in res across all
// configured namespaces. Each log is stored under the clean
// /api/v1/namespaces/<ns>/pods/<name>/log path so the mock server can serve
// it verbatim when kubectl logs is run against the replay server.
func (e *Engine) fetchPodsLogs(ctx context.Context, res config.Resource) {
	namespaces := res.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}
	for _, ns := range namespaces {
		listPath := buildAPIPath(res.Group, res.Version, res.Resource, ns)
		listBody, code := e.doFetch(ctx, listPath, "")
		if code != 200 || listBody == nil {
			continue
		}
		var list struct {
			Items []struct {
				Metadata struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(listBody, &list); err != nil {
			continue
		}
		for _, item := range list.Items {
			podNS := item.Metadata.Namespace
			if podNS == "" {
				podNS = ns
			}
			e.fetchOnePodLog(ctx, podNS, item.Metadata.Name, res.Logs)
		}
	}
}

// fetchOnePodLog fetches the last tailLines lines of a pod's log and stores
// the plain-text content as a JSON-encoded string under the clean log path.
// Storing as a JSON string ensures the record is valid JSON for the archive.
func (e *Engine) fetchOnePodLog(ctx context.Context, namespace, podName string, tailLines int) {
	logPath := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", namespace, podName)
	fetchURL := fmt.Sprintf("%s%s?tailLines=%d", e.baseURL, logPath, tailLines)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}

	// Encode plain-text log body as a JSON string so it can be stored in the
	// JSON archive without breaking serialisation.
	jsonBody, err := json.Marshal(string(body))
	if err != nil {
		return
	}

	if e.verbose {
		fmt.Fprintf(os.Stdout, "  [capture] %s -> %d (%d bytes)\n", logPath, resp.StatusCode, len(body))
	}

	rec := &Record{
		ID:           uuid.New().String(),
		CapturedAt:   time.Now().UTC(),
		APIPath:      logPath,
		HTTPMethod:   http.MethodGet,
		ResponseCode: http.StatusOK,
		ResponseBody: json.RawMessage(jsonBody),
	}
	if e.sink != nil {
		if err := e.sink.WriteRecord(rec); err != nil && e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] writing log record %s: %v\n", logPath, err)
		}
	}
	e.mu.Lock()
	if _, ok := e.index[logPath]; !ok {
		e.index[logPath] = &IndexEntry{APIPath: logPath}
	}
	e.index[logPath].RecordIDs = append(e.index[logPath].RecordIDs, rec.ID)
	e.index[logPath].Times = append(e.index[logPath].Times, rec.CapturedAt)
	e.mu.Unlock()
}

// expandWildcardNamespaces replaces "*" in any resource's Namespaces list with
// the full list of namespaces discovered from the source cluster. If no
// resource mentions "*" the method is a no-op. Expansion happens once before
// polling begins; namespaces created during the capture are not included.
//
// Cluster-scoped resources with "*" emit a warning and fall back to a
// cluster-scoped (no namespace) fetch.
func (e *Engine) expandWildcardNamespaces(ctx context.Context) error {
	// Fast path: check whether any resource actually uses "*".
	needsExpansion := false
	for _, r := range e.cfg.Resources {
		for _, ns := range r.Namespaces {
			if ns == "*" {
				needsExpansion = true
				break
			}
		}
		if needsExpansion {
			break
		}
	}
	if !needsExpansion {
		return nil
	}

	// Fetch the namespace list from the cluster.
	nsBody, code := e.doFetch(ctx, "/api/v1/namespaces", "")
	if code != http.StatusOK || nsBody == nil {
		return fmt.Errorf("namespace discovery failed (HTTP %d): check cluster permissions", code)
	}
	var nsList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(nsBody, &nsList); err != nil {
		return fmt.Errorf("parsing namespace list: %w", err)
	}
	allNS := make([]string, 0, len(nsList.Items))
	for _, item := range nsList.Items {
		allNS = append(allNS, item.Metadata.Name)
	}

	// Expand each resource entry that contains "*".
	for i := range e.cfg.Resources {
		r := &e.cfg.Resources[i]
		hasWildcard := false
		for _, ns := range r.Namespaces {
			if ns == "*" {
				hasWildcard = true
				break
			}
		}
		if !hasWildcard {
			continue
		}

		if config.IsClusterScoped(r.Resource) {
			fmt.Fprintf(os.Stderr,
				"  [warn] %s: cluster-scoped resource with namespaces: [\"*\"] — ignoring namespaces\n",
				r.Resource)
			r.Namespaces = nil
			continue
		}

		// Build expanded list: explicit (non-wildcard) namespaces first, then
		// all discovered, deduplicated while preserving order.
		seen := make(map[string]bool)
		expanded := make([]string, 0, len(allNS))
		for _, ns := range r.Namespaces {
			if ns != "*" && !seen[ns] {
				seen[ns] = true
				expanded = append(expanded, ns)
			}
		}
		for _, ns := range allNS {
			if !seen[ns] {
				seen[ns] = true
				expanded = append(expanded, ns)
			}
		}
		r.Namespaces = expanded

		if e.verbose {
			fmt.Fprintf(os.Stdout,
				"  [info] %s: expanded '*' to %d namespaces: %s\n",
				r.Resource, len(expanded), strings.Join(expanded, ", "))
		}
	}
	return nil
}
