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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
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
	PodLogs       PodLogSummary
}

// PodLogSummary describes the outcome of pod-log capture across every
// (pod, container) attempted during the run. Surfaced to the CLI so users see
// when logs were skipped (multi-container without log capture, RBAC denials,
// timeouts, terminated containers, etc.) without re-running in verbose mode.
type PodLogSummary struct {
	Attempted        int             // total (pod, container) fetch attempts (current logs only)
	Captured         int             // successful current-log captures
	Skipped          int             // current-log fetches that failed (non-OK / transport / timeout)
	CapturedPrevious int             // successful ?previous=true captures, when PreviousLogs is enabled
	Failures         []PodLogFailure // capped sample of current-log failures for CLI display
}

// PodLogFailure describes a single log fetch that did not produce a record.
type PodLogFailure struct {
	Namespace string
	Pod       string
	Container string
	Reason    string
}

// Engine orchestrates the capture loop.
// maxConcurrentFetches limits the number of goroutines simultaneously reading
// HTTP response bodies during a poll pass.  Bounding this caps peak in-flight
// memory (each body can be several MB for large list responses) regardless of
// how many resources are configured or auto-discovered.
const maxConcurrentFetches = 32

// perFetchTimeout caps any single list/discovery fetch (request + body read).
// Without it a stalled HTTP/2 stream can leave io.ReadAll blocked indefinitely
// while holding a fetchSem slot, starving every other fetch and stalling
// capture startup. The cap lets the fetch fail fast; polling retries on its
// next interval. Generous enough for large list responses on slow links.
const perFetchTimeout = 30 * time.Second

// Pod log fetch tuning.
const (
	// maxConcurrentLogFetches bounds the number of in-flight pod-log GETs.
	// Pod logs are fetched once at the end of the capture; on large clusters
	// (thousands of containers) parallelism is the difference between the
	// pass finishing in seconds vs. hitting a global timeout.
	maxConcurrentLogFetches = 16
	// perPodLogTimeout caps any single pod-log fetch so one hung container
	// can't stall the whole pass. Replaces the previous global 30s budget.
	perPodLogTimeout = 15 * time.Second
	// podLogFailureSampleLimit bounds how many failure entries the summary
	// holds for CLI display, so a million-container failure mode doesn't
	// balloon memory or output.
	podLogFailureSampleLimit = 20
)

type Engine struct {
	cfg            *config.Config
	verbose        bool
	httpClient     *http.Client
	dynClient      dynamic.Interface // client-go dynamic client used for watch streams
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
	fetchSem       chan struct{}   // semaphore bounding concurrent body reads
	fetchTimeout   time.Duration   // per-fetch cap (request + body read); 0 means perFetchTimeout
	// clusterListNamespacesSeen tracks, per wildcard-namespaced resource path,
	// the set of namespaces that have produced items in any prior cluster-wide
	// LIST response. Used by the demux so a namespace that empties out between
	// polls still gets an empty list written — otherwise the replay would
	// keep returning the prior non-empty body. Guarded by mu.
	clusterListNamespacesSeen map[string]map[string]bool
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

	// Watch streams go through client-go's dynamic client (which uses the proper
	// streaming watch decoder) rather than a hand-rolled HTTP request. It gets
	// its OWN transport/connection (NewForConfig, not the shared httpClient) so
	// long-lived watch streams don't contend with poll requests on a single
	// connection.
	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client: %w", err)
	}

	return &Engine{
		cfg:                       cfg,
		verbose:                   verbose,
		httpClient:                httpClient,
		dynClient:                 dynClient,
		baseURL:                   restCfg.Host,
		index:                     make(Index),
		watchIndex:                make(WatchIndex),
		discoveryCache:            make(map[string][]byte),
		lastHash:                  make(map[string][32]byte),
		warnedFallback:            make(map[string]bool),
		pathSeq:                   make(map[string]int),
		fetchSem:                  make(chan struct{}, maxConcurrentFetches),
		fetchTimeout:              perFetchTimeout,
		clusterListNamespacesSeen: make(map[string]map[string]bool),
	}, nil
}

// newEngineWith constructs an Engine with a pre-built HTTP client and base URL.
// Used in tests to inject a fake API server.
func newEngineWith(cfg *config.Config, client *http.Client, baseURL string, verbose bool) *Engine {
	// Build a dynamic client backed by the injected test client/server so watch
	// streams resolve against the fake API server. Errors are ignored because
	// the watch-streaming tests that need it construct against a valid baseURL;
	// non-watch tests never touch dynClient.
	dynClient, _ := dynamic.NewForConfigAndClient(&rest.Config{Host: baseURL}, client)
	return &Engine{
		cfg:                       cfg,
		verbose:                   verbose,
		httpClient:                client,
		dynClient:                 dynClient,
		baseURL:                   baseURL,
		index:                     make(Index),
		watchIndex:                make(WatchIndex),
		discoveryCache:            make(map[string][]byte),
		lastHash:                  make(map[string][32]byte),
		warnedFallback:            make(map[string]bool),
		pathSeq:                   make(map[string]int),
		fetchSem:                  make(chan struct{}, maxConcurrentFetches),
		fetchTimeout:              perFetchTimeout,
		clusterListNamespacesSeen: make(map[string]map[string]bool),
	}
}

