package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// OpenOptions holds parameters for opening a capture archive.
type OpenOptions struct {
	ArchivePath   string
	Port          string
	KubeconfigOut string
	At            string
	Verbose       bool
}

// ReplayOptions holds parameters for replaying a capture forward through time.
type ReplayOptions struct {
	ArchivePath   string
	Port          string
	KubeconfigOut string
	Speed         string // speed factor, e.g. "2x", "0.5x" (empty = real time)
	From          string // window start: RFC3339 or relative like -10m (empty = capture start)
	To            string // window end: RFC3339 or relative like -1m (empty = capture end)
	Loop          bool
	StartPaused   bool
	// PauseAtWindowEnd, when StartPaused is set, parks the clock at the window
	// end (the most complete captured state) instead of the window start. The
	// Web UI sets this: a capture's opening moments are typically sparse
	// (informers are still completing their initial LIST), so a dashboard
	// paused at the window start until the user presses Play would otherwise
	// show a near-empty cluster by default. Headless `kshrk replay
	// --start-paused` leaves this unset — its documented behavior is "press
	// Enter to begin playback" from the window start.
	PauseAtWindowEnd bool
	Writable         bool // accept client writes into an in-memory overlay
	// DisableScheduling turns off the pod-scheduling shim (which otherwise binds
	// an unscheduled Pod to a node on create). Zero value keeps the shim on under
	// --writable; set it to opt out (--schedule-pods=false).
	DisableScheduling bool
	Verbose           bool
}

// Server represents a running mock API server.
type Server struct {
	address           string
	kubeconfigPath    string
	certPEM           []byte // this run's self-signed TLS cert, for callers that need to pin it
	ar                *archive.Archive
	httpServer        *http.Server
	done              chan struct{}
	clock             *ReplayClock // non-nil in replay mode
	writable          bool         // overlay enabled
	hasWatch          bool         // capture contains watch events
	kubernetesVersion string       // capture's /version gitVersion, e.g. "v1.36.1"
}

// Open opens a capture archive, starts the mock HTTPS server, and writes
// a kubeconfig pointing at it.
func Open(opts OpenOptions) (*Server, error) {
	ar, err := archive.Open(opts.ArchivePath)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	store, err := LoadStore(ar)
	if err != nil {
		_ = ar.Close()
		return nil, fmt.Errorf("loading capture: %w", err)
	}
	at, err := parseReplayAt(store.Metadata, opts.At)
	if err != nil {
		_ = ar.Close()
		return nil, err
	}
	return serve(ar, store, at, nil, false, false, opts.Port, opts.KubeconfigOut, opts.Verbose)
}

// Replay opens a capture and starts the mock HTTPS server in replay mode: a
// clock advances through the [from, to] window at the given speed, streaming
// captured watch events over time. LIST/GET return state as-of the clock.
func Replay(opts ReplayOptions) (*Server, error) {
	ar, err := archive.Open(opts.ArchivePath)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	store, err := LoadStore(ar)
	if err != nil {
		_ = ar.Close()
		return nil, fmt.Errorf("loading capture: %w", err)
	}
	speed, err := parseSpeed(opts.Speed)
	if err != nil {
		_ = ar.Close()
		return nil, err
	}
	from, to, err := parseReplayWindow(store.Metadata, opts.From, opts.To)
	if err != nil {
		_ = ar.Close()
		return nil, err
	}
	clock := NewReplayClock(from, to, speed, opts.Loop, opts.StartPaused)
	if opts.StartPaused && opts.PauseAtWindowEnd {
		clock.ParkAtWindowEnd()
	}
	return serve(ar, store, time.Time{}, clock, opts.Writable, !opts.DisableScheduling, opts.Port, opts.KubeconfigOut, opts.Verbose)
}

// serve brings up the shared TLS listener, kubeconfig, and HTTP server. When
// clock is non-nil the handler runs in replay mode; when writable is set (replay
// only) client writes land in an in-memory overlay.
func serve(ar *archive.Archive, store *CaptureStore, at time.Time, clock *ReplayClock, writable, schedulePods bool, port, kubeconfigOut string, verbose bool) (*Server, error) {
	certPEM, keyPEM, err := generateSelfSignedCert()
	if err != nil {
		_ = ar.Close()
		return nil, fmt.Errorf("generating TLS cert: %w", err)
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		_ = ar.Close()
		return nil, fmt.Errorf("loading TLS cert: %w", err)
	}

	if port == "" || port == "0" {
		port = "0"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		_ = ar.Close()
		return nil, fmt.Errorf("listening: %w", err)
	}

	addr := fmt.Sprintf("https://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)

	kubeconfigPath := kubeconfigOut
	if kubeconfigPath == "" {
		home, _ := os.UserHomeDir()
		kubeconfigPath = filepath.Join(home, ".kube", "k8shark-"+store.Metadata.CaptureID+".yaml")
	}
	if err := writeKubeconfig(addr, kubeconfigPath); err != nil {
		_ = ar.Close()
		_ = ln.Close()
		return nil, fmt.Errorf("writing kubeconfig: %w", err)
	}

	tlsListener := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	})

	h := newHandler(store, at, verbose)
	h.clock = clock
	if writable && clock != nil { // writable is a replay-only feature
		h.overlay = newOverlay()
		h.schedulePods = schedulePods // bind unscheduled pods to a node (KWOK integration, #160)
		if h.schedulePods {
			// Eager node synthesis: give KWOK (and `kubectl get nodes`) a node to
			// manage before any Pod is created, when the capture has none.
			h.ensureSchedulableNode()
		}
	}
	httpSrv := &http.Server{Handler: h}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = httpSrv.Serve(tlsListener)
	}()

	return &Server{
		address:           addr,
		kubeconfigPath:    kubeconfigPath,
		certPEM:           certPEM,
		ar:                ar,
		httpServer:        httpSrv,
		done:              done,
		clock:             clock,
		writable:          h.overlay != nil,
		hasWatch:          len(store.WatchIndex) > 0,
		kubernetesVersion: store.Metadata.KubernetesVersion,
	}, nil
}

