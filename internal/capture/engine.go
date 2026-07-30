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

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/config"
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

// podLogFailureSampleLimit bounds how many failure entries the summary
// holds for CLI display, so a million-container failure mode doesn't
// balloon memory or output.
const podLogFailureSampleLimit = 20

type Engine struct {
	cfg        *config.Config
	verbose    bool
	httpClient *http.Client
	dynClient  dynamic.Interface // client-go dynamic client used for watch streams
	baseURL    string
	mu         sync.Mutex
	index      Index
	watchIndex WatchIndex
	sink       archive.RecordSink // set by Run(); exposed for tests
	// recipients, when non-empty, makes Run() write the output archive as an
	// age-encrypted envelope. Set via SetEncryption before Run(). Ignored when
	// output is "-" (NDJSON streaming to stdout is never encrypted).
	recipients []age.Recipient
	// pollPasses, when non-zero, makes pollResource fetch exactly this many
	// times back-to-back instead of waiting on a real time.Ticker paced by
	// res.Interval and bounded by the capture context's timeout (e.cfg.Duration).
	// Set by benchmarks so Run() finishes in the time actual fetches take
	// rather than blocking for the wall-clock capture window, keeping the
	// number of samples per run deterministic.
	pollPasses     int
	discoveryCache map[string][]byte // bodies saved by fetchDiscovery for autoDiscoverResources
	lastHash       map[string][32]byte
	dedupSkipped   int
	warnedFallback map[string]bool // dedup set for allNotFound cluster-scoped fallback warnings
	fetchSem       chan struct{}   // semaphore bounding concurrent body reads
	fetchTimeout   time.Duration   // per-fetch cap (request + body read); 0 means perFetchTimeout
	// captureErr is set by storeRecord on the first record-write failure and
	// surfaced by Run() so the process exits non-zero instead of silently
	// producing an archive with a dangling index reference. Guarded by mu.
	captureErr error
	// clusterListNamespacesSeen tracks, per wildcard-namespaced resource path,
	// the set of namespaces that have produced items in any prior cluster-wide
	// LIST response. Used by the demux so a namespace that empties out between
	// polls still gets an empty list written — otherwise the replay would
	// keep returning the prior non-empty body. Guarded by mu.
	clusterListNamespacesSeen map[string]map[string]bool
}

const maxConcurrentWatchStreams = 256

// newEngineBase builds an Engine from already-constructed HTTP/dynamic
// clients, initializing every other field. NewEngine and newEngineWith each
// build their clients differently but must agree on everything else — this
// is the one place that does, so adding a field here can't be forgotten in
// one constructor and only done in the other (#238).
func newEngineBase(cfg *config.Config, verbose bool, httpClient *http.Client, dynClient dynamic.Interface, baseURL string) *Engine {
	return &Engine{
		cfg:                       cfg,
		verbose:                   verbose,
		httpClient:                httpClient,
		dynClient:                 dynClient,
		baseURL:                   baseURL,
		index:                     make(Index),
		watchIndex:                make(WatchIndex),
		discoveryCache:            make(map[string][]byte),
		lastHash:                  make(map[string][32]byte),
		warnedFallback:            make(map[string]bool),
		fetchSem:                  make(chan struct{}, maxConcurrentFetches),
		fetchTimeout:              perFetchTimeout,
		clusterListNamespacesSeen: make(map[string]map[string]bool),
	}
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

	// Watch streams go through client-go's dynamic client (which uses the proper
	// streaming watch decoder) rather than a hand-rolled HTTP request. It gets
	// its OWN transport/connection (NewForConfig, not the shared httpClient) so
	// long-lived watch streams don't contend with poll requests on a single
	// connection.
	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client: %w", err)
	}

	return newEngineBase(cfg, verbose, httpClient, dynClient, restCfg.Host), nil
}

// SetEncryption makes Run() write the output archive as an age-encrypted
// envelope to the given recipients. It must be called before Run(). Passing
// no recipients leaves the archive unencrypted. Has no effect when output is
// "-" (NDJSON streaming to stdout is never encrypted).
func (e *Engine) SetEncryption(recipients []age.Recipient) {
	e.recipients = recipients
}

