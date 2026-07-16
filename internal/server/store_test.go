package server

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

func TestParseAPIPath(t *testing.T) {
	type tc struct {
		path      string
		group     string
		version   string
		resource  string
		namespace string
	}
	cases := []tc{
		{"/api/v1/pods", "", "v1", "pods", ""},
		{"/api/v1/namespaces/default/pods", "", "v1", "pods", "default"},
		{"/api/v1/namespaces/kube-system/configmaps", "", "v1", "configmaps", "kube-system"},
		{"/apis/apps/v1/deployments", "apps", "v1", "deployments", ""},
		{"/apis/apps/v1/namespaces/default/deployments", "apps", "v1", "deployments", "default"},
		{"/apis/batch/v1/namespaces/ci/jobs", "batch", "v1", "jobs", "ci"},
	}
	for _, tc := range cases {
		g, v, r, ns := parseAPIPath(tc.path)
		if g != tc.group || v != tc.version || r != tc.resource || ns != tc.namespace {
			t.Errorf("parseAPIPath(%q): got (%q,%q,%q,%q), want (%q,%q,%q,%q)",
				tc.path, g, v, r, ns, tc.group, tc.version, tc.resource, tc.namespace)
		}
	}
}

func TestResourceToKind(t *testing.T) {
	cases := map[string]string{
		"pods":                              "Pod",
		"deployments":                       "Deployment",
		"configmaps":                        "ConfigMap",
		"services":                          "Service",
		"endpointslices":                    "EndpointSlice", // naive singularize+capitalize would give "Endpointslice"
		"ipaddresses":                       "IPAddress",
		"csidrivers":                        "CSIDriver",
		"csinodes":                          "CSINode",
		"csistoragecapacities":              "CSIStorageCapacity", // irregular plural too ("capacitie")
		"priorityclasses":                   "PriorityClass",
		"validatingadmissionpolicies":       "ValidatingAdmissionPolicy", // irregular plural ("policie")
		"validatingadmissionpolicybindings": "ValidatingAdmissionPolicyBinding",
		"mutatingwebhookconfigurations":     "MutatingWebhookConfiguration",
		"validatingwebhookconfigurations":   "ValidatingWebhookConfiguration",
		"resourceclaimtemplates":            "ResourceClaimTemplate",
		"servicecidrs":                      "ServiceCIDR",
		"flowschemas":                       "FlowSchema",
		"prioritylevelconfigurations":       "PriorityLevelConfiguration",
		"certificatesigningrequests":        "CertificateSigningRequest",
		"customresourcedefinitions":         "CustomResourceDefinition", // aggregated API group, not in client-go scheme
		"apiservices":                       "APIService",               // aggregated API group, not in client-go scheme
		"widgets":                           "Widget",                   // unknown resource: naive fallback
	}
	for resource, want := range cases {
		if got := resourceToKind(resource); got != want {
			t.Errorf("resourceToKind(%q) = %q, want %q", resource, got, want)
		}
	}
}

// TestEnrichResourceInfoFromDiscovery_KnownButUncaptured verifies a resource
// listed in a captured discovery document, but with zero captured objects of
// its own, still gets a ResourceInfo entry — the root fix for #177 ("known
// kind, nothing captured" must be distinguishable from "genuinely unknown
// kind").
func TestEnrichResourceInfoFromDiscovery_KnownButUncaptured(t *testing.T) {
	discoveryBody := `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"networking.k8s.io/v1","resources":[` +
		`{"name":"ingresses","singularName":"ingress","namespaced":true,"kind":"Ingress","shortNames":["ing"]}]}`
	store := buildTestStore(t, map[string][]byte{
		"/apis/networking.k8s.io/v1": []byte(discoveryBody),
	})
	store.discoveryEnrichmentDone.Wait()

	if !store.isKnownResource("networking.k8s.io", "v1", "ingresses") {
		t.Fatal("ingresses should be a known resource after discovery enrichment")
	}
	resources := store.Resources()
	var found *ResourceInfo
	for i := range resources {
		if resources[i].Group == "networking.k8s.io" && resources[i].Resource == "ingresses" {
			found = &resources[i]
		}
	}
	if found == nil {
		t.Fatal("ingresses missing from Resources()")
	}
	if found.Kind != "Ingress" || found.SingularName != "ingress" || !found.Namespaced {
		t.Errorf("ingresses ResourceInfo = %+v, want Kind=Ingress SingularName=ingress Namespaced=true", found)
	}
	if len(found.ShortNames) != 1 || found.ShortNames[0] != "ing" {
		t.Errorf("ingresses ShortNames = %v, want [ing]", found.ShortNames)
	}

	// Mutating the returned ShortNames must not corrupt the store's internal
	// state — Resources() should be a fully decoupled snapshot, not just a
	// copy of the outer struct sharing the slice's backing array.
	found.ShortNames[0] = "mutated"
	resources2 := store.Resources()
	for i := range resources2 {
		if resources2[i].Group == "networking.k8s.io" && resources2[i].Resource == "ingresses" {
			if resources2[i].ShortNames[0] != "ing" {
				t.Errorf("mutating a returned ShortNames slice corrupted internal state: got %v", resources2[i].ShortNames)
			}
		}
	}
}

