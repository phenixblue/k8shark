package server

import (
	"context"
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
	cancel         context.CancelFunc
	done           chan struct{}
}

// Open loads a capture archive and starts the mock server.
// Full implementation comes in Milestone 3.
func Open(opts OpenOptions) (*Server, error) {
	panic("mock server not yet implemented")
}

// Address returns the listen address (e.g. "https://127.0.0.1:54321").
func (s *Server) Address() string {
	return s.address
}

// KubeconfigPath returns the path to the generated kubeconfig file.
func (s *Server) KubeconfigPath() string {
	return s.kubeconfigPath
}

// Wait blocks until the server is stopped (e.g. via Ctrl+C).
func (s *Server) Wait() error {
	<-s.done
	return nil
}
