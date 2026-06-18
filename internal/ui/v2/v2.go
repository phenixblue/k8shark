// Package v2 is the redesigned k8shark web UI. It mounts under /v2/ and
// provides a dashboard-style overview plus drilldowns for namespaces, pods,
// and other resources. Eventually intended to replace the original UI in
// internal/ui — until then both live side by side.
package v2

import (
	"embed"
	"io/fs"
	"net/http"
	"time"

	"github.com/phenixblue/k8shark/internal/server"
)

//go:embed static
var staticFS embed.FS

// Handler holds the shared state for v2 endpoints.
type Handler struct {
	Store       *server.CaptureStore
	At          time.Time
	ArchivePath string
	Verbose     bool
}

// Mount registers all /v2/* routes on mux. The /v2/ HTML shell is served
// from the embedded static FS; /v2/api/* endpoints return JSON.
func (h *Handler) Mount(mux *http.ServeMux) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embed failure is a compile-time misconfiguration
	}
	mux.Handle("/v2/", http.StripPrefix("/v2/", http.FileServer(http.FS(sub))))

	mux.HandleFunc("/v2/api/overview", h.serveOverview)
	mux.HandleFunc("/v2/api/namespace", h.serveNamespace)
	mux.HandleFunc("/v2/api/pods", h.serveAllPods)
	mux.HandleFunc("/v2/api/workloads", h.serveAllWorkloads)
	mux.HandleFunc("/v2/api/pod", h.servePod)
	mux.HandleFunc("/v2/api/events", h.serveEvents)
	mux.HandleFunc("/v2/api/logs", h.serveLogs)
	mux.HandleFunc("/v2/api/diff", h.serveDiff)
	mux.HandleFunc("/v2/api/object-history", h.serveObjectHistory)
	mux.HandleFunc("/v2/api/timestamps", h.serveTimestamps)
}