// Writable reports whether the server accepts client writes into the overlay.
func (s *Server) Writable() bool { return s.writable }

// Address returns the HTTPS listen address (e.g. "https://127.0.0.1:54321").
func (s *Server) Address() string { return s.address }

// KubeconfigPath returns the path to the generated kubeconfig.
func (s *Server) KubeconfigPath() string { return s.kubeconfigPath }

// CertPEM returns a copy of this run's self-signed TLS certificate in PEM
// form, so an in-process client (one that isn't going through the generated
// kubeconfig's insecure-skip-tls-verify) can pin it instead of disabling
// verification. A copy is returned so a caller can't mutate the server's
// stored cert bytes.
func (s *Server) CertPEM() []byte {
	out := make([]byte, len(s.certPEM))
	copy(out, s.certPEM)
	return out
}

// Clock returns the replay clock, or nil when the server is not in replay mode.
func (s *Server) Clock() *ReplayClock { return s.clock }

// KubernetesVersion returns the capture's Kubernetes gitVersion (e.g.
// "v1.36.1"), as reported by the captured cluster's /version endpoint.
func (s *Server) KubernetesVersion() string { return s.kubernetesVersion }

// HasWatchEvents reports whether the capture contains watch events to stream.
// Poll-only captures return false; replay still serves LIST/GET as-of the clock
// but emits no watch events (inferring events for poll-only captures is a later
// phase).
func (s *Server) HasWatchEvents() bool { return s.hasWatch }

// Shutdown immediately stops the server and closes the archive.
// Useful in tests and programmatic usage; Wait() is preferred for CLI use.
func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
	<-s.done
	if s.ar != nil {
		_ = s.ar.Close()
	}
}

// Wait blocks until Ctrl+C / SIGTERM, then shuts the server down and closes the archive.
func (s *Server) Wait() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-s.done:
	case <-sigCh:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(ctx)
		<-s.done
	}
	if s.ar != nil {
		_ = s.ar.Close()
	}
	return nil
}

func parseReplayAt(meta capture.CaptureMetadata, raw string) (time.Time, error) {
	return parseReplayTime(meta, raw, "--at")
}

// parseReplayTime parses an RFC3339 timestamp or a duration relative to the
// capture end (e.g. -5m), validating it lies within the capture window. flag is
// the CLI flag name used in error messages so callers report the right flag.
func parseReplayTime(meta capture.CaptureMetadata, raw, flag string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}

	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		d, derr := time.ParseDuration(raw)
		if derr != nil {
			return time.Time{}, fmt.Errorf("parsing %s %q: must be RFC3339 or a relative duration like -5m", flag, raw)
		}
		// Relative durations are anchored to the capture end; without one we'd
		// silently resolve against year 0001, so require an absolute time instead.
		if meta.CapturedUntil.IsZero() {
			return time.Time{}, fmt.Errorf("parsing %s %q: capture end time is unknown; use an absolute RFC3339 time", flag, raw)
		}
		at = meta.CapturedUntil.Add(d)
	}

	if !meta.CapturedAt.IsZero() && at.Before(meta.CapturedAt) {
		return time.Time{}, fmt.Errorf("parsing %s %q: requested replay time %s is before capture start %s", flag, raw, at.Format(time.RFC3339), meta.CapturedAt.Format(time.RFC3339))
	}
	if !meta.CapturedUntil.IsZero() && at.After(meta.CapturedUntil) {
		return time.Time{}, fmt.Errorf("parsing %s %q: requested replay time %s is after capture end %s", flag, raw, at.Format(time.RFC3339), meta.CapturedUntil.Format(time.RFC3339))
	}

	return at, nil
}

// parseReplayWindow resolves the [from, to] replay window. Empty from/to default
// to the capture bounds; non-empty values are RFC3339 timestamps or durations
// relative to the capture end (e.g. -10m), validated to lie within the capture.
func parseReplayWindow(meta capture.CaptureMetadata, fromRaw, toRaw string) (from, to time.Time, err error) {
	from = meta.CapturedAt
	to = meta.CapturedUntil

	if fromRaw != "" {
		from, err = parseReplayTime(meta, fromRaw, "--from")
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if toRaw != "" {
		to, err = parseReplayTime(meta, toRaw, "--to")
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	// If the capture metadata lacks bounds (absent/corrupt) and the caller didn't
	// supply them, report that clearly instead of comparing zero times.
	if from.IsZero() {
		return time.Time{}, time.Time{}, fmt.Errorf("capture start time is unknown; specify an explicit --from")
	}
	if to.IsZero() {
		return time.Time{}, time.Time{}, fmt.Errorf("capture end time is unknown; specify an explicit --to")
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("--to %s must be after --from %s", to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	return from, to, nil
}
