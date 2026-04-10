package capture

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/config"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// CaptureSummary holds statistics about a completed capture run.
type CaptureSummary struct {
	OutputPath    string
	OutputSize    int64
	RecordCount   int
	ResourceCount int        // distinct API paths captured
	Duration      time.Duration
}

// Engine orchestrates the capture loop.
type Engine struct {
	cfg        *config.Config
	verbose    bool
	httpClient *http.Client
	baseURL    string
	mu         sync.Mutex
	records    []*Record
	index      Index
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

	meta := &CaptureMetadata{
		CaptureID:         uuid.New().String(),
		CapturedAt:        time.Now().UTC().Add(-e.cfg.Duration),
		CapturedUntil:     time.Now().UTC(),
		KubernetesVersion: kVersion,
		ServerAddress:     serverAddr,
		RecordCount:       len(e.records),
	}

	if e.verbose {
		fmt.Fprintf(os.Stdout, "  captured %d records\n", len(e.records))
	}

	if err := archive.Write(e.cfg.Output, meta, e.records, e.index); err != nil {
		return nil, err
	}

	var outputSize int64
	if fi, err := os.Stat(e.cfg.Output); err == nil {
		outputSize = fi.Size()
	}

	return &CaptureSummary{
		OutputPath:    e.cfg.Output,
		OutputSize:    outputSize,
		RecordCount:   len(e.records),
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

	for _, ns := range namespaces {
		apiPath := buildAPIPath(res.Group, res.Version, res.Resource, ns)
		e.doFetch(ctx, apiPath, "")
		e.doFetch(ctx, apiPath, tableIndexKeySuffix)
	}
}

// fetchDiscovery captures the Kubernetes API discovery endpoints so the mock
// server can replay them with real resource lists rather than inferring them
// from the captured resource paths. Called once at the start of a capture run.
func (e *Engine) fetchDiscovery(ctx context.Context) {
	// Core discovery paths.
	e.doFetch(ctx, "/api", "")
	e.doFetch(ctx, "/api/v1", "")
	apisBody := e.doFetch(ctx, "/apis", "")

	// OpenAPI specs for kubectl explain.
	e.doFetch(ctx, "/openapi/v2", "")
	openapiV3Body := e.doFetch(ctx, "/openapi/v3", "")
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
// apiPath+tableKeySuffix in the index. Returns the response body.
func (e *Engine) doFetch(ctx context.Context, apiPath, tableKeySuffix string) []byte {
	url := e.baseURL + apiPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] build request %s: %v\n", apiPath, err)
		}
		return nil
	}

	if tableKeySuffix != "" {
		req.Header.Set("Accept", "application/json;as=Table;g=meta.k8s.io;v=v1")
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
			if ctx.Err() != nil {
				return nil // context cancelled, not a real error
			}
			if e.verbose {
				fmt.Fprintf(os.Stderr, "  [warn] GET %s: %v\n", apiPath, err)
			}
			return nil
		}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] read body %s: %v\n", apiPath, err)
		}
		return nil
	}

	if tableKeySuffix == "" && resp.StatusCode == http.StatusForbidden {
		fmt.Fprintf(os.Stderr, "  [warn] RBAC denied: %s (check cluster permissions)\n", apiPath)
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

	e.mu.Lock()
	e.records = append(e.records, rec)
	if _, ok := e.index[indexKey]; !ok {
		e.index[indexKey] = &IndexEntry{APIPath: indexKey}
	}
	e.index[indexKey].RecordIDs = append(e.index[indexKey].RecordIDs, rec.ID)
	e.index[indexKey].Times = append(e.index[indexKey].Times, rec.CapturedAt)
	e.mu.Unlock()
	return body
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

// statusNotFound returns true for 404 API errors (resource not present on server).
func statusNotFound(err error) bool {
	return k8serrors.IsNotFound(err)
}
