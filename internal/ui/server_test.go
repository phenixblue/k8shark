package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
)

func TestBuildTree_Hierarchy(t *testing.T) {
	store := buildTestStore(t)
	h := &explorerHandler{store: store}
	tree, err := h.buildTree()
	if err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	if len(tree.Namespaces) != 1 || tree.Namespaces[0].Name != "default" {
		t.Fatalf("expected one default namespace, got %+v", tree.Namespaces)
	}
	ns := tree.Namespaces[0]
	if len(ns.Workloads) == 0 {
		t.Fatal("expected workloads in namespace")
	}
	if len(ns.Workloads[0].Pods) == 0 {
		t.Fatal("expected pod attached to workload")
	}
	if len(ns.Workloads[0].Pods[0].Containers) == 0 {
		t.Fatal("expected containers in pod")
	}
}

func TestServeDetail_ItemFromList(t *testing.T) {
	store := buildTestStore(t)
	h := &explorerHandler{store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/ui/detail?path=/api/v1/namespaces/default/pods&name=demo-pod", nil)
	rr := httptest.NewRecorder()
	h.serveDetail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body["json"] == nil || body["yaml"] == nil {
		t.Fatalf("expected json and yaml in response, got %v", body)
	}
}

func buildTestStore(t *testing.T) *server.CaptureStore {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "capture.tar.gz")
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)

	deployList := `{"apiVersion":"apps/v1","kind":"DeploymentList","items":[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"demo-deploy","namespace":"default","labels":{"app":"demo"}}}]}`
	podList := `{"apiVersion":"v1","kind":"PodList","items":[{"apiVersion":"v1","kind":"Pod","metadata":{"name":"demo-pod","namespace":"default","labels":{"app":"demo"},"ownerReferences":[{"kind":"Deployment","name":"demo-deploy"}]},"spec":{"containers":[{"name":"main"},{"name":"sidecar"}]},"status":{"phase":"Running"}}]}`
	nodeList := `{"apiVersion":"v1","kind":"NodeList","items":[{"apiVersion":"v1","kind":"Node","metadata":{"name":"node-1"},"status":{"phase":"Ready"}}]}`

	recs := []*capture.Record{
		{ID: "r1", CapturedAt: now, APIPath: "/apis/apps/v1/namespaces/default/deployments", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(deployList)},
		{ID: "r2", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podList)},
		{ID: "r3", CapturedAt: now, APIPath: "/api/v1/nodes", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(nodeList)},
	}
	idx := capture.Index{
		"/apis/apps/v1/namespaces/default/deployments": {APIPath: "/apis/apps/v1/namespaces/default/deployments", RecordIDs: []string{"r1"}, Times: []time.Time{now}},
		"/api/v1/namespaces/default/pods":              {APIPath: "/api/v1/namespaces/default/pods", RecordIDs: []string{"r2"}, Times: []time.Time{now}},
		"/api/v1/nodes":                                {APIPath: "/api/v1/nodes", RecordIDs: []string{"r3"}, Times: []time.Time{now}},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-test", CapturedAt: now.Add(-5 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	if err := archive.Write(out, meta, recs, idx); err != nil {
		t.Fatalf("archive.Write: %v", err)
	}

	extractDir := filepath.Join(dir, "extract")
	if err := os.MkdirAll(extractDir, 0o750); err != nil {
		t.Fatalf("mkdir extract: %v", err)
	}
	if err := archive.Open(out, extractDir); err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	store, err := server.LoadStore(extractDir)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}
