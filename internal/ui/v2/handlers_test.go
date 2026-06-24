package v2

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/capture"
)

// newFleetTestHandler builds a small but representative capture: one namespace
// (default) with a healthy pod, a CrashLoopBackOff pod, and a Deployment. It
// exercises the cross-namespace list, pod-detail, namespace, and overview
// handlers from a single store. Index Counts are populated because the overview
// KPIs derive pod/workload totals from the index, not body reads.
func newFleetTestHandler(t *testing.T) *Handler {
	t.Helper()
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)

	nsList := `{"apiVersion":"v1","kind":"NamespaceList","items":[{"metadata":{"name":"default"}}]}`
	podList := `{"apiVersion":"v1","kind":"PodList","items":[` +
		`{"metadata":{"name":"web","namespace":"default"},"spec":{"containers":[{"name":"app"}]},` +
		`"status":{"phase":"Running","containerStatuses":[{"name":"app","ready":true,"restartCount":0,"state":{"running":{}}}]}},` +
		`{"metadata":{"name":"crasher","namespace":"default"},"spec":{"containers":[{"name":"app"}]},` +
		`"status":{"phase":"Running","containerStatuses":[{"name":"app","ready":false,"restartCount":5,` +
		`"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off 5m0s"}},` +
		`"lastState":{"terminated":{"reason":"Error","exitCode":1}}}]}}]}`
	deployList := `{"apiVersion":"apps/v1","kind":"DeploymentList","items":[` +
		`{"metadata":{"name":"web","namespace":"default","labels":{"app":"web"}},"spec":{"replicas":1},"status":{"readyReplicas":1}}]}`

	recs := []*capture.Record{
		{ID: "n1", CapturedAt: now, APIPath: "/api/v1/namespaces", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nsList)},
		{ID: "p1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podList)},
		{ID: "d1", CapturedAt: now, APIPath: "/apis/apps/v1/namespaces/default/deployments", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(deployList)},
	}
	idx := capture.Index{
		"/api/v1/namespaces":                           {APIPath: "/api/v1/namespaces", Seqs: []int{0}, Times: []time.Time{now}, Counts: []int{1}},
		"/api/v1/namespaces/default/pods":              {APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}, Times: []time.Time{now}, Counts: []int{2}},
		"/apis/apps/v1/namespaces/default/deployments": {APIPath: "/apis/apps/v1/namespaces/default/deployments", Seqs: []int{0}, Times: []time.Time{now}, Counts: []int{1}},
	}
	meta := &capture.CaptureMetadata{CaptureID: "fleet-test", CapturedAt: now.Add(-5 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	return &Handler{Store: buildV2TestStore(t, recs, idx, meta), At: now}
}

func getJSONInto(t *testing.T, h *Handler, fn http.HandlerFunc, target, query string, out any) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target+query, nil)
	w := httptest.NewRecorder()
	fn(w, req)
	if w.Code == http.StatusOK && out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v\n%s", target, err, w.Body.String())
		}
	}
	return w.Code
}

func TestServeAllPods(t *testing.T) {
	h := newFleetTestHandler(t)
	var resp PodsList
	if code := getJSONInto(t, h, h.serveAllPods, "/v2/api/pods", "", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2", resp.Total)
	}
	if resp.Unhealthy != 1 {
		t.Errorf("Unhealthy = %d, want 1", resp.Unhealthy)
	}
	var crasher *ClusterPodRow
	for i := range resp.Pods {
		if resp.Pods[i].Name == "crasher" {
			crasher = &resp.Pods[i]
		}
	}
	if crasher == nil {
		t.Fatalf("crasher pod not in rows: %+v", resp.Pods)
	}
	if crasher.Restarts != 5 {
		t.Errorf("crasher.Restarts = %d, want 5", crasher.Restarts)
	}
	if !crasher.Unhealthy {
		t.Errorf("crasher should be unhealthy")
	}
}

func TestServeAllWorkloads(t *testing.T) {
	h := newFleetTestHandler(t)
	var resp WorkloadsList
	if code := getJSONInto(t, h, h.serveAllWorkloads, "/v2/api/workloads", "", &resp); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	found := false
	for _, wl := range resp.Workloads {
		if wl.Kind == "Deployment" && wl.Name == "web" && wl.Namespace == "default" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Deployment web in workloads: %+v", resp.Workloads)
	}
}

func TestServePod_Detail(t *testing.T) {
	h := newFleetTestHandler(t)
	var d PodDetail
	if code := getJSONInto(t, h, h.servePod, "/v2/api/pod", "?ns=default&name=crasher", &d); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if d.Name != "crasher" || d.Namespace != "default" {
		t.Errorf("identity = %s/%s", d.Namespace, d.Name)
	}
	if d.Hero.Phase != "Running" {
		t.Errorf("Hero.Phase = %q, want Running", d.Hero.Phase)
	}
	if d.KPIs.Restarts != 5 {
		t.Errorf("KPIs.Restarts = %d, want 5", d.KPIs.Restarts)
	}
	if len(d.Containers) == 0 {
		t.Errorf("expected at least one container card")
	}
}

func TestServePod_NotFoundAndBadRequest(t *testing.T) {
	h := newFleetTestHandler(t)
	if code := getJSONInto(t, h, h.servePod, "/v2/api/pod", "?ns=default&name=ghost", nil); code != http.StatusNotFound {
		t.Errorf("missing pod: status = %d, want 404", code)
	}
	if code := getJSONInto(t, h, h.servePod, "/v2/api/pod", "?name=web", nil); code != http.StatusBadRequest {
		t.Errorf("missing ns: status = %d, want 400", code)
	}
}

func TestServeNamespace(t *testing.T) {
	h := newFleetTestHandler(t)
	var d NamespaceDetail
	if code := getJSONInto(t, h, h.serveNamespace, "/v2/api/namespace", "?ns=default", &d); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if d.Name != "default" {
		t.Errorf("Name = %q, want default", d.Name)
	}
	if d.KPIs.Pods != 2 {
		t.Errorf("KPIs.Pods = %d, want 2", d.KPIs.Pods)
	}
	if d.KPIs.UnhealthyPods != 1 {
		t.Errorf("KPIs.UnhealthyPods = %d, want 1", d.KPIs.UnhealthyPods)
	}
	if len(d.Pods) != 2 {
		t.Errorf("len(Pods) = %d, want 2", len(d.Pods))
	}
	if len(d.Workloads) == 0 {
		t.Errorf("expected at least one workload row")
	}
}

func TestServeOverview(t *testing.T) {
	h := newFleetTestHandler(t)
	var ov Overview
	if code := getJSONInto(t, h, h.serveOverview, "/v2/api/overview", "", &ov); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if ov.KPIs.Pods != 2 {
		t.Errorf("KPIs.Pods = %d, want 2", ov.KPIs.Pods)
	}
	if ov.KPIs.Workloads != 1 {
		t.Errorf("KPIs.Workloads = %d, want 1", ov.KPIs.Workloads)
	}
	if ov.KPIs.UnhealthyPods != 1 {
		t.Errorf("KPIs.UnhealthyPods = %d, want 1", ov.KPIs.UnhealthyPods)
	}
	if ov.KPIs.CrashLoopBackOff != 1 {
		t.Errorf("KPIs.CrashLoopBackOff = %d, want 1", ov.KPIs.CrashLoopBackOff)
	}
}