// TestEnrichResourceInfoFromDiscovery_CorrectsNamespacedFromIndex verifies
// discovery's "namespaced" field overwrites buildResourceInfo's index-derived
// guess on an existing entry, not just at creation — a capture with only a
// cluster-wide list (no namespaces: in config) would otherwise report a
// namespaced resource as cluster-scoped in the regenerated discovery
// document.
func TestEnrichResourceInfoFromDiscovery_CorrectsNamespacedFromIndex(t *testing.T) {
	discoveryBody := `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[` +
		`{"name":"pods","singularName":"pod","namespaced":true,"kind":"Pod"}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1":      []byte(discoveryBody),
		"/api/v1/pods": []byte(`{"kind":"PodList","items":[]}`), // cluster-wide only — buildResourceInfo infers Namespaced=false
	})
	store.discoveryEnrichmentDone.Wait()

	resources := store.Resources()
	var found *ResourceInfo
	for i := range resources {
		if resources[i].Group == "" && resources[i].Resource == "pods" {
			found = &resources[i]
		}
	}
	if found == nil {
		t.Fatal("pods missing from Resources()")
	}
	if !found.Namespaced {
		t.Error("discovery says pods is namespaced; enrichment should have corrected the index-derived guess")
	}
}

// buildTestStore creates a CaptureStore with the given per-path response bodies.
func buildTestStore(t *testing.T, records map[string][]byte) *CaptureStore {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test.kshrk")

	sw, err := archive.NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}

	index := make(capture.Index)
	for apiPath, body := range records {
		now := time.Now().UTC()
		rec := capture.Record{
			ID:           "rec-" + apiPath,
			CapturedAt:   now,
			APIPath:      apiPath,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(body),
		}
		if err := sw.WriteRecord(&rec); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
		index[apiPath] = &capture.IndexEntry{
			APIPath: apiPath,
			Seqs:    []int{0},
			Times:   []time.Time{now},
		}
	}

	meta := capture.CaptureMetadata{
		CaptureID:         "test-capture-id",
		KubernetesVersion: "v1.29.0",
		CapturedAt:        time.Now().UTC().Add(-time.Minute),
		CapturedUntil:     time.Now().UTC(),
		RecordCount:       len(records),
	}
	if err := sw.Finish(&meta, index, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	ar, err := archive.Open(outPath)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })

	store, err := LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}

// TestStore_NamespaceItemCountsAt verifies that NamespaceItemCountsAt reads
// IndexEntry.Counts (no body reads) and returns the correct per-(ns,
// resource) totals, picking the latest record at or before the requested
// time. Older archives whose IndexEntry omits Counts contribute nothing.
func TestStore_NamespaceItemCountsAt(t *testing.T) {
	t0 := time.Now().UTC()
	t1 := t0.Add(30 * time.Second)
	t2 := t0.Add(60 * time.Second)

	store := &CaptureStore{
		Index: capture.Index{
			// Two records at different times — only the latest before `at` counts.
			"/api/v1/namespaces/team-a/pods": {
				APIPath: "/api/v1/namespaces/team-a/pods",
				Seqs:    []int{0, 1},
				Times:   []time.Time{t0, t2},
				Counts:  []int{3, 5},
			},
			"/apis/apps/v1/namespaces/team-a/deployments": {
				APIPath: "/apis/apps/v1/namespaces/team-a/deployments",
				Seqs:    []int{0},
				Times:   []time.Time{t1},
				Counts:  []int{2},
			},
			"/apis/kubevirt.io/v1/namespaces/team-b/virtualmachines": {
				APIPath: "/apis/kubevirt.io/v1/namespaces/team-b/virtualmachines",
				Seqs:    []int{0},
				Times:   []time.Time{t1},
				Counts:  []int{4},
			},
			// Older archive: no Counts → must be skipped, not counted as 0.
			"/api/v1/namespaces/team-c/configmaps": {
				APIPath: "/api/v1/namespaces/team-c/configmaps",
				Seqs:    []int{0},
				Times:   []time.Time{t1},
			},
			// Cluster-scoped — must not appear in the per-namespace map.
			"/api/v1/nodes": {
				APIPath: "/api/v1/nodes",
				Seqs:    []int{0},
				Times:   []time.Time{t1},
				Counts:  []int{6},
			},
			// Table records — same data as plain LIST; must not double-count.
			"/api/v1/namespaces/team-a/pods?as=Table": {
				APIPath: "/api/v1/namespaces/team-a/pods?as=Table",
				Seqs:    []int{0},
				Times:   []time.Time{t2},
				Counts:  []int{5},
			},
		},
	}

	// At t2: pods/team-a uses the t2 record (5), deployments/team-a still 2,
	// virtualmachines/team-b 4. team-c contributes nothing (no Counts).
	got := store.NamespaceItemCountsAt(t2)
	wantTeamA := map[string]int{"pods": 5, "deployments": 2}
	wantTeamB := map[string]int{"virtualmachines": 4}
	if !reflect.DeepEqual(got["team-a"], wantTeamA) {
		t.Errorf("team-a = %v, want %v", got["team-a"], wantTeamA)
	}
	if !reflect.DeepEqual(got["team-b"], wantTeamB) {
		t.Errorf("team-b = %v, want %v", got["team-b"], wantTeamB)
	}
	if _, ok := got["team-c"]; ok {
		t.Errorf("team-c should be absent (no Counts in archive), got %v", got["team-c"])
	}

	// At t1 (between t0 and t2): pods/team-a uses the t0 record (3).
	got = store.NamespaceItemCountsAt(t1)
	if got["team-a"]["pods"] != 3 {
		t.Errorf("team-a pods at t1 = %d, want 3", got["team-a"]["pods"])
	}

	// At t0 (before deployments was recorded): only pods/team-a present.
	got = store.NamespaceItemCountsAt(t0)
	if got["team-a"]["pods"] != 3 {
		t.Errorf("team-a pods at t0 = %d, want 3", got["team-a"]["pods"])
	}
	if _, ok := got["team-a"]["deployments"]; ok {
		t.Error("deployments should not be present at t0 (recorded later)")
	}
}

func TestStore_Latest_Found(t *testing.T) {
	podList := `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"nginx"}}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(podList),
	})
	body, code, err := store.Latest("/api/v1/namespaces/default/pods", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	if result["kind"] != "PodList" {
		t.Errorf("expected kind=PodList, got %v", result["kind"])
	}
}