// Run executes the capture and writes the output archive.
func (e *Engine) Run() (*CaptureSummary, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), e.cfg.Duration)
	defer cancel()

	// Install SIGTERM/SIGINT handler so the capture can be wound down gracefully:
	// the context is canceled, polling stops, and Finish() still writes a valid
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

	// Always record columns-only Table schemas for native kinds not otherwise
	// targeted, so the replay server renders kubectl-accurate columns for
	// overlay-created and untargeted objects. Runs concurrently with polling.
	wg.Add(1)
	go func() {
		defer wg.Done()
		e.captureCoreTableSchemas(ctx)
	}()

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
	// all polling so we capture the most recent log state. A fresh background
	// context is used because the main capture context has already expired;
	// each individual log fetch enforces its own perPodLogTimeout so one
	// hung container does not stall the whole pass.
	var podLogSummary PodLogSummary
	for _, res := range e.cfg.Resources {
		if res.All {
			continue
		}
		if res.Logs > 0 && res.Resource == "pods" {
			s := e.fetchPodsLogs(context.Background(), res)
			podLogSummary.Attempted += s.Attempted
			podLogSummary.Captured += s.Captured
			podLogSummary.Skipped += s.Skipped
			podLogSummary.CapturedPrevious += s.CapturedPrevious
			for _, f := range s.Failures {
				if len(podLogSummary.Failures) >= podLogFailureSampleLimit {
					break
				}
				podLogSummary.Failures = append(podLogSummary.Failures, f)
			}
		}
	}

	meta := &CaptureMetadata{
		FormatVersion:     CurrentFormatVersion,
		CaptureID:         uuid.New().String(),
		CapturedAt:        time.Now().UTC().Add(-e.cfg.Duration),
		CapturedUntil:     time.Now().UTC(),
		KubernetesVersion: kVersion,
		ServerAddress:     serverAddr,
		RecordCount:       e.sink.RecordCount(),
		DeduplicatedCount: e.dedupSkipped,
		AutoDiscovered:    e.cfg.AutoDiscover || hasAllDirective(e.cfg.Resources),
		WatchEnabled:      anyWatchEnabled(e.cfg.Resources),
		Intervals:         distinctIntervals(e.cfg.Resources),
		UncompressedBytes: e.sink.UncompressedBytes(),
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
		PodLogs:       podLogSummary,
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

// watchResource starts watch loops for the given resource. By default one
// watch loop runs per configured namespace, but when the resource was
// originally configured with a wildcard namespace ("*") a single cluster-wide
// watch stream is used instead. This matches how in-cluster controllers
// watch resources and avoids opening N streams against the API server on
// clusters with many namespaces. For cluster-scoped resources a single watch
// loop is used.
func (e *Engine) watchResource(ctx context.Context, res config.Resource) {
	if res.WildcardNamespaces {
		// One cluster-wide stream; streamWatch demuxes events back to per-
		// namespace API paths via metadata.namespace on each event object.
		e.watchResourcePath(ctx, res, "")
		return
	}

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

		if err := e.streamWatch(ctx, res, apiPath, namespace); err != nil && ctx.Err() == nil && e.verbose {
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

// streamWatch opens a single watch stream via client-go's dynamic client and
// records each ADDED/MODIFIED/DELETED event. Using the dynamic client (rather
// than a hand-rolled HTTP request + json.Decoder) means watch events are
// decoded by client-go's StreamWatcher, which negotiates the correct content
// type and reliably streams events over HTTP/2 — a raw request would connect
// but never receive any frames against some API servers.
//
// namespace is "" for a cluster-wide stream (cluster-scoped resources and the
// single stream used for wildcard-namespaced resources); otherwise it is the
// specific namespace to watch. Returning nil/err drives watchResourcePath's
// reconnect loop, which re-bootstraps a fresh resourceVersion before retrying.
func (e *Engine) streamWatch(ctx context.Context, res config.Resource, apiPath, namespace string) error {
	// Wildcard-namespace watches open a single cluster-wide stream but rewrite
	// each event's stored APIPath to /api/.../namespaces/<ns>/<resource> so the
	// replay server's per-namespace reconstruction logic continues to work
	// without changes.
	demuxPerNamespace := res.WildcardNamespaces && apiPath == buildAPIPath(res.Group, res.Version, res.Resource, "")

	gvr := schema.GroupVersionResource{Group: res.Group, Version: res.Version, Resource: res.Resource}

	var ri dynamic.ResourceInterface = e.dynClient.Resource(gvr)
	if namespace != "" {
		ri = e.dynClient.Resource(gvr).Namespace(namespace)
	}

	// Bootstrap a starting resourceVersion with a cheap one-item list so the
	// watch streams only changes from this point forward rather than replaying
	// every existing object as an ADDED event. The list metadata carries the
	// collection resourceVersion regardless of how many items are returned.
	opts := metav1.ListOptions{}
	if l, err := ri.List(ctx, metav1.ListOptions{Limit: 1}); err == nil {
		opts.ResourceVersion = l.GetResourceVersion()
	} else if ctx.Err() != nil {
		return nil
	}

	w, err := ri.Watch(ctx, opts)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer w.Stop()

	if e.verbose {
		fmt.Fprintf(os.Stdout, "  [watch] %s connected\n", apiPath)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-w.ResultChan():
			if !ok {
				// Server closed the stream; the caller re-lists and reconnects.
				return nil
			}

			var eventType string
			switch event.Type {
			case watch.Added:
				eventType = "ADDED"
			case watch.Modified:
				eventType = "MODIFIED"
			case watch.Deleted:
				eventType = "DELETED"
			case watch.Error:
				// Typically a 410 Expired Status. Log and let the channel close
				// so watchResourcePath re-lists for a fresh resourceVersion.
				if e.verbose {
					msg := "unknown watch error"
					if st, ok := event.Object.(*metav1.Status); ok && st.Message != "" {
						msg = st.Message
					}
					fmt.Fprintf(os.Stderr, "  [watch] %s: error event: %s\n", apiPath, msg)
				}
				continue
			default:
				// Bookmark and any other types are not transitions.
				continue
			}

			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			body, err := u.MarshalJSON()
			if err != nil {
				continue
			}

			recordPath := apiPath
			if demuxPerNamespace {
				ns := u.GetNamespace()
				if ns == "" {
					// Namespaced resources should always have metadata.namespace
					// set on watch events. Skip rather than store an unattributable
					// event under the cluster-wide path where the replay server
					// won't find it.
					continue
				}
				recordPath = buildAPIPath(res.Group, res.Version, res.Resource, ns)
			}

			rec := &Record{
				ID:           uuid.New().String(),
				CapturedAt:   time.Now().UTC(),
				APIPath:      recordPath,
				EventType:    eventType,
				HTTPMethod:   http.MethodGet,
				ResponseCode: http.StatusOK,
				ResponseBody: body,
			}

			if e.sink != nil {
				if err := e.sink.WriteRecord(rec); err != nil {
					if e.verbose {
						fmt.Fprintf(os.Stderr, "  [warn] writing watch record %s: %v\n", recordPath, err)
					}
					continue
				}
			}

			e.mu.Lock()
			if _, ok := e.watchIndex[recordPath]; !ok {
				e.watchIndex[recordPath] = &WatchIndexEntry{APIPath: recordPath}
			}
			seq := e.pathSeq[recordPath]
			e.pathSeq[recordPath] = seq + 1
			e.watchIndex[recordPath].Seqs = append(e.watchIndex[recordPath].Seqs, seq)
			e.watchIndex[recordPath].Times = append(e.watchIndex[recordPath].Times, rec.CapturedAt)
			e.watchIndex[recordPath].EventTypes = append(e.watchIndex[recordPath].EventTypes, rec.EventType)
			e.mu.Unlock()
		}
	}
}

// extractNamespaceFromObject reads metadata.namespace from a Kubernetes
// object body returned in a watch event. Returns "" if the body can't be
// parsed or the field is absent.
func extractNamespaceFromObject(obj []byte) string {
	var x struct {
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(obj, &x); err != nil {
		return ""
	}
	return x.Metadata.Namespace
}

// formatHTTPFailure builds a concise human-readable reason for a non-OK HTTP
// response. When the body is a Kubernetes Status JSON envelope (the typical
// API server error format), only the `message` field is shown — much more
// readable than the raw JSON, which is what users see for pod-log failures
// like "container X is terminated" or "container Y is waiting to start".
// Falls back to a truncated raw body when the response is not a Status object.
func formatHTTPFailure(statusCode int, body []byte) string {
	reason := fmt.Sprintf("HTTP %d", statusCode)
	if len(body) == 0 {
		return reason
	}
	if msg := extractStatusMessage(body); msg != "" {
		const maxMessageLen = 300
		if len(msg) > maxMessageLen {
			msg = msg[:maxMessageLen] + "..."
		}
		return reason + ": " + msg
	}
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > 160 {
		trimmed = trimmed[:160] + "..."
	}
	return reason + ": " + trimmed
}

// extractStatusMessage returns the `message` field of a Kubernetes Status
// JSON object, or "" if the body isn't a Status envelope.
func extractStatusMessage(body []byte) string {
	var s struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return ""
	}
	if s.Kind != "Status" {
		return ""
	}
	return s.Message
}

// tableIndexKey is the virtual index key used to store Table-format responses
// alongside regular list responses. The sentinel "?as=Table" cannot appear in
// real API paths captured by the engine.
const tableIndexKeySuffix = "?as=Table"

// tableSchemaIndexKeySuffix marks a columns-only Table record: a captured
// meta.k8s.io/v1 Table with its rows stripped, keeping only columnDefinitions.
// It is written by captureCoreTableSchemas for native kinds that aren't
// otherwise targeted, so the replay server can render kubectl-accurate columns
// for objects (overlay-created or untargeted) that have no full captured Table —
// without ever persisting those kinds' object data. The sentinel cannot appear
// in real API paths.
const tableSchemaIndexKeySuffix = "?as=TableSchema"

// fetchResource issues one GET for res and stores the record. It also fetches
// the Table-format response so the mock server can replay rich column definitions.
//
// For wildcard-namespaced resources (namespaces: ["*"]), polling is performed
// against the cluster-wide endpoint and the response is demuxed into
// per-namespace records — this catches "zombie" resources whose namespace has
// been deleted but which still exist in etcd. Such items are invisible to
// per-namespace polling because the deleted namespace isn't in
// /api/v1/namespaces. See fetchResourceClusterWide for details.
func (e *Engine) fetchResource(ctx context.Context, res config.Resource) {
	if res.WildcardNamespaces {
		e.fetchResourceClusterWide(ctx, res)
		return
	}

	namespaces := res.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}

	// Track whether every explicitly-namespaced fetch returned 404. If so, the
	// resource is likely cluster-scoped and the config has 'namespaces:' set by
	// mistake — warn and also capture the cluster-scoped path as a fallback.
	// Only a genuine 404 counts as evidence of cluster scope: a code of 0 means
	// the fetch failed transiently (timeout/transport/cancellation), which must
	// not be mistaken for "this resource isn't served per-namespace" or every
	// flaky fetch would trigger a spurious cluster-wide fallback.
	allNotFound := len(res.Namespaces) > 0
	dedupEnabled := res.DedupEnabled()
	for _, ns := range namespaces {
		apiPath := buildAPIPath(res.Group, res.Version, res.Resource, ns)
		_, code := e.doFetch(ctx, apiPath, "", dedupEnabled)
		if code != http.StatusNotFound {
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

// fetchResourceClusterWide polls a wildcard-namespaced resource via the
// cluster-wide endpoint /apis/<g>/<v>/<r> (or /api/v1/<r> for core) and
// demuxes the response into per-namespace records keyed by metadata.namespace
// on each item. Catches "zombie" resources whose namespace has been deleted
// — invisible to per-namespace polling because the deleted namespace isn't
// in /api/v1/namespaces, the source of wildcard expansion.
//
// A side effect of demuxing is one API call per cycle instead of N, which
// also reduces API server load on clusters with many namespaces.
func (e *Engine) fetchResourceClusterWide(ctx context.Context, res config.Resource) {
	clusterPath := buildAPIPath(res.Group, res.Version, res.Resource, "")
	dedupEnabled := res.DedupEnabled()

	if body, code := e.rawFetch(ctx, clusterPath, ""); body != nil {
		e.demuxClusterPlainList(res, clusterPath, body, code, dedupEnabled)
	}
	if body, code := e.rawFetch(ctx, clusterPath, tableIndexKeySuffix); body != nil {
		e.demuxClusterTableList(res, clusterPath, body, code, dedupEnabled)
	}
}

// demuxClusterPlainList parses a standard <Kind>List response, groups items
// by metadata.namespace, and writes a per-namespace list record for each.
func (e *Engine) demuxClusterPlainList(res config.Resource, clusterPath string, body []byte, statusCode int, dedupEnabled bool) {
	var env struct {
		APIVersion string            `json:"apiVersion,omitempty"`
		Kind       string            `json:"kind,omitempty"`
		Metadata   json.RawMessage   `json:"metadata,omitempty"`
		Items      []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] cluster-wide demux %s: parse error: %v\n", clusterPath, err)
		}
		return
	}

	groups := make(map[string][]json.RawMessage)
	for _, raw := range env.Items {
		ns := extractNamespaceFromObject(raw)
		if ns == "" {
			continue
		}
		groups[ns] = append(groups[ns], raw)
	}

	e.writeDemuxedListGroups(res, clusterPath, statusCode, dedupEnabled, false, groups,
		func(items []json.RawMessage) []byte {
			return marshalDemuxedList(env.APIVersion, env.Kind, env.Metadata, items)
		})
}

// demuxClusterTableList parses a meta.k8s.io/v1 Table response, groups rows
// by their embedded object's metadata.namespace, and writes a per-namespace
// Table record for each.
func (e *Engine) demuxClusterTableList(res config.Resource, clusterPath string, body []byte, statusCode int, dedupEnabled bool) {
	var env struct {
		APIVersion        string            `json:"apiVersion,omitempty"`
		Kind              string            `json:"kind,omitempty"`
		Metadata          json.RawMessage   `json:"metadata,omitempty"`
		ColumnDefinitions json.RawMessage   `json:"columnDefinitions,omitempty"`
		Rows              []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		if e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] cluster-wide table demux %s: parse error: %v\n", clusterPath, err)
		}
		return
	}

	groups := make(map[string][]json.RawMessage)
	for _, rowRaw := range env.Rows {
		var rowObj struct {
			Object json.RawMessage `json:"object"`
		}
		if err := json.Unmarshal(rowRaw, &rowObj); err != nil {
			continue
		}
		ns := extractNamespaceFromObject(rowObj.Object)
		if ns == "" {
			continue
		}
		groups[ns] = append(groups[ns], rowRaw)
	}

	e.writeDemuxedListGroups(res, clusterPath, statusCode, dedupEnabled, true, groups,
		func(rows []json.RawMessage) []byte {
			return marshalDemuxedTable(env.APIVersion, env.Kind, env.Metadata, env.ColumnDefinitions, rows)
		})
}

