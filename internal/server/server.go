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
)

// OpenOptions holds parameters for opening a capture archive.
type OpenOptions struct {
	ArchivePath   string
	Port          string
	KubeconfigOut string
	At            string
	Verbose       bool
}

// Server represents a running mock API server.
type Server struct {
	address        string
	kubeconfigPath string
	tmpDir         string
	httpServer     *http.Server
	done           chan struct{}
}

// Open extracts a capture archive, starts the mock HTTPS server, and writes
// a kubeconfig pointing at it.
func Open(opts OpenOptions) (*Server, error) {
	// Extract the archive to a temp directory.
	tmpDir, err := os.MkdirTemp("", "k8shark-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}

	if err := archive.Open(opts.ArchivePath, tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("extracting archive: %w", err)
	}

	// Load the capture index into memory.
	store, err := LoadStore(tmpDir)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("loading capture: %w", err)
	}

	// Parse optional replay timestamp.
	var at time.Time
	if opts.At != "" {
		at, err = time.Parse(time.RFC3339, opts.At)
		if err != nil {
			_ = os.RemoveAll(tmpDir)
			return nil, fmt.Errorf("parsing --at timestamp %q: %w", opts.At, err)
		}
	}

	// Generate a self-signed TLS certificate.
	certPEM, keyPEM, err := generateSelfSignedCert()
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("generating TLS cert: %w", err)
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("loading TLS cert: %w", err)
	}

	// Listen on requested port (0 = any available port).
	port := opts.Port
	if port == "" || port == "0" {
		port = "0"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("listening: %w", err)
	}

	addr := fmt.Sprintf("https://127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port)

	// Write kubeconfig.
	kubeconfigPath := opts.KubeconfigOut
	if kubeconfigPath == "" {
		home, _ := os.UserHomeDir()
		kubeconfigPath = filepath.Join(home, ".kube", "k8shark-"+store.Metadata.CaptureID+".yaml")
	}
	if err := writeKubeconfig(addr, kubeconfigPath); err != nil {
		_ = os.RemoveAll(tmpDir)
		_ = ln.Close()
		return nil, fmt.Errorf("writing kubeconfig: %w", err)
	}

	// Wrap the listener with TLS.
	tlsListener := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS12,
	})

	httpSrv := &http.Server{Handler: newHandler(store, at, opts.Verbose)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = httpSrv.Serve(tlsListener)
	}()

	return &Server{
		address:        addr,
		kubeconfigPath: kubeconfigPath,
		tmpDir:         tmpDir,
		httpServer:     httpSrv,
		done:           done,
	}, nil
}

// Address returns the HTTPS listen address (e.g. "https://127.0.0.1:54321").
func (s *Server) Address() string { return s.address }

// KubeconfigPath returns the path to the generated kubeconfig.
func (s *Server) KubeconfigPath() string { return s.kubeconfigPath }

// Shutdown immediately stops the server and cleans up the temp directory.
// Useful in tests and programmatic usage; Wait() is preferred for CLI use.
func (s *Server) Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
	<-s.done
	_ = os.RemoveAll(s.tmpDir)
}

// Wait blocks until Ctrl+C / SIGTERM, then shuts the server down and cleans up.
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
	_ = os.RemoveAll(s.tmpDir)
	return nil
}
