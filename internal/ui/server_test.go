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

func TestBuildTree_IncludesQueryOnlyListPath(t *testing.T) {
	store := buildQueryOnlyStore(t)
	h := &explorerHandler{store: store}
	tree, err := h.buildTree()
	if err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	if len(tree.Namespaces) != 1 || tree.Namespaces[0].Name != "default" {
		t.Fatalf("expected one default namespace, got %+v", tree.Namespaces)
	}
	ns := tree.Namespaces[0]
	found := false
	for _, r := range ns.Resources {
		if r.Kind == "ConfigMap" && r.Name == "demo-config" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ConfigMap from query-only path, got resources: %+v", ns.Resources)
	}
}

func TestBuildTree_PrefersNonEmptyQueryListOverEmptyBaseList(t *testing.T) {
	store := buildPreferredQueryStore(t)
	h := &explorerHandler{store: store}
	tree, err := h.buildTree()
	if err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	if len(tree.Namespaces) != 1 || tree.Namespaces[0].Name != "default" {
		t.Fatalf("expected one default namespace, got %+v", tree.Namespaces)
	}
	ns := tree.Namespaces[0]
	if len(ns.Pods) != 1 || ns.Pods[0].Name != "demo-pod" {
		t.Fatalf("expected pod from non-empty query list, got %+v", ns.Pods)
	}
	if ns.Pods[0].ListPath != "/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo" {
		t.Fatalf("expected query path to back detail lookup, got %q", ns.Pods[0].ListPath)
	}
}

func TestBuildTree_MergesItemsAcrossCapturedQueryLists(t *testing.T) {
	store := buildMergedQueryStore(t)
	h := &explorerHandler{store: store}
	tree, err := h.buildTree()
	if err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	if len(tree.Namespaces) != 1 || tree.Namespaces[0].Name != "default" {
		t.Fatalf("expected one default namespace, got %+v", tree.Namespaces)
	}
	ns := tree.Namespaces[0]
	if len(ns.Pods) != 2 {
		t.Fatalf("expected two merged pods, got %+v", ns.Pods)
	}
	names := []string{ns.Pods[0].Name, ns.Pods[1].Name}
	if !(contains(names, "demo-a") && contains(names, "demo-b")) {
		t.Fatalf("expected merged pod names demo-a and demo-b, got %v", names)
	}
}

func TestBuildTree_IncludesItemOnlyPaths(t *testing.T) {
	store := buildItemOnlyStore(t)
	h := &explorerHandler{store: store}
	tree, err := h.buildTree()
	if err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	if len(tree.Namespaces) != 1 || tree.Namespaces[0].Name != "default" {
		t.Fatalf("expected one default namespace, got %+v", tree.Namespaces)
	}
	ns := tree.Namespaces[0]
	if len(ns.Resources) != 1 || ns.Resources[0].Name != "demo-config" {
		t.Fatalf("expected item-only ConfigMap to appear, got %+v", ns.Resources)
	}
	if ns.Resources[0].ListPath != "/api/v1/namespaces/default/configmaps/demo-config" {
		t.Fatalf("expected item path to be used for detail lookup, got %q", ns.Resources[0].ListPath)
	}
}

