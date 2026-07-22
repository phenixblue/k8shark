package v2

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
)

// newOverlayTestHandler starts a real writable-replay mock server over a
// minimal one-pod, one-namespace capture and wires its store+overlay into a
// v2 Handler exactly the way ui.Open does in production. Tests drive writes
// through the server's real HTTPS API (see overlayPost/overlayDelete) rather
// than reaching into overlay internals, so they exercise the same integration
// a real kubectl/helm client would.
func newOverlayTestHandler(t *testing.T) (*Handler, *server.Server) {
	t.Helper()
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "capture.kshrk")

	nsList := `{"apiVersion":"v1","kind":"NamespaceList","items":[{"metadata":{"name":"default"}}]}`
	podList := `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"web","namespace":"default"},"status":{"phase":"Running"}}]}`
	recs := []*capture.Record{
		{ID: "n1", CapturedAt: now, APIPath: "/api/v1/namespaces", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nsList)},
		{ID: "p1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podList)},
	}
	idx := capture.Index{
		"/api/v1/namespaces":              {APIPath: "/api/v1/namespaces", Seqs: []int{0}, Times: []time.Time{now}, Counts: []int{1}},
		"/api/v1/namespaces/default/pods": {APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}, Times: []time.Time{now}, Counts: []int{1}},
	}
	meta := &capture.CaptureMetadata{CaptureID: "overlay-test", CapturedAt: now.Add(-5 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}

	sw, err := archive.NewStreamWriter(path)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	for _, r := range recs {
		if err := sw.WriteRecord(r); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("archive Finish: %v", err)
	}

	srv, err := server.Replay(server.ReplayOptions{
		ArchivePath:       path,
		KubeconfigOut:     filepath.Join(dir, "kubeconfig.yaml"),
		StartPaused:       true,
		Writable:          true,
		DisableScheduling: true,
	})
	if err != nil {
		t.Fatalf("server.Replay: %v", err)
	}
	t.Cleanup(srv.Shutdown)

	return &Handler{Store: srv.Store(), Overlay: srv, At: now}, srv
}

var overlayTestClient = &http.Client{
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // #nosec G402 -- test only
}

// overlayPost creates an object through the mock server's real HTTPS API.
func overlayPost(t *testing.T, srv *server.Server, path, body string) {
	t.Helper()
	resp, err := overlayTestClient.Post(srv.Address()+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST %s = %d, want 201: %s", path, resp.StatusCode, b)
	}
}

// overlayDelete deletes an object through the mock server's real HTTPS API.
func overlayDelete(t *testing.T, srv *server.Server, path string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, srv.Address()+path, nil)
	if err != nil {
		t.Fatalf("new DELETE request: %v", err)
	}
	resp, err := overlayTestClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("DELETE %s = %d, want 200: %s", path, resp.StatusCode, b)
	}
}

const vsPath = "/apis/networking.istio.io/v1beta1/namespaces/istio-system/virtualservices"
const vsBody = `{"apiVersion":"networking.istio.io/v1beta1","kind":"VirtualService","metadata":{"name":"reviews","namespace":"istio-system"},"spec":{"hosts":["reviews"]}}`

