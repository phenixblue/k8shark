// Package v2 is the redesigned k8shark web UI. It mounts under /v2/ and
// provides a dashboard-style overview plus drilldowns for namespaces, pods,
// and other resources. Eventually intended to replace the original UI in
// internal/ui — until then both live side by side.
package v2

import (
	"embed"
	"io/fs"
	"net/http"
	"sync"
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
	// Clock, when non-nil, puts the dashboard in replay mode: resolveAt follows
	// the clock and /v2/api/replay drives it (see replay.go).
	Clock *server.ReplayClock
	// Overlay is the mock API server backing this same archive. Every
	// list/detail read merges its writable-overlay writes (kubectl/helm/kwok/
	// controller-manager) over the captured state, so the dashboard shows
	// live changes instead of only what was captured. Its accessor methods
	// (OverlayScopes, MergeOverlayList, ...) are nil-safe on both a nil
	// *Server (plain `open`, or a caller that didn't wire one up) and a
	// Server without a writable overlay — callers here can call them on
	// h.Overlay directly without checking either case first.
	Overlay *server.Server

	// discoveryMetaOnce/discoveryMetaCache memoize discoveryResourceMeta
	// (objects.go): it's derived purely from the capture's own discovery
	// documents, which never change for a given archive, but computing it
	// scans the whole index and re-parses every discovery document — worth
	// doing once per Handler (one per running dashboard) rather than per
	// request.
	discoveryMetaOnce  sync.Once
	discoveryMetaCache map[string]discMeta
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
	mux.HandleFunc("/v2/api/diagnostics", h.serveDiagnostics)
	mux.HandleFunc("/v2/api/capture", h.serveCaptureInfo)
	mux.HandleFunc("/v2/api/namespace", h.serveNamespace)
	mux.HandleFunc("/v2/api/pods", h.serveAllPods)
	mux.HandleFunc("/v2/api/workloads", h.serveAllWorkloads)
	mux.HandleFunc("/v2/api/resources", h.serveResourceCatalog)
	mux.HandleFunc("/v2/api/resource", h.serveResourceList)
	mux.HandleFunc("/v2/api/object", h.serveObject)
	mux.HandleFunc("/v2/api/object-relationships", h.serveObjectRelationships)
	mux.HandleFunc("/v2/api/pod", h.servePod)
	mux.HandleFunc("/v2/api/events", h.serveEvents)
	mux.HandleFunc("/v2/api/logs", h.serveLogs)
	mux.HandleFunc("/v2/api/diff", h.serveDiff)
	mux.HandleFunc("/v2/api/object-history", h.serveObjectHistory)
	mux.HandleFunc("/v2/api/timestamps", h.serveTimestamps)
	mux.HandleFunc("/v2/api/replay", h.serveReplay)
	mux.HandleFunc("/v2/api/replay/", h.serveReplay)
}