func TestBuildTree_IncludesTableRows(t *testing.T) {
	store := buildTableOnlyStore(t)
	h := &explorerHandler{store: store}
	tree, err := h.buildTree()
	if err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	if len(tree.Namespaces) != 1 || tree.Namespaces[0].Name != "default" {
		t.Fatalf("expected one default namespace, got %+v", tree.Namespaces)
	}
	ns := tree.Namespaces[0]
	if len(ns.Resources) != 1 || ns.Resources[0].Name != "table-config" {
		t.Fatalf("expected table row object to appear, got %+v", ns.Resources)
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

func buildQueryOnlyStore(t *testing.T) *server.CaptureStore {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "capture-query-only.tar.gz")
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)

	configMapList := `{"apiVersion":"v1","kind":"ConfigMapList","items":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"demo-config","namespace":"default","labels":{"app":"demo"}}}]}`

	recs := []*capture.Record{
		{ID: "q1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/configmaps?labelSelector=app%3Ddemo", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(configMapList)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/configmaps?labelSelector=app%3Ddemo": {
			APIPath:   "/api/v1/namespaces/default/configmaps?labelSelector=app%3Ddemo",
			RecordIDs: []string{"q1"},
			Times:     []time.Time{now},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-query-test", CapturedAt: now.Add(-2 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
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

func buildPreferredQueryStore(t *testing.T) *server.CaptureStore {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "capture-preferred-query.tar.gz")
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)

	emptyPodList := `{"apiVersion":"v1","kind":"PodList","items":[]}`
	queryPodList := `{"apiVersion":"v1","kind":"PodList","items":[{"apiVersion":"v1","kind":"Pod","metadata":{"name":"demo-pod","namespace":"default","labels":{"app":"demo"}},"spec":{"containers":[{"name":"main"}]},"status":{"phase":"Running"}}]}`

	recs := []*capture.Record{
		{ID: "p1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(emptyPodList)},
		{ID: "p2", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(queryPodList)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/pods": {
			APIPath:   "/api/v1/namespaces/default/pods",
			RecordIDs: []string{"p1"},
			Times:     []time.Time{now},
		},
		"/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo": {
			APIPath:   "/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo",
			RecordIDs: []string{"p2"},
			Times:     []time.Time{now},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-preferred-query-test", CapturedAt: now.Add(-2 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
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

func buildMergedQueryStore(t *testing.T) *server.CaptureStore {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "capture-merged-query.tar.gz")
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)

	queryA := `{"apiVersion":"v1","kind":"PodList","items":[{"apiVersion":"v1","kind":"Pod","metadata":{"name":"demo-a","namespace":"default","uid":"uid-a","labels":{"app":"demo-a"}},"spec":{"containers":[{"name":"main"}]},"status":{"phase":"Running"}}]}`
	queryB := `{"apiVersion":"v1","kind":"PodList","items":[{"apiVersion":"v1","kind":"Pod","metadata":{"name":"demo-b","namespace":"default","uid":"uid-b","labels":{"app":"demo-b"}},"spec":{"containers":[{"name":"main"}]},"status":{"phase":"Running"}}]}`

	recs := []*capture.Record{
		{ID: "m1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo-a", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(queryA)},
		{ID: "m2", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo-b", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(queryB)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo-a": {
			APIPath:   "/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo-a",
			RecordIDs: []string{"m1"},
			Times:     []time.Time{now},
		},
		"/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo-b": {
			APIPath:   "/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo-b",
			RecordIDs: []string{"m2"},
			Times:     []time.Time{now},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-merged-query-test", CapturedAt: now.Add(-2 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
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

func buildItemOnlyStore(t *testing.T) *server.CaptureStore {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "capture-item-only.tar.gz")
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)

	configMap := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"demo-config","namespace":"default","uid":"cfg-1","labels":{"app":"demo"}},"data":{"k":"v"}}`

	recs := []*capture.Record{
		{ID: "i1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/configmaps/demo-config", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(configMap)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/configmaps/demo-config": {
			APIPath:   "/api/v1/namespaces/default/configmaps/demo-config",
			RecordIDs: []string{"i1"},
			Times:     []time.Time{now},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-item-only-test", CapturedAt: now.Add(-2 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
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

func buildTableOnlyStore(t *testing.T) *server.CaptureStore {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "capture-table-only.tar.gz")
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)

	table := `{"apiVersion":"meta.k8s.io/v1","kind":"Table","columnDefinitions":[],"rows":[{"object":{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"table-config","namespace":"default","uid":"tbl-1"}}}]}`

	recs := []*capture.Record{
		{ID: "t1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/configmaps?as=Table", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(table)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/configmaps?as=Table": {
			APIPath:   "/api/v1/namespaces/default/configmaps?as=Table",
			RecordIDs: []string{"t1"},
			Times:     []time.Time{now},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-table-only-test", CapturedAt: now.Add(-2 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
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

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
