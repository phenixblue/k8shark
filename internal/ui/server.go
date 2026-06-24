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
	"syscall"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/server"
	v2 "github.com/phenixblue/k8shark/internal/ui/v2"
)

// OpenOptions configures a UI server for a single capture archive.
type OpenOptions struct {
	ArchivePath string
	Port        string
	At          string
	Verbose     bool
}

// Server is a running UI HTTP server.
type Server struct {
	address    string
	httpServer *http.Server
	done       chan struct{}
}

// Open loads the archive, mounts the v2 dashboard, and starts serving on the
// requested port (random when empty/"0"). The archive is kept open for the life
// of the server so the store can read records on demand.
func Open(opts OpenOptions) (*Server, error) {
	ar, err := archive.Open(opts.ArchivePath)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}

	store, err := server.LoadStore(ar)
	if err != nil {
		_ = ar.Close()
		return nil, fmt.Errorf("loading capture: %w", err)
	}

	at, err := parseReplayAt(store.Metadata.CapturedAt, store.Metadata.CapturedUntil, opts.At)
	if err != nil {
		_ = ar.Close()
		return nil, err
	}

	port := opts.Port
	if port == "" || port == "0" {
		port = "0"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		_ = ar.Close()
		return nil, fmt.Errorf("listening: %w", err)
	}

	mux := http.NewServeMux()
	// The v2 dashboard is the UI: "/" redirects to /v2/.
	mux.HandleFunc("/", serveRoot)
	v2h := &v2.Handler{
		Store:       store,
		At:          at,
		ArchivePath: opts.ArchivePath,
		Verbose:     opts.Verbose,
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
	<-s.done
}

// Wait blocks until the server stops or an interrupt signal is received.
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