func TestStore_Latest_NotFound(t *testing.T) {
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(`{"kind":"PodList","items":[]}`),
	})
	_, code, err := store.Latest("/api/v1/namespaces/default/services", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 404 {
		t.Errorf("expected 404, got %d", code)
	}
}

func TestStore_Latest_AtTimestamp(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test.kshrk")

	path := "/api/v1/namespaces/default/pods"
	t1 := time.Date(2026, 4, 9, 10, 40, 0, 0, time.UTC)
	t2 := t1.Add(2 * time.Minute)
	records := []capture.Record{
		{ID: "rec-1", CapturedAt: t1, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(`{"kind":"PodList","items":[{"metadata":{"name":"before"}}]}`)},
		{ID: "rec-2", CapturedAt: t2, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(`{"kind":"PodList","items":[{"metadata":{"name":"after"}}]}`)},
	}

	sw, err := archive.NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	for _, rec := range records {
		rcopy := rec
		if err := sw.WriteRecord(&rcopy); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
	}
	idx := capture.Index{
		path: {
			APIPath: path,
			Seqs:    []int{0, 1},
			Times:   []time.Time{t1, t2},
		},
	}
	meta := capture.CaptureMetadata{
		CaptureID:     "test-capture-id",
		CapturedAt:    t1,
		CapturedUntil: t2,
		RecordCount:   len(records),
	}
	if err := sw.Finish(&meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	ar, err := archive.Open(outPath)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })

	store, err := LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}

	body, code, err := store.Latest(path, t1.Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if string(body) == "" || !containsString(string(body), "before") {
		t.Fatalf("expected first record body, got %s", string(body))
	}

	body, code, err = store.Latest(path, t2.Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !containsString(string(body), "after") {
		t.Fatalf("expected second record body, got %s", string(body))
	}

	_, code, err = store.Latest(path, t1.Add(-time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 404 {
		t.Fatalf("expected 404 before first record, got %d", code)
	}
}

func containsString(s, sub string) bool { return strings.Contains(s, sub) }

// buildTestStoreWithWatch creates a CaptureStore with snapshot records in
// index.json and watch event records in watch-index.json.
func buildTestStoreWithWatch(t *testing.T, snapshots map[string]watchTestRecord, events []watchTestEvent) *CaptureStore {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test.kshrk")

	sw, err := archive.NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}

	index := make(capture.Index)
	watchIndex := make(capture.WatchIndex)

	for apiPath, s := range snapshots {
		rec := capture.Record{
			ID:           s.id,
			CapturedAt:   s.at,
			APIPath:      apiPath,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(s.body),
		}
		if err := sw.WriteRecord(&rec); err != nil {
			t.Fatalf("WriteRecord(snap): %v", err)
		}
		index[apiPath] = &capture.IndexEntry{
			APIPath: apiPath,
			Seqs:    []int{0},
			Times:   []time.Time{s.at},
		}
	}

	// Track per-path seq counter for ALL records (snap + watch share path namespace).
	allSeq := map[string]int{}
	for apiPath := range snapshots {
		allSeq[apiPath] = 1 // snapshot already wrote seq=0
	}
	for _, ev := range events {
		rec := capture.Record{
			ID:           ev.id,
			CapturedAt:   ev.at,
			APIPath:      ev.apiPath,
			EventType:    ev.eventType,
			HTTPMethod:   "GET",
			ResponseCode: 200,
			ResponseBody: json.RawMessage(ev.objectBody),
		}
		if err := sw.WriteRecord(&rec); err != nil {
			t.Fatalf("WriteRecord(watch %s): %v", ev.id, err)
		}
		wi := watchIndex[ev.apiPath]
		if wi == nil {
			wi = &capture.WatchIndexEntry{APIPath: ev.apiPath}
			watchIndex[ev.apiPath] = wi
		}
		seq := allSeq[ev.apiPath]
		allSeq[ev.apiPath] = seq + 1
		wi.Seqs = append(wi.Seqs, seq)
		wi.Times = append(wi.Times, ev.at)
		wi.EventTypes = append(wi.EventTypes, ev.eventType)
	}

	meta := capture.CaptureMetadata{
		CaptureID: "test-watch-id", KubernetesVersion: "v1.29.0",
		CapturedAt: time.Now().UTC().Add(-time.Minute), CapturedUntil: time.Now().UTC(),
	}
	var wi any
	if len(watchIndex) > 0 {
		wi = watchIndex
	}
	if err := sw.Finish(&meta, index, wi); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	ar, err := archive.Open(outPath)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })

	store, err := LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}