// newEngineWith constructs an Engine with a pre-built HTTP client and base URL.
// Used in tests to inject a fake API server.
func newEngineWith(cfg *config.Config, client *http.Client, baseURL string, verbose bool) *Engine {
	// Build a dynamic client backed by the injected test client/server so watch
	// streams resolve against the fake API server. Errors are ignored because
	// the watch-streaming tests that need it construct against a valid baseURL;
	// non-watch tests never touch dynClient.
	dynClient, _ := dynamic.NewForConfigAndClient(&rest.Config{Host: baseURL}, client)
	return newEngineBase(cfg, verbose, client, dynClient, baseURL)
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
			var sw *archive.StreamWriter
			if len(e.recipients) > 0 {
				sw, err = archive.NewEncryptedStreamWriter(e.cfg.Output, e.recipients)
			} else {
				sw, err = archive.NewStreamWriter(e.cfg.Output)
			}
			if err != nil {
				return nil, err
			}
			e.sink = sw
			// Release the writer's file handle on any early-return error path
			// between here and Finish (e.g. namespace expansion or watch
			// validation below). Abort is a no-op once Finish has run, so the
			// success path is unaffected.
			defer func() { _ = sw.Abort() }()
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

	// pollStart is recorded right before the first poll/watch goroutines launch,
	// so it can stand in for CapturedAt below: preflight, discovery, and
	// namespace expansion above can themselves take non-trivial wall-clock time
	// (a full CRD discovery pass in particular), and backdating CapturedAt from
	// e.cfg.Duration ignored all of it — understating the true first-poll time
	// by that same amount. A replay session's default window start (from ≈
	// CapturedAt) could then land before any resource's first captured
	// snapshot, so a client querying immediately at replay start (e.g. `replay
	// --with-kwok`'s auto-launched kwok subprocess) would see a spurious "not
	// found in capture" for a resource that genuinely was captured.
	pollStart := time.Now().UTC()

	var wg sync.WaitGroup

	// Record columns-only Table schemas for native kinds whose cluster-scoped
	// list path isn't already captured as a full ?as=Table, so the replay server
	// renders kubectl-accurate columns for overlay-created and untargeted
	// objects. Runs concurrently with polling.
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
		CapturedAt:        pollStart,
		CapturedUntil:     time.Now().UTC(),
		KubernetesVersion: kVersion,
		ServerAddress:     serverAddr,
		RecordCount:       e.sink.RecordCount(),
		DeduplicatedCount: e.dedupSkipped,
		AutoDiscovered:    e.cfg.AutoDiscover || hasAllDirective(e.cfg.Resources),
		WatchEnabled:      anyWatchEnabled(e.cfg.Resources),
		Intervals:         distinctIntervals(e.cfg.Resources),
		UncompressedBytes: e.sink.UncompressedBytes(),
		// Mirror the sink-selection condition: recipients are ignored for
		// "-" (NDJSON to stdout), so the archive is only actually encrypted
		// when writing to a file.
		Encrypted: len(e.recipients) > 0 && e.cfg.Output != "-",
	}

	if e.verbose {
		// When records stream to stdout as NDJSON, keep stdout pure and send
		// this diagnostic to stderr instead.
		w := os.Stdout
		if e.cfg.Output == "-" {
			w = os.Stderr
		}
		fmt.Fprintf(w, "  captured %d records\n", e.sink.RecordCount())
	}

	if err := e.sink.Finish(meta, e.index, e.watchIndex); err != nil {
		return nil, err
	}

	// A record-write failure doesn't abort the capture — everything else that
	// succeeded is still worth keeping in the archive — but it must not exit
	// 0: the archive is missing data the user asked for and, absent this,
	// would have no way of knowing.
	e.mu.Lock()
	captureErr := e.captureErr
	e.mu.Unlock()
	if captureErr != nil {
		return nil, captureErr
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

// tableIndexKeySuffix is the virtual index key used to store Table-format responses
// alongside regular list responses. The sentinel "?as=Table" cannot appear in
// real API paths captured by the engine.
const tableIndexKeySuffix = "?as=Table"

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
