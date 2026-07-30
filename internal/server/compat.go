package server

import (
	"net/http"
	"strings"
)

// tryRejectInteractiveSubresource intercepts interactive sub-resources that
// can never work against a capture replay, writing a specific, actionable
// error and returning true. ServeHTTP checks this before the read-only
// method check so the client gets this message instead of the generic
// "write operations are not supported" one — and so we don't hang waiting
// for a protocol upgrade.
//
//	kubectl exec / kubectl cp  → POST .../pods/<name>/exec
//	kubectl port-forward       → POST .../pods/<name>/portforward
//	kubectl attach             → POST .../pods/<name>/attach
//	istioctl proxy-status      → GET/POST .../pods/<name>/proxy/...
//	                             GET/POST .../services/<name>/proxy/...
func (h *handler) tryRejectInteractiveSubresource(w http.ResponseWriter, path string) bool {
	if !(strings.HasSuffix(path, "/exec") ||
		strings.HasSuffix(path, "/portforward") ||
		strings.HasSuffix(path, "/attach") ||
		strings.HasSuffix(path, "/proxy") ||
		strings.Contains(path, "/proxy/")) {
		return false
	}
	w.Header().Set("Allow", "")
	h.writeStatus(w, http.StatusMethodNotAllowed,
		"k8shark capture replay: exec, cp, port-forward, and proxy are not supported — "+
			"this mock server replays a captured snapshot and cannot run commands "+
			"or proxy connections to pods/services")
	return true
}

// tryClientCompatShim synthesizes permissive read-only responses for
// client-tooling capability checks — e.g. k9s POSTs authorization review
// resources to determine what actions are available. These requests are
// read-only capability checks, not mutating operations, so without this
// shim they'd otherwise break navigation workflows against a read-only
// replay. Returns true if it handled the request.
func (h *handler) tryClientCompatShim(w http.ResponseWriter, r *http.Request, path string) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch path {
	case "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
		writeJSON(w, http.StatusCreated, map[string]any{
			"apiVersion": "authorization.k8s.io/v1",
			"kind":       "SelfSubjectAccessReview",
			"status": map[string]any{
				"allowed": true,
				"denied":  false,
				"reason":  "k8shark replay server: read-only access checks allowed for client compatibility",
			},
		})
		return true
	case "/apis/authorization.k8s.io/v1/selfsubjectrulesreviews":
		writeJSON(w, http.StatusCreated, map[string]any{
			"apiVersion": "authorization.k8s.io/v1",
			"kind":       "SelfSubjectRulesReview",
			"status": map[string]any{
				"incomplete": false,
				"resourceRules": []map[string]any{
					{
						"verbs":     []string{"get", "list", "watch"},
						"apiGroups": []string{"*"},
						"resources": []string{"*"},
					},
				},
				"nonResourceRules": []map[string]any{
					{
						"verbs":           []string{"get"},
						"nonResourceURLs": []string{"*"},
					},
				},
			},
		})
		return true
	}
	return false
}
