package capture

import (
	"github.com/phenixblue/k8shark/internal/config"
)

// Engine orchestrates the capture loop.
type Engine struct {
	cfg     *config.Config
	verbose bool
}

// NewEngine creates a capture Engine from validated config.
func NewEngine(cfg *config.Config, verbose bool) (*Engine, error) {
	return &Engine{cfg: cfg, verbose: verbose}, nil
}

// Run executes the capture and writes the output archive.
// Full implementation comes in Milestone 2.
func (e *Engine) Run() error {
	panic("capture engine not yet implemented")
}