// writeDemuxedListGroups writes one record per namespace group, plus an
// empty-list record for any namespace that previously had items but has
// none in this cycle — so the replay's "latest" view converges with the
// source instead of returning stale items forever.
func (e *Engine) writeDemuxedListGroups(
	res config.Resource,
	clusterPath string,
	statusCode int,
	dedupEnabled bool,
	isTable bool,
	groups map[string][]json.RawMessage,
	buildBody func([]json.RawMessage) []byte,
) {
	cacheKey := clusterPath
	if isTable {
		cacheKey += tableIndexKeySuffix
	}

	e.mu.Lock()
	if e.clusterListNamespacesSeen == nil {
		e.clusterListNamespacesSeen = make(map[string]map[string]bool)
	}
	seenPrev := e.clusterListNamespacesSeen[cacheKey]
	if seenPrev == nil {
		seenPrev = make(map[string]bool)
		e.clusterListNamespacesSeen[cacheKey] = seenPrev
	}
	var emptied []string
	for ns := range groups {
		seenPrev[ns] = true
	}
	for ns := range seenPrev {
		if _, ok := groups[ns]; !ok {
			emptied = append(emptied, ns)
		}
	}
	e.mu.Unlock()

	for ns, items := range groups {
		indexKey := buildAPIPath(res.Group, res.Version, res.Resource, ns)
		if isTable {
			indexKey += tableIndexKeySuffix
		}
		e.storeRecord(indexKey, buildBody(items), statusCode, dedupEnabled)
	}
	for _, ns := range emptied {
		indexKey := buildAPIPath(res.Group, res.Version, res.Resource, ns)
		if isTable {
			indexKey += tableIndexKeySuffix
		}
		e.storeRecord(indexKey, buildBody(nil), statusCode, dedupEnabled)
	}
}

