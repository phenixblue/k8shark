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
		PauseAtWindowEnd:  true, // park after the pod/namespace records, not before
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
	Timeout:   10 * time.Second,                                                        // bound test failures instead of hanging on a stalled server
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

// overlayPatch strategic-merge-patches an object (adopting it into the
// overlay if it was only ever captured) through the mock server's real
// HTTPS API.
func overlayPatch(t *testing.T, srv *server.Server, path, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, srv.Address()+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new PATCH request: %v", err)
	}
	req.Header.Set("Content-Type", "application/strategic-merge-patch+json")
	resp, err := overlayTestClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH %s = %d, want 200: %s", path, resp.StatusCode, b)
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

// TestOverlay_UpdateExistingObject_CatalogCountNotDoubled is a regression
// test: an overlay write that updates an object the capture already had
// (not a new one) must not inflate the resource catalog's count. Before the
// fix, OverlayScopes' count (every *live* overlay entry, including updates)
// was summed on top of the index-derived count unconditionally.
func TestOverlay_UpdateExistingObject_CatalogCountNotDoubled(t *testing.T) {
	h, srv := newOverlayTestHandler(t)
	overlayPatch(t, srv, "/api/v1/namespaces/default/pods/web", `{"metadata":{"labels":{"updated":"true"}}}`)

	var out ResourceCatalog
	if code := getJSONInto(t, h, h.serveResourceCatalog, "/v2/api/resources", "", &out); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var row *ResourceCatalogRow
	for i := range out.Resources {
		if out.Resources[i].Resource == "pods" {
			row = &out.Resources[i]
		}
	}
	if row == nil {
		t.Fatalf("serveResourceCatalog missing pods: %+v", out.Resources)
	}
	if row.Count != 1 {
		t.Errorf("pods Count = %d, want 1 (the same pod, updated in place — not double-counted against the index)", row.Count)
	}
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

// TestOverlay_NewNamespace_KPIsResourcesIncludesPodsWorkloadsVMs is a
// regression test: KPIs.Resources for an overlay-only namespace (no index
// counts at all) must include the exact pod/workload/VM counts, not just the
// index-derived resTotal — before the fix it stayed at 0 for those kinds.
func TestOverlay_NewNamespace_KPIsResourcesIncludesPodsWorkloadsVMs(t *testing.T) {
	h, srv := newOverlayTestHandler(t)
	overlayPost(t, srv, "/api/v1/namespaces", `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"istio-system"}}`)
	overlayPost(t, srv, "/api/v1/namespaces/istio-system/pods",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"istiod","namespace":"istio-system"},"status":{"phase":"Running"}}`)

	var d NamespaceDetail
	if code := getJSONInto(t, h, h.serveNamespace, "/v2/api/namespace", "?ns=istio-system", &d); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if d.KPIs.Pods != 1 {
		t.Fatalf("KPIs.Pods = %d, want 1", d.KPIs.Pods)
	}
	if d.KPIs.Resources < 1 {
		t.Errorf("KPIs.Resources = %d, want >= 1 (must include the overlay-only pod)", d.KPIs.Resources)
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

// TestResourcePathsForResources_GroupsBySingleScan covers the multi-resource
// helper the cluster-wide workload list uses to avoid scanning the index once
// per workload kind: a single call must correctly group paths — including an
// overlay-only one with no index entry at all — by resource.
func TestResourcePathsForResources_GroupsBySingleScan(t *testing.T) {
	h, srv := newOverlayTestHandler(t)
	overlayPost(t, srv, "/apis/apps/v1/namespaces/default/deployments",
		`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"default"}}`)

	got := h.resourcePathsForResources(map[string]bool{"pods": true, "deployments": true, "statefulsets": true})

	if paths := got["pods"]; len(paths) != 1 || paths[0] != "/api/v1/namespaces/default/pods" {
		t.Errorf("pods = %v, want exactly the captured path", paths)
	}
	if paths := got["deployments"]; len(paths) != 1 || paths[0] != "/apis/apps/v1/namespaces/default/deployments" {
		t.Errorf("deployments = %v, want the overlay-only path (no index entry for it)", paths)
	}
	if paths := got["statefulsets"]; len(paths) != 0 {
		t.Errorf("statefulsets = %v, want none (neither captured nor overlay-created)", paths)
	}

	// resourcePathsFor(resource) must agree with the grouped result.
	if single := h.resourcePathsFor("pods"); len(single) != 1 || single[0] != got["pods"][0] {
		t.Errorf("resourcePathsFor(pods) = %v, want to match resourcePathsForResources' pods entry %v", single, got["pods"])
	}
}

// TestServeObject_WholeListView_IgnoresNonListCapturedBody is a regression
// test: when the path's last captured record was NOT a successful list
// response (e.g. a Kubernetes Status object from a 404, from before a CRD
// existed), the whole-list view must build the synthetic envelope from the
// live overlay items instead of treating the Status body as the envelope —
// which would otherwise leak a bogus kind/apiVersion/status into the output.
func TestServeObject_WholeListView_IgnoresNonListCapturedBody(t *testing.T) {
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "capture.kshrk")

	statusBody := `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"the server could not find the requested resource","code":404}`
	recs := []*capture.Record{
		{ID: "s1", CapturedAt: now, APIPath: vsPath, HTTPMethod: "GET", ResponseCode: 404, ResponseBody: json.RawMessage(statusBody)},
	}
	idx := capture.Index{
		vsPath: {APIPath: vsPath, Seqs: []int{0}, Times: []time.Time{now}},
	}
	meta := &capture.CaptureMetadata{CaptureID: "overlay-404-test", CapturedAt: now.Add(-5 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}

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
		PauseAtWindowEnd:  true,
		Writable:          true,
		DisableScheduling: true,
	})
	if err != nil {
		t.Fatalf("server.Replay: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	h := &Handler{Store: srv.Store(), Overlay: srv, At: now}

	overlayPost(t, srv, vsPath, vsBody)

	var d ObjectDetail
	if code := getJSONInto(t, h, h.serveObject, "/v2/api/object", "?path="+strings.ReplaceAll(vsPath, "/", "%2F"), &d); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !d.Found {
		t.Fatalf("Found = false, want true (the overlay has a live item)")
	}
	if !strings.Contains(d.JSON, `"kind": "VirtualServiceList"`) {
		t.Errorf("JSON missing correct synthetic envelope kind:\n%s", d.JSON)
	}
	if strings.Contains(d.JSON, "Failure") || strings.Contains(d.JSON, `"code": 404`) {
		t.Errorf("JSON leaked the captured 404 Status body instead of a synthetic list envelope:\n%s", d.JSON)
	}
}
