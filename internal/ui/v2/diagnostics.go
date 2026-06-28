package v2

import (
	"net/http"

	"github.com/phenixblue/k8shark/internal/diagnose"
)

// serveDiagnostics runs the diagnose engine over the capture at the resolved
// time and returns the ranked Report. Backs the dashboard Diagnostics view.
func (h *Handler) serveDiagnostics(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeError(w, http.StatusInternalServerError, "store not initialized")
		return
	}
	at := h.resolveAt(r)
	rep := diagnose.Run(h.Store, diagnose.Options{At: at})
	writeJSON(w, http.StatusOK, rep)
}
