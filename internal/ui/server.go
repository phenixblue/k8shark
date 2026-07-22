// Package ui serves the k8shark web UI for a captured archive. It hosts the v2
// dashboard (internal/ui/v2) under /v2/ and redirects "/" there. The package
// owns the HTTP server lifecycle and replay-time selection; all page and API
// rendering lives in the v2 subpackage.
package ui

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/phenixblue/k8shark/internal/server"
	v2 "github.com/phenixblue/k8shark/internal/ui/v2"
)

// OpenOptions configures a UI server for a single capture archive.
type OpenOptions struct {
	// MockServer is the already-running mock API server for this archive
	// (`kshrk ui`/`kshrk replay --ui` always start one first). Its store is
	// reused directly — the UI no longer loads the archive a second time —
	// and, when writable, its overlay is merged into every v2 read so client
	// writes (kubectl/helm/kwok/controller-manager) are visible in the
	// dashboard too. Required.
	MockServer  *server.Server
	ArchivePath string
	Port        string
	At          string
	Verbose     bool
	// Clock, when non-nil, puts the dashboard in replay mode: views follow the
	// shared replay clock and the header exposes transport controls. The same
	// clock instance also drives the mock API server so kubectl and the UI stay
	// coherent.
	Clock *server.ReplayClock
}

// Server is a running UI HTTP server.
type Server struct {
	address    string
	httpServer *http.Server
	done       chan struct{}
	closeOnce  sync.Once
}

// Open mounts the v2 dashboard over MockServer's store (and overlay, when
// writable) and starts serving on the requested port (random when empty/"0").
// The archive itself is owned by MockServer — Shutdown/Wait here only stop
// this HTTP server, they don't touch the archive.
func Open(opts OpenOptions) (*Server, error) {
	if opts.MockServer == nil {
		return nil, fmt.Errorf("opening UI: MockServer is required")
	}
	store := opts.MockServer.Store()

	at, err := parseReplayAt(store.Metadata.CapturedAt, store.Metadata.CapturedUntil, opts.At)
	if err != nil {
		return nil, err
	}

	port := opts.Port
	if port == "" || port == "0" {
		port = "0"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		return nil, fmt.Errorf("listening: %w", err)
	}

	mux := http.NewServeMux()
	// The v2 dashboard is the UI: "/" redirects to /v2/.
	mux.HandleFunc("/", serveRoot)
	v2h := &v2.Handler{
		Store:       store,
		Overlay:     opts.MockServer,
		At:          at,
		ArchivePath: opts.ArchivePath,
		Verbose:     opts.Verbose,
		Clock:       opts.Clock,
	}
	v2h.Mount(mux)

	httpSrv := &http.Server{Handler: mux}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = httpSrv.Serve(ln)
	}()

	addr := fmt.Sprintf("http://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)
	return &Server{address: addr, httpServer: httpSrv, done: done}, nil
}

// Address returns the base URL the server is listening on.
func (s *Server) Address() string { return s.address }

// Shutdown gracefully stops the server.
func (s *Server) Shutdown() {
	s.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(ctx)
		<-s.done
	})
}

// Wait blocks until the server stops or an interrupt signal is received.
func (s *Server) Wait() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-s.done:
		return nil
	case <-sigCh:
	}
	s.Shutdown()
	return nil
}

// serveRoot redirects the bare root to the v2 dashboard and 404s anything else
// that falls through to the catch-all.
func serveRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/v2/", http.StatusFound)
		return
	}
	http.NotFound(w, r)
}

// parseReplayAt resolves the --at value (RFC3339 timestamp or relative duration
// like -5m) against the capture window. An empty value selects the latest state.
func parseReplayAt(start, end time.Time, raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		d, derr := time.ParseDuration(raw)
		if derr != nil {
			return time.Time{}, fmt.Errorf("parsing --at %q: must be RFC3339 or a relative duration like -5m", raw)
		}
		at = end.Add(d)
	}
	if !start.IsZero() && at.Before(start) {
		return time.Time{}, fmt.Errorf("parsing --at %q: requested time %s is before capture start %s", raw, at.Format(time.RFC3339), start.Format(time.RFC3339))
	}
	if !end.IsZero() && at.After(end) {
		return time.Time{}, fmt.Errorf("parsing --at %q: requested time %s is after capture end %s", raw, at.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	return at, nil
}