// marshalDemuxedList builds a <Kind>List JSON body containing only items.
// Empty items render as "items":[] so kubectl/clients see a valid empty list.
func marshalDemuxedList(apiVersion, kind string, metadata json.RawMessage, items []json.RawMessage) []byte {
	if items == nil {
		items = []json.RawMessage{}
	}
	out := map[string]any{"items": items}
	if apiVersion != "" {
		out["apiVersion"] = apiVersion
	}
	if kind != "" {
		out["kind"] = kind
	}
	if len(metadata) > 0 {
		out["metadata"] = metadata
	}
	body, _ := json.Marshal(out)
	return body
}

// marshalDemuxedTable builds a meta.k8s.io/v1 Table JSON body containing only rows.
func marshalDemuxedTable(apiVersion, kind string, metadata, columnDefs json.RawMessage, rows []json.RawMessage) []byte {
	if rows == nil {
		rows = []json.RawMessage{}
	}
	out := map[string]any{"rows": rows}
	if apiVersion != "" {
		out["apiVersion"] = apiVersion
	}
	if kind != "" {
		out["kind"] = kind
	}
	if len(metadata) > 0 {
		out["metadata"] = metadata
	}
	if len(columnDefs) > 0 {
		out["columnDefinitions"] = columnDefs
	}
	body, _ := json.Marshal(out)
	return body
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

// nativeAPIGroups are the built-in Kubernetes API groups whose Table column
// schemas captureCoreTableSchemas always records. CRD-backed groups are
// deliberately excluded: custom resources derive their columns from the CRD's
// additionalPrinterColumns at replay time, so a schema sweep would add cost
// without benefit (and could touch far more, unrelated groups). The empty
// string is the core group (/api/v1).
var nativeAPIGroups = map[string]bool{
	"":                             true,
	"apps":                         true,
	"batch":                        true,
	"autoscaling":                  true,
	"networking.k8s.io":            true,
	"policy":                       true,
	"rbac.authorization.k8s.io":    true,
	"storage.k8s.io":               true,
	"scheduling.k8s.io":            true,
	"node.k8s.io":                  true,
	"coordination.k8s.io":          true,
	"certificates.k8s.io":          true,
	"discovery.k8s.io":             true,
	"events.k8s.io":                true,
	"admissionregistration.k8s.io": true,
	"apiextensions.k8s.io":         true,
	"apiregistration.k8s.io":       true,
	"authentication.k8s.io":        true,
	"authorization.k8s.io":         true,
	"flowcontrol.apiserver.k8s.io": true,
}

// nativeSchemaKind is a list-capable native GVR discovered from the API server.
type nativeSchemaKind struct {
	group, version, resource string
}

// captureCoreTableSchemas records the Table columnDefinitions (rows stripped)
// for every list-capable native kind that isn't already being captured, so the
// replay server can render kubectl-accurate columns/`-o wide` for objects it
// has no full captured Table for (writable-overlay creations, or kinds/captures
// the user didn't target). It fetches one row (?limit=1) purely to obtain the
// server-side columnDefinitions, then discards the rows — so it never persists
// object data for kinds (e.g. Secrets) the user didn't ask to capture.
//
// Runs once; safe to run concurrently with polling (storeRecord is mutex-guarded).
func (e *Engine) captureCoreTableSchemas(ctx context.Context) {
	kinds := e.nativeListableKinds()
	if len(kinds) == 0 {
		return
	}

	const schemaWorkers = 8
	sem := make(chan struct{}, schemaWorkers)
	var wg sync.WaitGroup
	for _, k := range kinds {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(k nativeSchemaKind) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			e.fetchTableSchema(ctx, k)
		}(k)
	}
	wg.Wait()
}

