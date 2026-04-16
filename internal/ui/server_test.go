package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
	"github.com/phenixblue/k8shark/internal/transitions"
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

func TestServeDetail_ItemFromWatchOnlyAddedResource(t *testing.T) {
	store := buildWatchOnlyStore(t)
	h := &explorerHandler{store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/ui/detail?path=/api/v1/namespaces/default/pods&name=redis&at=2026-04-10T10:00:31Z", nil)
	rr := httptest.NewRecorder()
	h.serveDetail(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	jsonBody, _ := body["json"].(string)
	if !strings.Contains(jsonBody, "redis") {
		t.Fatalf("expected redis in detail response, got %s", jsonBody)
	}
}

func TestBuildTree_IncludesWatchOnlyAddedResource(t *testing.T) {
	store := buildWatchOnlyStore(t)
	h := &explorerHandler{store: store}
	tree, err := h.buildTreeAt(time.Date(2026, 4, 10, 10, 0, 31, 0, time.UTC))
	if err != nil {
		t.Fatalf("buildTreeAt: %v", err)
	}
	if len(tree.Namespaces) != 1 || tree.Namespaces[0].Name != "default" {
		t.Fatalf("expected one default namespace, got %+v", tree.Namespaces)
	}
	ns := tree.Namespaces[0]
	if len(ns.Pods) != 1 || ns.Pods[0].Name != "redis" {
		t.Fatalf("expected watch-only pod to appear, got %+v", ns.Pods)
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
			APIPath: path,
			Seqs:    []int{0},
			Times:   []time.Time{ts},
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

// buildStoreFromArchive writes records via StreamWriter, opens the archive, and
// returns a CaptureStore. wIdx may be nil for archives without watch data.
func buildStoreFromArchive(t *testing.T, out string, recs []*capture.Record, idx capture.Index, wIdx capture.WatchIndex, meta *capture.CaptureMetadata) *server.CaptureStore {
	t.Helper()
	sw, err := archive.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	for _, r := range recs {
		if err := sw.WriteRecord(r); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
	}
	var wi any
	if len(wIdx) > 0 {
		wi = wIdx
	}
	if err := sw.Finish(meta, idx, wi); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ar, err := archive.Open(out)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })
	store, err := server.LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
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
		"/apis/apps/v1/namespaces/default/deployments": {APIPath: "/apis/apps/v1/namespaces/default/deployments", Seqs: []int{0}, Times: []time.Time{now}},
		"/api/v1/namespaces/default/pods":              {APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}, Times: []time.Time{now}},
		"/api/v1/nodes":                                {APIPath: "/api/v1/nodes", Seqs: []int{0}, Times: []time.Time{now}},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-test", CapturedAt: now.Add(-5 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	return buildStoreFromArchive(t, out, recs, idx, nil, meta)
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
			APIPath: "/api/v1/namespaces/default/configmaps?labelSelector=app%3Ddemo",
			Seqs:    []int{0},
			Times:   []time.Time{now},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-query-test", CapturedAt: now.Add(-2 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	return buildStoreFromArchive(t, out, recs, idx, nil, meta)
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
			APIPath: "/api/v1/namespaces/default/pods",
			Seqs:    []int{0},
			Times:   []time.Time{now},
		},
		"/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo": {
			APIPath: "/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo",
			Seqs:    []int{0},
			Times:   []time.Time{now},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-preferred-query-test", CapturedAt: now.Add(-2 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	return buildStoreFromArchive(t, out, recs, idx, nil, meta)
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
			APIPath: "/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo-a",
			Seqs:    []int{0},
			Times:   []time.Time{now},
		},
		"/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo-b": {
			APIPath: "/api/v1/namespaces/default/pods?labelSelector=app%3Ddemo-b",
			Seqs:    []int{0},
			Times:   []time.Time{now},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-merged-query-test", CapturedAt: now.Add(-2 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	return buildStoreFromArchive(t, out, recs, idx, nil, meta)
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
			APIPath: "/api/v1/namespaces/default/configmaps/demo-config",
			Seqs:    []int{0},
			Times:   []time.Time{now},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-item-only-test", CapturedAt: now.Add(-2 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	return buildStoreFromArchive(t, out, recs, idx, nil, meta)
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
			APIPath: "/api/v1/namespaces/default/configmaps?as=Table",
			Seqs:    []int{0},
			Times:   []time.Time{now},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-table-only-test", CapturedAt: now.Add(-2 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}
	return buildStoreFromArchive(t, out, recs, idx, nil, meta)
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
			APIPath: "/api/v1/namespaces/default/pods",
			Seqs:    []int{0, 1},
			Times:   []time.Time{t1, t2},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-multi-snapshot-test", CapturedAt: t1, CapturedUntil: t2, RecordCount: len(recs)}
	return buildStoreFromArchive(t, out, recs, idx, nil, meta)
}

func buildWatchOnlyStore(t *testing.T) *server.CaptureStore {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "capture-watch-only.tar.gz")

	t0 := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)
	watchRec := &capture.Record{
		ID:           "watch-1",
		CapturedAt:   t1,
		APIPath:      "/api/v1/namespaces/default/pods",
		EventType:    "ADDED",
		HTTPMethod:   http.MethodGet,
		ResponseCode: http.StatusOK,
		ResponseBody: json.RawMessage(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"redis","namespace":"default","uid":"pod-redis"},"spec":{"containers":[{"name":"main"}]},"status":{"phase":"Running"}}`),
	}

	recs := []*capture.Record{watchRec}
	idx := capture.Index{
		"/api/v1/namespaces/default/pods": {
			APIPath: "/api/v1/namespaces/default/pods",
			Seqs:    []int{},
			Times:   []time.Time{},
		},
	}
	wi := capture.WatchIndex{
		"/api/v1/namespaces/default/pods": {
			APIPath:    "/api/v1/namespaces/default/pods",
			Seqs:       []int{0},
			Times:      []time.Time{t1},
			EventTypes: []string{"ADDED"},
		},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-watch-only-test", CapturedAt: t0, CapturedUntil: t1.Add(time.Second), RecordCount: 1}
	return buildStoreFromArchive(t, out, recs, idx, wi, meta)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestServeTransitions(t *testing.T) {
	store := buildTestStore(t)
	h := &explorerHandler{
		store: store,
		allTransitions: []transitions.Transition{
			{Time: time.Date(2026, 4, 10, 10, 0, 5, 0, time.UTC), EventType: "ADDED", Resource: "pods", Namespace: "default", Name: "nginx"},
			{Time: time.Date(2026, 4, 10, 10, 0, 10, 0, time.UTC), EventType: "MODIFIED", Resource: "pods", Namespace: "default", Name: "nginx"},
			{Time: time.Date(2026, 4, 10, 10, 0, 15, 0, time.UTC), EventType: "ADDED", Resource: "deployments", Namespace: "kube-system", Name: "coredns"},
		},
	}

	// Unfiltered — returns all 3.
	req := httptest.NewRequest(http.MethodGet, "/api/ui/transitions", nil)
	rr := httptest.NewRecorder()
	h.serveTransitions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var markers []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &markers); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(markers) != 3 {
		t.Fatalf("expected 3 markers, got %d", len(markers))
	}

	// Filter by resource.
	req2 := httptest.NewRequest(http.MethodGet, "/api/ui/transitions?resource=pods", nil)
	rr2 := httptest.NewRecorder()
	h.serveTransitions(rr2, req2)
	var filtered []map[string]any
	_ = json.Unmarshal(rr2.Body.Bytes(), &filtered)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 pod markers, got %d", len(filtered))
	}

	// Filter by namespace.
	req3 := httptest.NewRequest(http.MethodGet, "/api/ui/transitions?namespace=kube-system", nil)
	rr3 := httptest.NewRecorder()
	h.serveTransitions(rr3, req3)
	var nsFiltered []map[string]any
	_ = json.Unmarshal(rr3.Body.Bytes(), &nsFiltered)
	if len(nsFiltered) != 1 {
		t.Fatalf("expected 1 kube-system marker, got %d", len(nsFiltered))
	}
}

func TestServeObjectHistory(t *testing.T) {
	store := buildTestStore(t)
	h := &explorerHandler{
		store: store,
		allTransitions: []transitions.Transition{
			{Time: time.Date(2026, 4, 10, 10, 0, 5, 0, time.UTC), EventType: "ADDED", Resource: "pods", Namespace: "default", Name: "nginx", After: json.RawMessage(`{"metadata":{"name":"nginx"}}`)},
			{Time: time.Date(2026, 4, 10, 10, 0, 10, 0, time.UTC), EventType: "MODIFIED", Resource: "pods", Namespace: "default", Name: "nginx", Before: json.RawMessage(`{"metadata":{"name":"nginx"}}`), After: json.RawMessage(`{"metadata":{"name":"nginx"},"status":{"phase":"Running"}}`)},
			{Time: time.Date(2026, 4, 10, 10, 0, 15, 0, time.UTC), EventType: "ADDED", Resource: "pods", Namespace: "default", Name: "redis"},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/ui/object-history?name=nginx", nil)
	rr := httptest.NewRecorder()
	h.serveObjectHistory(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var entries []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 history entries for nginx, got %d", len(entries))
	}
	if entries[0]["event_type"] != "ADDED" || entries[1]["event_type"] != "MODIFIED" {
		t.Errorf("unexpected event types: %v, %v", entries[0]["event_type"], entries[1]["event_type"])
	}
	// MODIFIED entry should have before and after.
	if entries[1]["before"] == nil || entries[1]["after"] == nil {
		t.Errorf("expected before/after on MODIFIED entry")
	}
}

func TestServeObjectHistory_MissingName(t *testing.T) {
	store := buildTestStore(t)
	h := &explorerHandler{store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/ui/object-history", nil)
	rr := httptest.NewRecorder()
	h.serveObjectHistory(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
}