// TestOverlay_NewCRDResourceType_VisibleEverywhere covers the core "resources
// created only via the writable overlay have no entry in the capture's index"
// case: a namespace and a custom-resource kind the capture never saw (as if a
// CRD were installed and a CR created mid-replay) must still surface through
// every discovery path — the generic resource list, the resource catalog, and
// the single-object view.
func TestOverlay_NewCRDResourceType_VisibleEverywhere(t *testing.T) {
	h, srv := newOverlayTestHandler(t)
	overlayPost(t, srv, vsPath, vsBody)

	t.Run("resourcePathsFor discovers the overlay-only path", func(t *testing.T) {
		paths := h.resourcePathsFor("virtualservices")
		found := false
		for _, p := range paths {
			if p == vsPath {
				found = true
			}
		}
		if !found {
			t.Errorf("resourcePathsFor(virtualservices) = %v, want it to include %s", paths, vsPath)
		}
	})

	t.Run("serveResourceList includes it", func(t *testing.T) {
		var out ResourceList
		if code := getJSONInto(t, h, h.serveResourceList, "/v2/api/resource", "?resource=virtualservices", &out); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if out.Total != 1 || len(out.Items) != 1 || out.Items[0].Name != "reviews" || out.Items[0].Namespace != "istio-system" {
			t.Errorf("serveResourceList = %+v, want one reviews/istio-system item", out)
		}
	})

	t.Run("serveResourceCatalog includes it with the right Kind and count", func(t *testing.T) {
		var out ResourceCatalog
		if code := getJSONInto(t, h, h.serveResourceCatalog, "/v2/api/resources", "", &out); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		var row *ResourceCatalogRow
		for i := range out.Resources {
			if out.Resources[i].Resource == "virtualservices" {
				row = &out.Resources[i]
			}
		}
		if row == nil {
			t.Fatalf("serveResourceCatalog missing virtualservices: %+v", out.Resources)
		}
		if row.Kind != "VirtualService" {
			t.Errorf("Kind = %q, want VirtualService (derived from the overlay object's own kind field)", row.Kind)
		}
		if row.Count != 1 || !row.Namespaced {
			t.Errorf("row = %+v, want Count=1 Namespaced=true", row)
		}
	})

	t.Run("serveObject finds the single object", func(t *testing.T) {
		var d ObjectDetail
		if code := getJSONInto(t, h, h.serveObject, "/v2/api/object", "?path="+strings.ReplaceAll(vsPath, "/", "%2F")+"&name=reviews", &d); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if !d.Found || d.Kind != "VirtualService" || d.Namespace != "istio-system" {
			t.Errorf("ObjectDetail = %+v, want Found=true Kind=VirtualService", d)
		}
	})

	t.Run("serveObject whole-list view has correct synthetic Kind/apiVersion casing", func(t *testing.T) {
		var d ObjectDetail
		if code := getJSONInto(t, h, h.serveObject, "/v2/api/object", "?path="+strings.ReplaceAll(vsPath, "/", "%2F"), &d); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if !d.Found {
			t.Fatalf("ObjectDetail.Found = false, want true")
		}
		if !strings.Contains(d.JSON, `"kind": "VirtualServiceList"`) {
			t.Errorf("JSON missing correctly-cased synthetic envelope kind:\n%s", d.JSON)
		}
		if !strings.Contains(d.JSON, `"apiVersion": "networking.istio.io/v1beta1"`) {
			t.Errorf("JSON missing synthetic envelope apiVersion:\n%s", d.JSON)
		}
	})

	t.Run("delete tombstones it everywhere", func(t *testing.T) {
		overlayDelete(t, srv, vsPath+"/reviews")

		var d ObjectDetail
		if code := getJSONInto(t, h, h.serveObject, "/v2/api/object", "?path="+strings.ReplaceAll(vsPath, "/", "%2F")+"&name=reviews", &d); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if d.Found {
			t.Errorf("ObjectDetail.Found = true after delete, want false")
		}

		var out ResourceList
		if code := getJSONInto(t, h, h.serveResourceList, "/v2/api/resource", "?resource=virtualservices", &out); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if out.Total != 0 {
			t.Errorf("serveResourceList after delete = %+v, want empty", out)
		}
	})
}

// TestOverlay_NewNamespace_VisibleInPicker covers a namespace created purely
// via the overlay (e.g. `helm install --create-namespace`), which — like the
// CRD case above — has no index entry at all: it must still appear in the
// namespace picker (the overview's namespace list).
func TestOverlay_NewNamespace_VisibleInPicker(t *testing.T) {
	h, srv := newOverlayTestHandler(t)
	overlayPost(t, srv, "/api/v1/namespaces", `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"istio-system"}}`)
	overlayPost(t, srv, vsPath, vsBody)

	var ov Overview
	if code := getJSONInto(t, h, h.serveOverview, "/v2/api/overview", "", &ov); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var found *NamespaceSummary
	for i := range ov.Namespaces {
		if ov.Namespaces[i].Name == "istio-system" {
			found = &ov.Namespaces[i]
		}
	}
	if found == nil {
		t.Fatalf("overview.Namespaces missing istio-system: %+v", ov.Namespaces)
	}
	if found.Resources < 1 {
		t.Errorf("istio-system.Resources = %d, want >= 1 (the virtualservice)", found.Resources)
	}
}

// TestOverlay_PodInExistingNamespace_VisibleInLists is the more common case:
// a pod added to a namespace the capture already had, e.g. via kwok/
// controller-manager. Existing list views (already index-known paths) must
// merge it in.
func TestOverlay_PodInExistingNamespace_VisibleInLists(t *testing.T) {
	h, srv := newOverlayTestHandler(t)
	overlayPost(t, srv, "/api/v1/namespaces/default/pods",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"sidecar-injected","namespace":"default"},"status":{"phase":"Running"}}`)

	var resp PodsList
	if code := getJSONInto(t, h, h.serveAllPods, "/v2/api/pods", "", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2 (captured web + overlay sidecar-injected)", resp.Total)
	}
	var names []string
	for _, p := range resp.Pods {
		names = append(names, p.Name)
	}
	found := false
	for _, n := range names {
		if n == "sidecar-injected" {
			found = true
		}
	}
	if !found {
		t.Errorf("pods = %v, want sidecar-injected included", names)
	}
}
