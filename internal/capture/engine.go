package capture

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	cfg            *config.Config
	verbose        bool
	httpClient     *http.Client
	baseURL        string
	mu             sync.Mutex
	index          Index
	watchIndex     WatchIndex
	sink           archive.RecordSink // set by Run(); exposed for tests
	discoveryCache map[string][]byte  // bodies saved by fetchDiscovery for autoDiscoverResources
	lastHash       map[string][32]byte
	dedupSkipped   int
	warnedFallback map[string]bool // dedup set for allNotFound cluster-scoped fallback warnings
	pathSeq        map[string]int  // per-path record sequence counter (matches archive on-disk seq)
}

const maxConcurrentWatchStreams = 256

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
		cfg:            cfg,
		verbose:        verbose,
		httpClient:     httpClient,
		baseURL:        restCfg.Host,
		index:          make(Index),
		watchIndex:     make(WatchIndex),
		discoveryCache: make(map[string][]byte),
		lastHash:       make(map[string][32]byte),
		warnedFallback: make(map[string]bool),
		pathSeq:        make(map[string]int),
	}, nil
}

// newEngineWith constructs an Engine with a pre-built HTTP client and base URL.
// Used in tests to inject a fake API server.
func newEngineWith(cfg *config.Config, client *http.Client, baseURL string, verbose bool) *Engine {
	return &Engine{
		cfg:            cfg,
		verbose:        verbose,
		httpClient:     client,
		baseURL:        baseURL,
		index:          make(Index),
		watchIndex:     make(WatchIndex),
		discoveryCache: make(map[string][]byte),
		lastHash:       make(map[string][32]byte),
		warnedFallback: make(map[string]bool),
		pathSeq:        make(map[string]int),
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

	if err := e.preflight(ctx); err != nil {
		return nil, err
	}

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

	// Collect server version for metadata.
	kVersion, serverAddr := e.fetchServerVersion(ctx)

	// Capture API discovery endpoints so the mock server can replay them faithfully.
	e.fetchDiscovery(ctx)

	// Auto-discover CRD-backed and non-core resources from /apis when
	// explicitly requested or when all=true directives are present.
	if e.cfg.AutoDiscover || hasAllDirective(e.cfg.Resources) {
		e.autoDiscoverResources(ctx)
	}

	// Expand wildcard namespaces before polling begins. This must happen after
	// auto-discovery because all=true directives add namespaced resources with
	// Namespaces=["*"] by default.
	if err := e.expandWildcardNamespaces(ctx); err != nil {
		return nil, err
	}

	if err := e.validateWatchConcurrency(); err != nil {
		return nil, err
	}

	var wg sync.WaitGroup
	for _, res := range e.cfg.Resources {
		if res.All {
			continue
		}
		wg.Add(1)
		go func(r config.Resource) {
			defer wg.Done()
			e.pollResource(ctx, r)
		}(res)
		if res.Watch {
			wg.Add(1)
			go func(r config.Resource) {
				defer wg.Done()
				e.watchResource(ctx, r)
			}(res)
		}
	}
	wg.Wait()

	// Fetch pod logs for any pods resource entry with logs > 0. This runs after
	// all polling so we capture the most recent log state. A short background
	// context is used because the main capture context has already expired.
	for _, res := range e.cfg.Resources {
		if res.All {
			continue
		}
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
		DeduplicatedCount: e.dedupSkipped,
	}

	if e.verbose {
		fmt.Fprintf(os.Stdout, "  captured %d records\n", e.sink.RecordCount())
	}

	if err := e.sink.Finish(meta, e.index, e.watchIndex); err != nil {
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

// preflight validates that the configured kubeconfig/context can reach the
// target API server before any archive writer is initialized.
func (e *Engine) preflight(ctx context.Context) error {
	timeout := 5 * time.Second
	if e.cfg != nil && e.cfg.Duration > 0 && e.cfg.Duration < timeout {
		timeout = e.cfg.Duration
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, e.baseURL+"/version", nil)
	if err != nil {
		return fmt.Errorf("capture preflight failed (kubeconfig=%s, server=%s): building version request: %w", kubeconfigLabel(e.cfg), e.baseURL, err)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("capture preflight failed (kubeconfig=%s, server=%s): %w", kubeconfigLabel(e.cfg), e.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			detail = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("capture preflight failed (kubeconfig=%s, server=%s): GET /version returned %d: %s", kubeconfigLabel(e.cfg), e.baseURL, resp.StatusCode, detail)
	}

	return nil
}

func kubeconfigLabel(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.Kubeconfig) != "" {
		return cfg.Kubeconfig
	}
	return "default"
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

// watchResource starts one watch loop per configured namespace for the given
// resource. For cluster-scoped resources, a single watch loop is used.
func (e *Engine) watchResource(ctx context.Context, res config.Resource) {
	namespaces := res.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}

	var wg sync.WaitGroup
	for _, ns := range namespaces {
		wg.Add(1)
		go func(namespace string) {
			defer wg.Done()
			e.watchResourcePath(ctx, res, namespace)
		}(ns)
	}
	wg.Wait()
}

func (e *Engine) watchResourcePath(ctx context.Context, res config.Resource, namespace string) {
	apiPath := buildAPIPath(res.Group, res.Version, res.Resource, namespace)

	for {
		if ctx.Err() != nil {
			return
		}

		resourceVersion := ""
		if body, code := e.doFetch(ctx, apiPath, "", res.DedupEnabled()); code == http.StatusOK && body != nil {
			resourceVersion = extractResourceVersion(body)
		}

		if err := e.streamWatch(ctx, apiPath, resourceVersion); err != nil && ctx.Err() == nil && e.verbose {
			fmt.Fprintf(os.Stderr, "  [watch] %s: %v\n", apiPath, err)
		}

		if ctx.Err() != nil {
			return
		}

		// Brief backoff before reconnecting after a disconnect/error.
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (e *Engine) streamWatch(ctx context.Context, apiPath, resourceVersion string) error {
	q := url.Values{}
	q.Set("watch", "1")
	if resourceVersion != "" {
		q.Set("resourceVersion", resourceVersion)
	}

	watchURL := e.baseURL + apiPath + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, watchURL, nil)
	if err != nil {
		return err
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("watch status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	if e.verbose {
		fmt.Fprintf(os.Stdout, "  [watch] %s connected\n", apiPath)
	}

	dec := json.NewDecoder(resp.Body)
	for {
		var event struct {
			Type   string          `json:"type"`
			Object json.RawMessage `json:"object"`
		}
		if err := dec.Decode(&event); err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			return err
		}

		switch event.Type {
		case "ADDED", "MODIFIED", "DELETED":
			// Keep these event types.
		default:
			continue
		}
		if len(event.Object) == 0 {
			continue
		}

		rec := &Record{
			ID:           uuid.New().String(),
			CapturedAt:   time.Now().UTC(),
			APIPath:      apiPath,
			EventType:    event.Type,
			HTTPMethod:   http.MethodGet,
			ResponseCode: http.StatusOK,
			ResponseBody: event.Object,
		}

		if e.sink != nil {
			if err := e.sink.WriteRecord(rec); err != nil {
				if e.verbose {
					fmt.Fprintf(os.Stderr, "  [warn] writing watch record %s: %v\n", apiPath, err)
				}
				continue
			}
		}

		e.mu.Lock()
		if _, ok := e.watchIndex[apiPath]; !ok {
			e.watchIndex[apiPath] = &WatchIndexEntry{APIPath: apiPath}
		}
		seq := e.pathSeq[apiPath]
		e.pathSeq[apiPath] = seq + 1
		e.watchIndex[apiPath].Seqs = append(e.watchIndex[apiPath].Seqs, seq)
		e.watchIndex[apiPath].Times = append(e.watchIndex[apiPath].Times, rec.CapturedAt)
		e.watchIndex[apiPath].EventTypes = append(e.watchIndex[apiPath].EventTypes, rec.EventType)
		e.mu.Unlock()
	}
}

func extractResourceVersion(body []byte) string {
	var meta struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return ""
	}
	return meta.Metadata.ResourceVersion
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
	dedupEnabled := res.DedupEnabled()
	for _, ns := range namespaces {
		apiPath := buildAPIPath(res.Group, res.Version, res.Resource, ns)
		_, code := e.doFetch(ctx, apiPath, "", dedupEnabled)
		if code != 0 && code != http.StatusNotFound {
			allNotFound = false
		}
		e.doFetch(ctx, apiPath, tableIndexKeySuffix, dedupEnabled)
	}

	if allNotFound {
		clusterPath := buildAPIPath(res.Group, res.Version, res.Resource, "")
		// For auto-discovered resources the namespace assignment came from the
		// Kubernetes discovery API, not from user config. Some resources
		// (especially OpenShift CRDs) report "namespaced" in discovery but only
		// serve data at the cluster-scoped path. Silently fall back rather than
		// printing a misleading "remove 'namespaces:'" hint the user can't act on.
		if !res.AutoDiscovered && e.verbose {
			// Deduplicate: only warn once per unique cluster-scoped path per run.
			e.mu.Lock()
			alreadyWarned := e.warnedFallback[clusterPath]
			if !alreadyWarned {
				e.warnedFallback[clusterPath] = true
			}
			e.mu.Unlock()
			if !alreadyWarned {
				fmt.Fprintf(os.Stderr,
					"  [warn] %s: all namespace-scoped fetches returned 404 — "+
						"this is likely a cluster-scoped resource; remove 'namespaces:' "+
						"from its config entry. Fetching cluster-scoped path %s as fallback.\n",
					res.Resource, clusterPath)
			}
		} else if res.AutoDiscovered && e.verbose {
			e.mu.Lock()
			alreadyWarned := e.warnedFallback[clusterPath]
			if !alreadyWarned {
				e.warnedFallback[clusterPath] = true
			}
			e.mu.Unlock()
			if !alreadyWarned {
				fmt.Fprintf(os.Stderr,
					"  [debug] %s: all namespace-scoped fetches returned 404; falling back to cluster-scoped path %s\n",
					res.Resource, clusterPath)
			}
		}
		e.doFetch(ctx, clusterPath, "", dedupEnabled)
		e.doFetch(ctx, clusterPath, tableIndexKeySuffix, dedupEnabled)
	}
}

// defaultAutoDiscoverExcludeGroups are API groups that produce no useful
// captures (internal machinery, aggregated metrics, etc.) and are always
// excluded during auto-discovery regardless of user config.
var defaultAutoDiscoverExcludeGroups = map[string]bool{
	"metrics.k8s.io":         true,
	"apiregistration.k8s.io": true,
	"authentication.k8s.io":  true,
	"authorization.k8s.io":   true,
}

// autoDiscoverResources reads the already-cached /apis discovery documents
// from e.discoveryCache (populated earlier in the same run by fetchDiscovery)
// and appends one config.Resource entry per group-version-resource tuple that
// is not already covered by an explicit config entry. The appended entries are
// then picked up by the standard poll loop in Run().
func (e *Engine) autoDiscoverResources(ctx context.Context) {
	// Build the exclude set: defaults + user overrides.
	exclude := make(map[string]bool, len(defaultAutoDiscoverExcludeGroups))
	for g := range defaultAutoDiscoverExcludeGroups {
		exclude[g] = true
	}
	for _, g := range e.cfg.AutoDiscoverExcludeGroups {
		exclude[g] = true
	}

	// Build a set of already-configured (group, version, resource) triples so
	// we don't add duplicates.
	type gvr struct{ group, version, resource string }
	configured := make(map[gvr]bool, len(e.cfg.Resources))
	directives := make([]config.Resource, 0)
	for _, r := range e.cfg.Resources {
		if r.All {
			directives = append(directives, r)
			continue
		}
		configured[gvr{r.Group, r.Version, r.Resource}] = true
	}
	if len(directives) == 0 && e.cfg.AutoDiscover {
		directives = append(directives, config.Resource{All: true, IntervalRaw: "30s", Interval: 30 * time.Second})
	}
	discoverCore := len(directives) > 0

	apisBody := e.discoveryCache["/apis"]
	if apisBody == nil {
		// Resilience: retry /apis once if initial discovery capture missed it
		// (e.g. transient API error). Without this, whole API groups can be
		// silently skipped from auto-discovery.
		if fetched, code := e.doFetch(ctx, "/apis", "", true); code == http.StatusOK && fetched != nil {
			apisBody = fetched
			e.discoveryCache["/apis"] = fetched
		} else {
			if e.verbose {
				fmt.Fprintln(os.Stderr, "  [auto-discover] /apis not in discovery cache; skipping")
			}
			return
		}
	}

	var groupList struct {
		Kind   string `json:"kind"`
		Groups []struct {
			Name     string `json:"name"`
			Versions []struct {
				GroupVersion string `json:"groupVersion"`
				Version      string `json:"version"`
			} `json:"versions"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(apisBody, &groupList); err != nil || (groupList.Kind != "" && groupList.Kind != "APIGroupList") {
		if fetched, code := e.doFetch(ctx, "/apis", "", true); code == http.StatusOK && fetched != nil {
			apisBody = fetched
			e.discoveryCache["/apis"] = fetched
			if err := json.Unmarshal(apisBody, &groupList); err != nil || (groupList.Kind != "" && groupList.Kind != "APIGroupList") {
				return
			}
		} else {
			return
		}
	}

	added := 0
	for _, g := range groupList.Groups {
		if exclude[g.Name] {
			continue
		}
		for _, gv := range g.Versions {
			gvPath := "/apis/" + gv.GroupVersion
			gvBody := e.discoveryCache[gvPath]
			if gvBody == nil {
				// Resilience: retry this group-version once if missing from cache.
				if fetched, code := e.doFetch(ctx, gvPath, "", true); code == http.StatusOK && fetched != nil {
					gvBody = fetched
					e.discoveryCache[gvPath] = fetched
				} else {
					continue
				}
			}

			var resList struct {
				Kind      string `json:"kind"`
				Resources []struct {
					Name       string `json:"name"`
					Namespaced bool   `json:"namespaced"`
				} `json:"resources"`
			}
			if err := json.Unmarshal(gvBody, &resList); err != nil || (resList.Kind != "" && resList.Kind != "APIResourceList") {
				if fetched, code := e.doFetch(ctx, gvPath, "", true); code == http.StatusOK && fetched != nil {
					gvBody = fetched
					e.discoveryCache[gvPath] = fetched
					if err := json.Unmarshal(gvBody, &resList); err != nil || (resList.Kind != "" && resList.Kind != "APIResourceList") {
						continue
					}
				} else {
					continue
				}
			}

			parts := strings.SplitN(gv.GroupVersion, "/", 2)
			group := parts[0]
			version := gv.Version
			if len(parts) == 2 {
				version = parts[1]
			}

			for _, res := range resList.Resources {
				// Skip sub-resources (contain a slash, e.g. "pods/status").
				if strings.Contains(res.Name, "/") {
					continue
				}
				key := gvr{group, version, res.Name}
				if configured[key] {
					continue
				}

				for _, d := range directives {
					if d.Scope == "cluster" && res.Namespaced {
						continue
					}
					if d.Scope == "namespaced" && !res.Namespaced {
						continue
					}

					newRes := config.Resource{
						Group:          group,
						Version:        version,
						Resource:       res.Name,
						IntervalRaw:    d.IntervalRaw,
						Interval:       d.Interval,
						Dedup:          d.Dedup,
						Watch:          d.Watch,
						AutoDiscovered: true,
					}
					if newRes.Interval == 0 {
						newRes.Interval = 30 * time.Second
						newRes.IntervalRaw = "30s"
					}
					if res.Namespaced {
						if len(d.Namespaces) > 0 {
							newRes.Namespaces = append([]string(nil), d.Namespaces...)
						} else {
							newRes.Namespaces = []string{"*"}
						}
					}

					e.cfg.Resources = append(e.cfg.Resources, newRes)
					configured[key] = true
					added++
					if e.verbose {
						fmt.Fprintf(os.Stdout, "  [auto-discover] added %s/%s/%s (namespaced=%v, scope=%s)\n",
							group, version, res.Name, res.Namespaced, firstNonEmpty(d.Scope, "all"))
					}
					break
				}
			}
		}
	}

	// all=true directives represent "capture all", which includes core/v1
	// resources like pods, services, configmaps, and nodes.
	if discoverCore {
		coreBody := e.discoveryCache["/api/v1"]
		if coreBody == nil {
			if fetched, code := e.doFetch(ctx, "/api/v1", "", true); code == http.StatusOK && fetched != nil {
				coreBody = fetched
				e.discoveryCache["/api/v1"] = fetched
			}
		}
		if coreBody != nil {
			var coreList struct {
				Kind      string `json:"kind"`
				Resources []struct {
					Name       string `json:"name"`
					Namespaced bool   `json:"namespaced"`
				} `json:"resources"`
			}
			if err := json.Unmarshal(coreBody, &coreList); err == nil && (coreList.Kind == "" || coreList.Kind == "APIResourceList") {
				for _, res := range coreList.Resources {
					if strings.Contains(res.Name, "/") {
						continue
					}
					key := gvr{"", "v1", res.Name}
					if configured[key] {
						continue
					}

					for _, d := range directives {
						if d.Scope == "cluster" && res.Namespaced {
							continue
						}
						if d.Scope == "namespaced" && !res.Namespaced {
							continue
						}

						newRes := config.Resource{
							Group:          "",
							Version:        "v1",
							Resource:       res.Name,
							IntervalRaw:    d.IntervalRaw,
							Interval:       d.Interval,
							Dedup:          d.Dedup,
							Watch:          d.Watch,
							AutoDiscovered: true,
						}
						if newRes.Interval == 0 {
							newRes.Interval = 30 * time.Second
							newRes.IntervalRaw = "30s"
						}
						if res.Namespaced {
							if len(d.Namespaces) > 0 {
								newRes.Namespaces = append([]string(nil), d.Namespaces...)
							} else {
								newRes.Namespaces = []string{"*"}
							}
						}

						e.cfg.Resources = append(e.cfg.Resources, newRes)
						configured[key] = true
						added++
						if e.verbose {
							fmt.Fprintf(os.Stdout, "  [auto-discover] added %s/%s/%s (namespaced=%v, scope=%s)\n",
								"core", "v1", res.Name, res.Namespaced, firstNonEmpty(d.Scope, "all"))
						}
						break
					}
				}
			}
		}
	}

	if e.verbose {
		fmt.Fprintf(os.Stdout, "  [auto-discover] added %d resource types\n", added)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func hasAllDirective(resources []config.Resource) bool {
	for _, r := range resources {
		if r.All {
			return true
		}
	}
	return false
}

func (e *Engine) validateWatchConcurrency() error {
	watchStreams := 0
	for _, r := range e.cfg.Resources {
		if !r.Watch || r.All {
			continue
		}
		if len(r.Namespaces) == 0 {
			watchStreams++
			continue
		}
		watchStreams += len(r.Namespaces)
	}

	if watchStreams > maxConcurrentWatchStreams {
		return fmt.Errorf(
			"capture config expands to %d concurrent watch streams (max %d); reduce watch usage, narrow namespaces, or avoid all=true with watch=true",
			watchStreams,
			maxConcurrentWatchStreams,
		)
	}
	return nil
}

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
		// Parse the v3 path index and fetch each per-group spec.
		var v3Index struct {
			Paths map[string]json.RawMessage `json:"paths"`
		}
		if err := json.Unmarshal(openapiV3Body, &v3Index); err == nil {
			for p := range v3Index.Paths {
				e.doFetch(ctx, "/openapi/v3/"+p, "", true)
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
			gvPath := "/apis/" + v.GroupVersion
			gvBody, _ := e.doFetch(ctx, gvPath, "", true)
			if gvBody != nil {
				e.discoveryCache[gvPath] = gvBody
			}
		}
	}
}

// doFetch issues one GET for apiPath. When tableKeySuffix is non-empty the
// request uses a Table Accept header and the response is stored under
// apiPath+tableKeySuffix in the index. Returns the response body and HTTP
// status code, or (nil, 0) when the request could not be completed.
func (e *Engine) doFetch(ctx context.Context, apiPath, tableKeySuffix string, dedupEnabled bool) ([]byte, int) {
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
	if dedupEnabled {
		h := sha256.Sum256(body)
		e.mu.Lock()
		prev, ok := e.lastHash[indexKey]
		if ok && prev == h {
			e.dedupSkipped++
			e.mu.Unlock()
			if e.verbose {
				fmt.Fprintf(os.Stdout, "  [dedup] %s unchanged; skipping write\n", indexKey)
			}
			return body, resp.StatusCode
		}
		e.lastHash[indexKey] = h
		e.mu.Unlock()
	}

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
	seq := e.pathSeq[indexKey]
	e.pathSeq[indexKey] = seq + 1
	e.index[indexKey].Seqs = append(e.index[indexKey].Seqs, seq)
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
		listBody, code := e.doFetch(ctx, listPath, "", res.DedupEnabled())
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
	logSeq := e.pathSeq[logPath]
	e.pathSeq[logPath] = logSeq + 1
	e.index[logPath].Seqs = append(e.index[logPath].Seqs, logSeq)
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
	nsBody, code := e.doFetch(ctx, "/api/v1/namespaces", "", true)
	if code != http.StatusOK || nsBody == nil {
		if code == 0 {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("namespace discovery failed: request cancelled before completion (try a longer --duration): %w", err)
			}
			return fmt.Errorf("namespace discovery failed (HTTP 0): request could not be completed; check kubeconfig/context and cluster connectivity")
		}
		if code == http.StatusForbidden {
			return fmt.Errorf("namespace discovery failed (HTTP %d): check cluster permissions", code)
		}
		return fmt.Errorf("namespace discovery failed (HTTP %d): unable to list namespaces", code)
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
