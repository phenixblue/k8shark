package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestServeTimestamps(t *testing.T) {
	store := buildMultiSnapshotStore(t)
	h := &explorerHandler{store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/ui/timestamps", nil)
	rr := httptest.NewRecorder()
	h.serveTimestamps(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	raw, ok := body["timestamps"].([]any)
	if !ok {
		t.Fatalf("expected timestamps array, got %T", body["timestamps"])
	}
	if len(raw) != 2 {
		t.Fatalf("expected 2 timestamps, got %d", len(raw))
	}
	if total, ok := body["total_count"].(float64); !ok || int(total) != 2 {
		t.Fatalf("expected total_count=2, got %#v", body["total_count"])
	}
	if sampled, ok := body["sampled"].(bool); !ok || sampled {
		t.Fatalf("expected sampled=false, got %#v", body["sampled"])
	}
	first, _ := raw[0].(string)
	second, _ := raw[1].(string)
	if !(strings.HasSuffix(first, "Z") && strings.HasSuffix(second, "Z")) {
		t.Fatalf("expected RFC3339 timestamps, got %q and %q", first, second)
	}
}

func TestCollectTimestamps_Sampled(t *testing.T) {
	base := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	index := capture.Index{}
	for i := 0; i < 20; i++ {
		path := "/api/v1/namespaces/default/pods?snapshot=" + time.Duration(i).String()
		ts := base.Add(time.Duration(i) * time.Minute)
		index[path] = &capture.IndexEntry{
			APIPath:   path,
			RecordIDs: []string{"r"},
			Times:     []time.Time{ts},
		}
	}

	timestamps, totalCount, sampled := collectTimestamps(index, 5)
	if totalCount != 20 {
		t.Fatalf("expected total_count=20, got %d", totalCount)
	}
	if !sampled {
		t.Fatal("expected sampled=true")
	}
	if len(timestamps) != 5 {
		t.Fatalf("expected 5 sampled timestamps, got %d", len(timestamps))
	}
	if timestamps[0] != base.Format(time.RFC3339) {
		t.Fatalf("expected first timestamp preserved, got %q", timestamps[0])
	}
	if timestamps[len(timestamps)-1] != base.Add(19*time.Minute).Format(time.RFC3339) {
		t.Fatalf("expected last timestamp preserved, got %q", timestamps[len(timestamps)-1])
	}
	for i := 1; i < len(timestamps); i++ {
		if timestamps[i] <= timestamps[i-1] {
			t.Fatalf("expected strictly increasing timestamps, got %v", timestamps)
		}
	}
}

func TestServeTree_AtOverride(t *testing.T) {
	store := buildMultiSnapshotStore(t)
	h := &explorerHandler{store: store}
	target := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC).Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, "/api/ui/tree?at="+target, nil)
	rr := httptest.NewRecorder()
	h.serveTree(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var tree treeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &tree); err != nil {
		t.Fatalf("unmarshal tree: %v", err)
	}
	if len(tree.Namespaces) != 1 || len(tree.Namespaces[0].Pods) != 1 {
		t.Fatalf("expected one namespace with one pod, got %+v", tree.Namespaces)
	}
	if tree.Namespaces[0].Pods[0].Name != "demo-old" {
		t.Fatalf("expected older pod at selected timestamp, got %q", tree.Namespaces[0].Pods[0].Name)
	}
}

func TestServeTree_AtInvalid(t *testing.T) {
	store := buildTestStore(t)
	h := &explorerHandler{store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/ui/tree?at=not-a-time", nil)
	rr := httptest.NewRecorder()
	h.serveTree(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
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

func buildMultiSnapshotStore(t *testing.T) *server.CaptureStore {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "capture-multi-snapshot.tar.gz")
	t1 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(5 * time.Minute)

	podListOld := `{"apiVersion":"v1","kind":"PodList","items":[{"apiVersion":"v1","kind":"Pod","metadata":{"name":"demo-old","namespace":"default","uid":"pod-old"},"spec":{"containers":[{"name":"main"}]},"status":{"phase":"Running"}}]}`
	podListNew := `{"apiVersion":"v1","kind":"PodList","items":[{"apiVersion":"v1","kind":"Pod","metadata":{"name":"demo-new","namespace":"default","uid":"pod-new"},"spec":{"containers":[{"name":"main"}]},"status":{"phase":"Running"}}]}`

	recs := []*capture.Record{
		{ID: "s1", CapturedAt: t1, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podListOld)},
		{ID: "s2", CapturedAt: t2, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podListNew)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/pods": {
			APIPath:   "/api/v1/namespaces/default/pods",
			RecordIDs: []string{"s1", "s2"},
			Times:     []time.Time{t1, t2},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-multi-snapshot-test", CapturedAt: t1, CapturedUntil: t2, RecordCount: len(recs)}
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
