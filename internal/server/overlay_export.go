package server

import "encoding/json"

// This file exposes a read-only view of the store and (when writable replay
// is on) the overlay to other in-process readers — namely the web UI
// (internal/ui), which otherwise has no way to see client writes made through
// the mock API server (kubectl/helm/kwok/controller-manager). All overlay
// accessors are nil-safe, both on a nil *Server (a caller with no mock server
// wired up at all) and on a Server without a writable overlay — either way
// they behave as if the overlay were empty, so callers don't need to check
// for nil or Writable() first.

// Store returns the CaptureStore backing this server, so a second in-process
// reader (the web UI) can share it instead of loading the archive again.
// Returns nil on a nil *Server, matching every other accessor in this file.
func (s *Server) Store() *CaptureStore {
	if s == nil {
		return nil
	}
	return s.handler.store
}

// OverlayScope identifies a group/version/resource/namespace combination with
// at least one live object in the writable overlay, plus a representative
// object body — used to discover kinds/namespaces that exist only because of
// overlay writes (e.g. a CRD's custom resources, or a namespace created by
// `helm install --create-namespace`) and so have no entry in the capture's
// static index at all. Namespace is "" for cluster-scoped resources.
type OverlayScope struct {
	Group, Version, Resource, Namespace string
	Count                               int
	Sample                              json.RawMessage
}

// OverlayScopes returns every distinct scope with at least one live overlay
// entry. Returns nil when the server has no writable overlay.
func (s *Server) OverlayScopes() []OverlayScope {
	if s == nil || s.handler.overlay == nil {
		return nil
	}
	scopes := s.handler.overlay.scopes()
	out := make([]OverlayScope, len(scopes))
	for i, sc := range scopes {
		out[i] = OverlayScope{
			Group: sc.group, Version: sc.version, Resource: sc.resource, Namespace: sc.namespace,
			Count: sc.count, Sample: sc.sample,
		}
	}
	return out
}

// MergeOverlayList merges overlay writes for (group, version, resource,
// namespace — "" for cluster-wide/all-namespaces) over base, "overlay wins"
// (see overlay.applyToList), and drops any items left in a namespace the
// overlay deleted. Returns base unchanged when the server has no writable
// overlay.
func (s *Server) MergeOverlayList(group, version, resource, namespace string, base []json.RawMessage) []json.RawMessage {
	if s == nil || s.handler.overlay == nil {
		return base
	}
	merged, _ := s.handler.overlay.applyToList(group, version, resource, namespace, base)
	return dropDeletedNamespaceItems(merged, s.handler.overlay.deletedNamespaces())
}

// OverlayObject returns the overlay's copy of a single object identity, if any
// overlay write touched it. found is false when the server has no writable
// overlay or no write ever touched this identity; deleted is true for a
// tombstone (the object was created then deleted in the overlay).
func (s *Server) OverlayObject(group, version, resource, namespace, name string) (obj json.RawMessage, deleted, found bool) {
	if s == nil || s.handler.overlay == nil {
		return nil, false, false
	}
	e, ok := s.handler.overlay.get(group, version, resource, namespace, name)
	if !ok {
		return nil, false, false
	}
	return e.obj, e.deleted, true
}

// OverlayDeletedNamespaces returns the set of namespaces deleted via the
// overlay, so a reader can filter their (possibly still-captured) contents
// out. Returns nil when the server has no writable overlay.
func (s *Server) OverlayDeletedNamespaces() map[string]struct{} {
	if s == nil || s.handler.overlay == nil {
		return nil
	}
	return s.handler.overlay.deletedNamespaces()
}