// nativeListableKinds enumerates list-capable, non-subresource GVRs in the
// native API groups from the discovery documents cached by fetchDiscovery.
// Reads discoveryCache under the lock; callers run after fetchDiscovery has
// populated it.
//
// A GVR is skipped only when the capture already polls its cluster-scoped list
// path (a config entry with no explicit namespaces) — that yields a cluster-path
// ?as=Table already. GVRs captured only in specific namespaces (or via wildcard
// demux, which stores per-namespace Tables) have no cluster-path Table, so we
// still record a columns-only schema there — otherwise overlay-created objects
// in an uncaptured namespace, for a native kind without a built-in printer,
// would fall back to generic columns.
func (e *Engine) nativeListableKinds() []nativeSchemaKind {
	type gvr struct{ group, version, resource string }
	clusterCaptured := make(map[gvr]bool, len(e.cfg.Resources))
	for _, r := range e.cfg.Resources {
		if r.All {
			continue
		}
		if len(r.Namespaces) == 0 {
			clusterCaptured[gvr{r.Group, r.Version, r.Resource}] = true
		}
	}

	e.mu.Lock()
	coreBody := e.discoveryCache["/api/v1"]
	apisBody := e.discoveryCache["/apis"]
	e.mu.Unlock()

	var out []nativeSchemaKind
	add := func(group, version string, body []byte) {
		for _, r := range listableResources(body) {
			key := gvr{group, version, r}
			if clusterCaptured[key] {
				continue
			}
			out = append(out, nativeSchemaKind{group, version, r})
		}
	}

	// Core group.
	if nativeAPIGroups[""] {
		add("", "v1", coreBody)
	}

	// Native API groups.
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
	// Guard against a non-APIGroupList /apis cache (e.g. a Status object from a
	// transient discovery error), which would parse to zero groups and silently
	// skip schema capture for every non-core native group.
	if err := json.Unmarshal(apisBody, &groupList); err != nil || (groupList.Kind != "" && groupList.Kind != "APIGroupList") {
		if e.verbose {
			fmt.Fprintln(os.Stderr, "  [schema] /apis discovery unavailable or invalid; capturing core-group schemas only")
		}
	} else {
		for _, g := range groupList.Groups {
			if !nativeAPIGroups[g.Name] {
				continue
			}
			for _, gv := range g.Versions {
				e.mu.Lock()
				gvBody := e.discoveryCache["/apis/"+gv.GroupVersion]
				e.mu.Unlock()
				version := gv.Version
				if version == "" {
					if parts := strings.SplitN(gv.GroupVersion, "/", 2); len(parts) == 2 {
						version = parts[1]
					}
				}
				add(g.Name, version, gvBody)
			}
		}
	}
	return out
}

