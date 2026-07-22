package server

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// TestServer_Store_ReturnsSharedInstance covers the whole point of exposing
// Store(): a second in-process reader (the web UI) must see the exact same
// CaptureStore the mock API server reads from, not a fresh independent load.
func TestServer_Store_ReturnsSharedInstance(t *testing.T) {
	archivePath := buildTestArchive(t)
	srv, err := Open(OpenOptions{ArchivePath: archivePath, KubeconfigOut: filepath.Join(t.TempDir(), "kubeconfig.yaml")})
	if err != nil {
		t.Fatalf("server.Open: %v", err)
	}
	defer srv.Shutdown()

	if got := srv.Store(); got != srv.handler.store {
		t.Errorf("Store() = %p, want the handler's own store %p", got, srv.handler.store)
	}
}

// TestServer_OverlayAccessors_NilSafeWithoutOverlay covers a plain `open`/`ui`
// server (no --writable): every overlay accessor must behave as an empty
// overlay rather than panicking, so a caller (the web UI) doesn't need to
// special-case Writable() before calling them.
func TestServer_OverlayAccessors_NilSafeWithoutOverlay(t *testing.T) {
	archivePath := buildTestArchive(t)
	srv, err := Open(OpenOptions{ArchivePath: archivePath, KubeconfigOut: filepath.Join(t.TempDir(), "kubeconfig.yaml")})
	if err != nil {
		t.Fatalf("server.Open: %v", err)
	}
	defer srv.Shutdown()

	if scopes := srv.OverlayScopes(); scopes != nil {
		t.Errorf("OverlayScopes() = %v, want nil", scopes)
	}
	base := []json.RawMessage{json.RawMessage(`{"metadata":{"name":"nginx"}}`)}
	if got := srv.MergeOverlayList("", "v1", "pods", "default", base); len(got) != 1 || string(got[0]) != string(base[0]) {
		t.Errorf("MergeOverlayList() = %v, want base unchanged", got)
	}
	if _, _, found := srv.OverlayObject("", "v1", "pods", "default", "nginx"); found {
		t.Error("OverlayObject() found = true, want false")
	}
	if dns := srv.OverlayDeletedNamespaces(); dns != nil {
		t.Errorf("OverlayDeletedNamespaces() = %v, want nil", dns)
	}
}

// TestServer_OverlayAccessors_WritableServer drives a writable replay
// server's overlay directly (same package) and checks the exported
// accessors — the surface the web UI will use — return what was written.
func TestServer_OverlayAccessors_WritableServer(t *testing.T) {
	archivePath := buildTestArchive(t)
	srv, err := Replay(ReplayOptions{
		ArchivePath:       archivePath,
		KubeconfigOut:     filepath.Join(t.TempDir(), "kubeconfig.yaml"),
		Writable:          true,
		DisableScheduling: true, // keep the overlay free of the synthetic scheduling node
	})
	if err != nil {
		t.Fatalf("server.Replay: %v", err)
	}
	defer srv.Shutdown()

	ov := srv.handler.overlay
	if ov == nil {
		t.Fatal("expected a writable server to have an overlay")
	}

	// A brand-new CRD's custom resource: a GVR never in the capture's index.
	vsBody := json.RawMessage(`{"apiVersion":"networking.istio.io/v1beta1","kind":"VirtualService","metadata":{"name":"reviews","namespace":"istio-system"}}`)
	ov.store("networking.istio.io", "v1beta1", "virtualservices", "istio-system", "reviews", vsBody, ov.nextRV(0))

	// A namespace object, created via the overlay (e.g. --create-namespace).
	nsBody := json.RawMessage(`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"istio-system"}}`)
	ov.store("", "v1", "namespaces", "", "istio-system", nsBody, ov.nextRV(0))

	scopes := srv.OverlayScopes()
	if len(scopes) != 2 {
		t.Fatalf("OverlayScopes() = %d scopes, want 2: %+v", len(scopes), scopes)
	}
	var sawVS bool
	for _, sc := range scopes {
		if sc.Group == "networking.istio.io" && sc.Resource == "virtualservices" {
			sawVS = true
			if sc.Namespace != "istio-system" || sc.Count != 1 {
				t.Errorf("virtualservices scope = %+v, want namespace=istio-system count=1", sc)
			}
			if string(sc.Sample) != string(vsBody) {
				t.Errorf("virtualservices sample = %s, want %s", sc.Sample, vsBody)
			}
		}
	}
	if !sawVS {
		t.Errorf("OverlayScopes() missing the overlay-only virtualservices scope: %+v", scopes)
	}

	merged := srv.MergeOverlayList("networking.istio.io", "v1beta1", "virtualservices", "istio-system", nil)
	if len(merged) != 1 || string(merged[0]) != string(vsBody) {
		t.Errorf("MergeOverlayList() = %v, want [%s]", merged, vsBody)
	}

	obj, deleted, found := srv.OverlayObject("networking.istio.io", "v1beta1", "virtualservices", "istio-system", "reviews")
	if !found || deleted || string(obj) != string(vsBody) {
		t.Errorf("OverlayObject() = (%s, deleted=%v, found=%v), want (%s, false, true)", obj, deleted, found, vsBody)
	}

	// Deleting the namespace should surface it via OverlayDeletedNamespaces
	// and MergeOverlayList should drop the virtualservices item accordingly.
	ov.cascadeDeleteNamespace("istio-system")
	ov.del("", "v1", "namespaces", "", "istio-system", nsBody, ov.nextRV(0))
	dns := srv.OverlayDeletedNamespaces()
	if _, ok := dns["istio-system"]; !ok {
		t.Errorf("OverlayDeletedNamespaces() = %v, want istio-system present", dns)
	}
	if merged := srv.MergeOverlayList("networking.istio.io", "v1beta1", "virtualservices", "istio-system", nil); len(merged) != 0 {
		t.Errorf("MergeOverlayList() after namespace delete = %v, want empty", merged)
	}
}