type watchTestRecord struct {
	id   string
	at   time.Time
	body string
}
type watchTestEvent struct {
	id         string
	apiPath    string
	at         time.Time
	eventType  string
	objectBody string
}

func TestStore_ReconstructAt_NoWatchEvents_FallsBackToLatest(t *testing.T) {
	snapBody := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(snapBody),
	})
	body, code, err := store.ReconstructAt("/api/v1/namespaces/default/pods", time.Time{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !containsString(string(body), "nginx") {
		t.Errorf("expected nginx in result, got %s", string(body))
	}
}

func TestStore_ReconstructAt_Added(t *testing.T) {
	path := "/api/v1/namespaces/default/pods"
	t0 := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)

	snap := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	newPod := `{"metadata":{"name":"redis","namespace":"default"},"spec":{}}`

	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			path: {id: "snap-1", at: t0, body: snap},
		},
		[]watchTestEvent{
			{id: "ev-1", apiPath: path, at: t1, eventType: "ADDED", objectBody: newPod},
		},
	)

	body, code, err := store.ReconstructAt(path, t1.Add(time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if !containsString(string(body), "nginx") {
		t.Errorf("expected nginx still present, got %s", string(body))
	}
	if !containsString(string(body), "redis") {
		t.Errorf("expected redis added, got %s", string(body))
	}
}

func TestStore_ReconstructAt_WatchOnlyPath(t *testing.T) {
	path := "/api/v1/namespaces/default/pods"
	t0 := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)

	newPod := `{"metadata":{"name":"redis","namespace":"default"},"spec":{}}`

	store := buildTestStoreWithWatch(t, nil, []watchTestEvent{
		{id: "ev-1", apiPath: path, at: t1, eventType: "ADDED", objectBody: newPod},
	})

	body, code, err := store.ReconstructAt(path, t0.Add(5*time.Second))
	if err != nil {
		t.Fatalf("unexpected error before event: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200 before event, got %d", code)
	}
	if containsString(string(body), "redis") {
		t.Fatalf("did not expect redis before event, got %s", string(body))
	}

	body, code, err = store.ReconstructAt(path, t1.Add(time.Second))
	if err != nil {
		t.Fatalf("unexpected error after event: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200 after event, got %d", code)
	}
	if !containsString(string(body), "redis") {
		t.Fatalf("expected redis after watch-only add, got %s", string(body))
	}
}