// listableResources parses an APIResourceList body and returns the names of
// resources that support `list` and are not sub-resources (no "/").
func listableResources(body []byte) []string {
	if len(body) == 0 {
		return nil
	}
	var rl struct {
		Kind      string `json:"kind"`
		Resources []struct {
			Name  string   `json:"name"`
			Verbs []string `json:"verbs"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &rl); err != nil || (rl.Kind != "" && rl.Kind != "APIResourceList") {
		return nil
	}
	var out []string
	for _, r := range rl.Resources {
		if strings.Contains(r.Name, "/") {
			continue
		}
		listable := false
		for _, v := range r.Verbs {
			if v == "list" {
				listable = true
				break
			}
		}
		if listable {
			out = append(out, r.Name)
		}
	}
	return out
}

// fetchTableSchema fetches a single-row Table for a kind and stores a
// columns-only record (rows stripped) under the schema index key. RBAC denials
// and non-Table responses are skipped silently.
func (e *Engine) fetchTableSchema(ctx context.Context, k nativeSchemaKind) {
	clusterPath := buildAPIPath(k.group, k.version, k.resource, "")
	// ?limit=1 minimizes data transfer; we only want the columnDefinitions.
	body, code := e.rawFetch(ctx, clusterPath+"?limit=1", tableIndexKeySuffix)
	if body == nil || code != http.StatusOK {
		return
	}
	schema, ok := stripTableRows(body)
	if !ok {
		return // not a Table (kind doesn't support server-side printing)
	}
	e.storeRecord(clusterPath+tableSchemaIndexKeySuffix, schema, code, false)
	if e.verbose {
		fmt.Fprintf(os.Stdout, "  [schema] %s%s\n", clusterPath, tableSchemaIndexKeySuffix)
	}
}

// stripTableRows returns a meta.k8s.io/v1 Table body carrying only the
// columnDefinitions from the input Table (rows removed). ok is false when the
// input isn't a Table with column definitions.
func stripTableRows(body []byte) ([]byte, bool) {
	var t struct {
		APIVersion        string          `json:"apiVersion"`
		Kind              string          `json:"kind"`
		ColumnDefinitions json.RawMessage `json:"columnDefinitions"`
	}
	// Require an actual Table (not just any payload that happens to carry a
	// columnDefinitions field), so we never persist a bogus schema record.
	if err := json.Unmarshal(body, &t); err != nil || t.Kind != "Table" || len(t.ColumnDefinitions) == 0 {
		return nil, false
	}
	out, err := json.Marshal(map[string]any{
		"apiVersion":        firstNonEmpty(t.APIVersion, "meta.k8s.io/v1"),
		"kind":              firstNonEmpty(t.Kind, "Table"),
		"metadata":          map[string]any{"resourceVersion": "0"},
		"columnDefinitions": t.ColumnDefinitions,
		"rows":              []any{},
	})
	if err != nil {
		return nil, false
	}
	return out, true
}

func hasAllDirective(resources []config.Resource) bool {
	for _, r := range resources {
		if r.All {
			return true
		}
	}
	return false
}

// anyWatchEnabled reports whether any configured resource requested a watch.
func anyWatchEnabled(resources []config.Resource) bool {
	for _, r := range resources {
		if r.Watch {
			return true
		}
	}
	return false
}

// distinctIntervals collects the unique human-readable poll intervals across
// configured resources, for display in the capture-details panel.
func distinctIntervals(resources []config.Resource) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range resources {
		if r.IntervalRaw == "" || seen[r.IntervalRaw] {
			continue
		}
		seen[r.IntervalRaw] = true
		out = append(out, r.IntervalRaw)
	}
	return out
}

func (e *Engine) validateWatchConcurrency() error {
	watchStreams := 0
	for _, r := range e.cfg.Resources {
		if !r.Watch || r.All {
			continue
		}
		if r.WildcardNamespaces {
			// Wildcard-namespace watches collapse to a single cluster-wide stream
			// regardless of how many namespaces the wildcard expanded to.
			watchStreams++
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

	if e.verbose {
		label := apiPath
		if tableKeySuffix != "" {
			label += tableKeySuffix
		}
		fmt.Fprintf(os.Stdout, "  [capture] %s -> %d\n", label, resp.StatusCode)
	}
	return body, resp.StatusCode
}

// storeRecord persists body at indexKey with optional dedup. Used both by the
// per-resource doFetch path and by the cluster-wide demux path, which
// constructs synthetic per-namespace bodies and writes them under per-
// namespace index keys.
func (e *Engine) storeRecord(indexKey string, body []byte, statusCode int, dedupEnabled bool) {
	if len(body) == 0 {
		return
	}
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
			return
		}
		e.lastHash[indexKey] = h
		e.mu.Unlock()
	}

	rec := &Record{
		ID:           uuid.New().String(),
		CapturedAt:   time.Now().UTC(),
		APIPath:      indexKey,
		HTTPMethod:   http.MethodGet,
		ResponseCode: statusCode,
		ResponseBody: json.RawMessage(body),
	}
	if e.sink != nil {
		if err := e.sink.WriteRecord(rec); err != nil && e.verbose {
			fmt.Fprintf(os.Stderr, "  [warn] writing record %s: %v\n", indexKey, err)
		}
	}
	itemCount := countListItems(body)
	e.mu.Lock()
	if _, ok := e.index[indexKey]; !ok {
		e.index[indexKey] = &IndexEntry{APIPath: indexKey}
	}
	seq := e.pathSeq[indexKey]
	e.pathSeq[indexKey] = seq + 1
	e.index[indexKey].Seqs = append(e.index[indexKey].Seqs, seq)
	e.index[indexKey].Times = append(e.index[indexKey].Times, rec.CapturedAt)
	e.index[indexKey].Counts = append(e.index[indexKey].Counts, itemCount)
	e.mu.Unlock()
}

// countListItems peeks at a JSON response body and returns the number of
// top-level items in it. Recognizes both standard <Kind>List responses
// (items[]) and meta.k8s.io/v1 Table responses (rows[]). Returns 0 for
// non-list bodies (single objects, discovery, OpenAPI). Used to populate
// IndexEntry.Counts so the UI can show namespace card counts without
// loading each record body.
func countListItems(body []byte) int {
	var probe struct {
		Items []json.RawMessage `json:"items"`
		Rows  []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0
	}
	if probe.Items != nil {
		return len(probe.Items)
	}
	return len(probe.Rows)
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

// fetchPodsLogs fetches the tail log for each container of each pod found in
// res across all configured namespaces. Each log is stored under
// /api/v1/namespaces/<ns>/pods/<name>/log?container=<c> so the replay server
// can route both bare `kubectl logs <pod>` (single-container) and
// `kubectl logs <pod> -c <c>` requests to the right record.
//
// Fetches run concurrently bounded by maxConcurrentLogFetches, and each fetch
// has its own perPodLogTimeout so one hung container does not stall the pass.
// Per-fetch failures (HTTP errors, RBAC, timeouts, terminated containers) are
// recorded in the returned PodLogSummary so the CLI can show users what was
// skipped without re-running in verbose mode.
func (e *Engine) fetchPodsLogs(ctx context.Context, res config.Resource) PodLogSummary {
	type job struct {
		namespace string
		pod       string
		container string
		previous  bool
	}

	namespaces := res.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}

	var jobs []job
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
				Spec struct {
					Containers []struct {
						Name string `json:"name"`
					} `json:"containers"`
					InitContainers []struct {
						Name string `json:"name"`
					} `json:"initContainers"`
				} `json:"spec"`
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
			queue := func(container string) {
				jobs = append(jobs, job{namespace: podNS, pod: item.Metadata.Name, container: container})
				if res.PreviousLogs {
					jobs = append(jobs, job{namespace: podNS, pod: item.Metadata.Name, container: container, previous: true})
				}
			}
			// Init containers first — they ran before main containers and
			// often carry the most actionable diagnostic output on Pending
			// or CrashLoopBackOff pods.
			for _, c := range item.Spec.InitContainers {
				queue(c.Name)
			}
			for _, c := range item.Spec.Containers {
				queue(c.Name)
			}
		}
	}

	if len(jobs) == 0 {
		return PodLogSummary{}
	}

	// Only current-log fetches count toward the user-facing summary so it
	// stays focused on the headline number ("X/Y captured"). Previous-log
	// fetches are best-effort: successes grow CapturedPrevious, but failures
	// (the very common "container has not been previously terminated" 400)
	// are dropped silently to avoid swamping the failure sample with noise.
	attempted := 0
	for _, j := range jobs {
		if !j.previous {
			attempted++
		}
	}

	var (
		mu      sync.Mutex
		summary = PodLogSummary{Attempted: attempted}
		wg      sync.WaitGroup
	)
	sem := make(chan struct{}, maxConcurrentLogFetches)

	for _, j := range jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()
			result := e.fetchOnePodLog(ctx, j.namespace, j.pod, j.container, res.Logs, j.previous)

			mu.Lock()
			defer mu.Unlock()
			if j.previous {
				if result.captured {
					summary.CapturedPrevious++
				}
				return
			}
			if result.captured {
				summary.Captured++
				return
			}
			summary.Skipped++
			if result.failure != nil && len(summary.Failures) < podLogFailureSampleLimit {
				summary.Failures = append(summary.Failures, *result.failure)
			}
		}(j)
	}
	wg.Wait()
	return summary
}

type logFetchResult struct {
	captured bool
	failure  *PodLogFailure // nil when captured is true
}

// fetchOnePodLog fetches the last tailLines of one container's log and stores
// the plain-text content as a JSON-encoded string under
// /api/v1/namespaces/<ns>/pods/<name>/log?container=<c> (or, when previous is
// true, …&previous=true). The query-suffix index key lets the replay server
// route `kubectl logs <pod> -c <c> [--previous]` to the matching record.
func (e *Engine) fetchOnePodLog(ctx context.Context, namespace, podName, containerName string, tailLines int, previous bool) logFetchResult {
	fetchCtx, cancel := context.WithTimeout(ctx, perPodLogTimeout)
	defer cancel()

	logPath := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log", namespace, podName)
	indexKey := logPath + "?container=" + containerName
	fetchURL := fmt.Sprintf("%s%s?container=%s&tailLines=%d", e.baseURL, logPath, url.QueryEscape(containerName), tailLines)
	if previous {
		indexKey += "&previous=true"
		fetchURL += "&previous=true"
	}

	failure := func(reason string) logFetchResult {
		if e.verbose {
			marker := ""
			if previous {
				marker = ",previous=true"
			}
			fmt.Fprintf(os.Stderr, "  [warn] log %s/%s [container=%s%s]: %s\n", namespace, podName, containerName, marker, reason)
		}
		return logFetchResult{failure: &PodLogFailure{
			Namespace: namespace,
			Pod:       podName,
			Container: containerName,
			Reason:    reason,
		}}
	}

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return failure(fmt.Sprintf("build request: %v", err))
	}
	resp, err := e.httpClient.Do(req)
	if err != nil {
		if fetchCtx.Err() != nil {
			return failure("timeout after " + perPodLogTimeout.String())
		}
		return failure(fmt.Sprintf("request error: %v", err))
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return failure(formatHTTPFailure(resp.StatusCode, body))
	}
	if readErr != nil {
		return failure(fmt.Sprintf("reading body: %v", readErr))
	}

	jsonBody, err := json.Marshal(string(body))
	if err != nil {
		return failure(fmt.Sprintf("encoding body: %v", err))
	}

	if e.verbose {
		fmt.Fprintf(os.Stdout, "  [capture] %s -> %d (%d bytes)\n", indexKey, resp.StatusCode, len(body))
	}

	rec := &Record{
		ID:           uuid.New().String(),
		CapturedAt:   time.Now().UTC(),
		APIPath:      indexKey,
		HTTPMethod:   http.MethodGet,
		ResponseCode: http.StatusOK,
		ResponseBody: json.RawMessage(jsonBody),
	}
	if e.sink != nil {
		if err := e.sink.WriteRecord(rec); err != nil {
			return failure(fmt.Sprintf("writing record: %v", err))
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

	return logFetchResult{captured: true}
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

	// Fetch the namespace list from the cluster, following pagination tokens so
	// that clusters with more than 500 namespaces are fully enumerated.
	// (Kubernetes defaults to a page size of 500 when no ?limit= is specified.)
	var allNS []string
	continueToken := ""
	for {
		path := "/api/v1/namespaces?limit=500"
		if continueToken != "" {
			path += "&continue=" + continueToken
		}
		nsBody, code := e.doFetch(ctx, path, "", true)
		if code != http.StatusOK || nsBody == nil {
			if code == 0 {
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("namespace discovery failed: request canceled before completion (try a longer --duration): %w", err)
				}
				return fmt.Errorf("namespace discovery failed (HTTP 0): request could not be completed; check kubeconfig/context and cluster connectivity")
			}
			if code == http.StatusForbidden {
				return fmt.Errorf("namespace discovery failed (HTTP %d): check cluster permissions", code)
			}
			return fmt.Errorf("namespace discovery failed (HTTP %d): unable to list namespaces", code)
		}
		var nsList struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(nsBody, &nsList); err != nil {
			return fmt.Errorf("parsing namespace list: %w", err)
		}
		for _, item := range nsList.Items {
			allNS = append(allNS, item.Metadata.Name)
		}
		continueToken = nsList.Metadata.Continue
		if continueToken == "" {
			break
		}
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
		// Remember that this entry was wildcarded so watchResource can open a
		// single cluster-wide watch stream instead of one per expanded namespace.
		r.WildcardNamespaces = true

		if e.verbose {
			fmt.Fprintf(os.Stdout,
				"  [info] %s: expanded '*' to %d namespaces: %s\n",
				r.Resource, len(expanded), strings.Join(expanded, ", "))
		}
	}
	return nil
}