func TestStore_ReconstructAt_Modified(t *testing.T) {
	path := "/api/v1/namespaces/default/pods"
	t0 := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)

	snap := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"},"status":{"phase":"Pending"}}]}`
	modifiedPod := `{"metadata":{"name":"nginx","namespace":"default"},"status":{"phase":"Running"}}`

	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			path: {id: "snap-1", at: t0, body: snap},
		},
		[]watchTestEvent{
			{id: "ev-1", apiPath: path, at: t1, eventType: "MODIFIED", objectBody: modifiedPod},
		},
	)

	body, code, err := store.ReconstructAt(path, t1.Add(time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if containsString(string(body), "Pending") {
		t.Errorf("Pending phase should have been replaced, got %s", string(body))
	}
	if !containsString(string(body), "Running") {
		t.Errorf("expected Running phase after MODIFIED, got %s", string(body))
	}
}

func TestStore_ReconstructAt_Deleted(t *testing.T) {
	path := "/api/v1/namespaces/default/pods"
	t0 := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)

	snap := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}},{"metadata":{"name":"redis","namespace":"default"}}]}`
	deletedPod := `{"metadata":{"name":"nginx","namespace":"default"}}`

	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			path: {id: "snap-1", at: t0, body: snap},
		},
		[]watchTestEvent{
			{id: "ev-1", apiPath: path, at: t1, eventType: "DELETED", objectBody: deletedPod},
		},
	)

	body, code, err := store.ReconstructAt(path, t1.Add(time.Second))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if containsString(string(body), "nginx") {
		t.Errorf("nginx should have been deleted, got %s", string(body))
	}
	if !containsString(string(body), "redis") {
		t.Errorf("redis should still be present, got %s", string(body))
	}
}

func TestStore_ReconstructAt_EventBeforeSnapshot_Ignored(t *testing.T) {
	path := "/api/v1/namespaces/default/pods"
	t0 := time.Date(2026, 4, 14, 10, 0, 0, 0, time.UTC)
	// Event is before the snapshot — should not be applied.
	tBefore := t0.Add(-10 * time.Second)

	snap := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	stalePod := `{"metadata":{"name":"ghost","namespace":"default"}}`

	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{
			path: {id: "snap-1", at: t0, body: snap},
		},
		[]watchTestEvent{
			{id: "ev-1", apiPath: path, at: tBefore, eventType: "ADDED", objectBody: stalePod},
		},
	)

	body, code, err := store.ReconstructAt(path, t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected 200, got %d", code)
	}
	if containsString(string(body), "ghost") {
		t.Errorf("event before snapshot should be ignored, got %s", string(body))
	}
}

func TestStore_ReconstructAt_OldArchiveNoWatchIndex(t *testing.T) {
	// Old archives without watch-index.json must load and serve correctly.
	snapBody := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/default/pods": []byte(snapBody),
	})
	// WatchIndex should be empty (not nil).
	if store.WatchIndex == nil {
		t.Fatal("WatchIndex should be initialized to empty map, not nil")
	}
	body, code, err := store.ReconstructAt("/api/v1/namespaces/default/pods", time.Time{})
	if err != nil || code != 200 {
		t.Fatalf("old archive must serve via Latest fallback: code=%d err=%v", code, err)
	}
	if !containsString(string(body), "nginx") {
		t.Errorf("expected nginx, got %s", string(body))
	}
}
