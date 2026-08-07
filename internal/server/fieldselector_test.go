package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fieldSelectorTestStore holds two pods on different nodes, in different
// phases — enough to tell "filtered correctly" apart from "matched everything".
func fieldSelectorTestStore(t *testing.T) *handler {
	t.Helper()
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/demo/pods": listWithPods([]podSpec{
			{name: "web-1", namespace: "demo", nodeName: "node-a", phase: "Running"},
			{name: "web-2", namespace: "demo", nodeName: "node-b", phase: "Failed"},
		}),
	})
	return newHandler(store, time.Time{}, false)
}

const fsPodsPath = "/api/v1/namespaces/demo/pods"

// TestHandler_FieldSelector_ReadPathFilters is the direct regression test for
// #339: on the read path a supported non-metadata key must filter, and must not
// degrade to matching every item.
func TestHandler_FieldSelector_ReadPathFilters(t *testing.T) {
	h := fieldSelectorTestStore(t)

	cases := []struct {
		query string
		want  []string
	}{
		{"metadata.name=web-1", []string{"web-1"}},
		{"spec.nodeName=node-a", []string{"web-1"}},
		{"spec.nodeName=node-b", []string{"web-2"}},
		// The case from the issue: a supported key with a value nothing
		// matches used to return every pod.
		{"spec.nodeName=does-not-exist", nil},
		{"status.phase=Failed", []string{"web-2"}},
		{"status.phase=Running", []string{"web-1"}},
		{"status.phase=Pending", nil},
		{"spec.nodeName=node-a,status.phase=Failed", nil},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, fsPodsPath+"?fieldSelector="+tc.query, nil)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != 200 {
			t.Errorf("[%s] status %d, want 200 (body: %s)", tc.query, rw.Code, rw.Body.String())
			continue
		}
		got := itemNames(t, rw.Body.Bytes())
		if !equalStrings(got, tc.want) {
			t.Errorf("[%s] got %v, want %v", tc.query, got, tc.want)
		}
	}
}

// TestHandler_FieldSelector_RejectsUnsupportedKey checks the 400. A real
// apiserver either supports a key or rejects it; it never over-matches.
func TestHandler_FieldSelector_RejectsUnsupportedKey(t *testing.T) {
	h := fieldSelectorTestStore(t)

	cases := []struct {
		query   string
		wantMsg string
	}{
		{"spec.bogus=x", "field label not supported: spec.bogus"},
		// Accepted for nodes, not for pods.
		{"spec.unschedulable=true", "field label not supported: spec.unschedulable"},
		// Valid for pods, but paired with an invalid one.
		{"spec.nodeName=node-a,spec.bogus=x", "field label not supported: spec.bogus"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, fsPodsPath+"?fieldSelector="+tc.query, nil)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusBadRequest {
			t.Errorf("[%s] status %d, want 400 (body: %s)", tc.query, rw.Code, rw.Body.String())
			continue
		}
		var status struct {
			Kind    string `json:"kind"`
			Status  string `json:"status"`
			Message string `json:"message"`
			Code    int    `json:"code"`
		}
		if err := json.Unmarshal(rw.Body.Bytes(), &status); err != nil {
			t.Errorf("[%s] body is not a Status: %v", tc.query, err)
			continue
		}
		if status.Kind != "Status" || status.Code != 400 {
			t.Errorf("[%s] body = %+v, want a Status with code 400", tc.query, status)
		}
		if !strings.Contains(status.Message, tc.wantMsg) {
			t.Errorf("[%s] message = %q, want it to contain %q", tc.query, status.Message, tc.wantMsg)
		}
	}
}

// TestHandler_FieldSelector_ItemGetIgnoresSelector checks we don't invent a 400
// where upstream has none: a single-object GET takes GetOptions, which carries
// no field selector, so the parameter is ignored rather than validated.
func TestHandler_FieldSelector_ItemGetIgnoresSelector(t *testing.T) {
	h := fieldSelectorTestStore(t)
	req := httptest.NewRequest(http.MethodGet, fsPodsPath+"/web-1?fieldSelector=spec.bogus=x", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Errorf("item GET with a bogus fieldSelector: status %d, want 200 (body: %s)",
			rw.Code, rw.Body.String())
	}
}

// TestHandler_FieldSelector_TablePathFilters covers the Table projection, which
// filters rows through a separate code path from the JSON list — `kubectl get`
// without -o hits this one.
func TestHandler_FieldSelector_TablePathFilters(t *testing.T) {
	h := fieldSelectorTestStore(t)

	for _, tc := range []struct {
		query string
		want  int
	}{
		{"spec.nodeName=node-a", 1},
		{"spec.nodeName=does-not-exist", 0},
	} {
		req := httptest.NewRequest(http.MethodGet, fsPodsPath+"?fieldSelector="+tc.query, nil)
		req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != 200 {
			t.Fatalf("[%s] status %d (body: %s)", tc.query, rw.Code, rw.Body.String())
		}
		var table struct {
			Kind string            `json:"kind"`
			Rows []json.RawMessage `json:"rows"`
		}
		if err := json.Unmarshal(rw.Body.Bytes(), &table); err != nil {
			t.Fatalf("[%s] decoding table: %v", tc.query, err)
		}
		if table.Kind != "Table" {
			t.Fatalf("[%s] kind = %q, want Table", tc.query, table.Kind)
		}
		if len(table.Rows) != tc.want {
			t.Errorf("[%s] %d rows, want %d", tc.query, len(table.Rows), tc.want)
		}
	}
}

// TestHandler_LabelSelectorOnly_NoFieldSelector covers every request shape that
// carries a labelSelector and no fieldSelector, which leaves the compiled
// *kstore.FieldSelector nil all the way down. The methods called on it
// (NeedsFullObject, Matches, Restricts) are nil-safe by design — a nil receiver
// means "matches everything" — and these exercise the JSON list, stored Table
// and watch paths to keep it that way.
func TestHandler_LabelSelectorOnly_NoFieldSelector(t *testing.T) {
	pods := []podSpec{
		{name: "web-1", namespace: "demo", nodeName: "node-a", phase: "Running",
			labels: map[string]string{"app": "web"}},
		{name: "db-1", namespace: "demo", nodeName: "node-b", phase: "Running",
			labels: map[string]string{"app": "db"}},
	}
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/demo/pods":          listWithPods(pods),
		"/api/v1/namespaces/demo/pods?as=Table": partialMetadataTable(pods),
	})
	h := newHandler(store, time.Time{}, false)

	t.Run("json list", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fsPodsPath+"?labelSelector=app%3Dweb", nil)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != 200 {
			t.Fatalf("status %d (body: %s)", rw.Code, rw.Body.String())
		}
		if got := itemNames(t, rw.Body.Bytes()); !equalStrings(got, []string{"web-1"}) {
			t.Errorf("got %v, want [web-1]", got)
		}
	})

	t.Run("stored table", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fsPodsPath+"?labelSelector=app%3Dweb", nil)
		req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != 200 {
			t.Fatalf("status %d (body: %s)", rw.Code, rw.Body.String())
		}
		if got := tableRowNames(t, rw.Body.Bytes()); !equalStrings(got, []string{"web-1"}) {
			t.Errorf("rows %v, want [web-1]", got)
		}
	})

	t.Run("watch", func(t *testing.T) {
		names := watchInitialNames(t, h,
			fsPodsPath+"?watch=true&timeoutSeconds=1&labelSelector=app%3Dweb")
		if !equalStrings(names, []string{"web-1"}) {
			t.Errorf("watch initial events = %v, want [web-1]", names)
		}
	})

	t.Run("no selectors at all", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, fsPodsPath, nil)
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != 200 {
			t.Fatalf("status %d (body: %s)", rw.Code, rw.Body.String())
		}
		if got := itemNames(t, rw.Body.Bytes()); !equalStrings(got, []string{"web-1", "db-1"}) {
			t.Errorf("got %v, want both pods", got)
		}
	})
}

// partialMetadataTable builds a stored Table whose rows embed
// PartialObjectMetadata, which is what a real apiserver serves for a Table
// projection. The row objects carry the *full* ObjectMeta — labels included, so
// a labelSelector still filters rows directly — and nothing else: spec and
// status are absent, which is why a selector on those has to be resolved
// against the JSON list instead. Verified against a real capture; getting this
// shape wrong in either direction makes the Table tests meaningless.
func partialMetadataTable(pods []podSpec) []byte {
	rows := make([]map[string]any, 0, len(pods))
	for _, p := range pods {
		rows = append(rows, map[string]any{
			"cells": []any{p.name, "1/1", "Running", 0, "1d"},
			"object": map[string]any{
				"apiVersion": "meta.k8s.io/v1",
				"kind":       "PartialObjectMetadata",
				"metadata": map[string]any{
					"name":      p.name,
					"namespace": p.namespace,
					"labels":    p.labels,
				},
			},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "meta.k8s.io/v1",
		"kind":       "Table",
		"metadata":   map[string]any{"resourceVersion": "1"},
		"columnDefinitions": []map[string]any{
			{"name": "Name", "type": "string"},
			{"name": "Ready", "type": "string"},
			{"name": "Status", "type": "string"},
			{"name": "Restarts", "type": "integer"},
			{"name": "Age", "type": "date"},
		},
		"rows": rows,
	})
	return body
}

// TestHandler_FieldSelector_StoredTableWithPartialMetadata is the regression
// test for the subtler half of the Table path. `kubectl get` without -o asks for
// a Table, and a stored Table's rows carry metadata only — so evaluating
// spec.nodeName against the row matches nothing regardless of its value, turning
// the over-matching bug into an under-matching one. A real apiserver filters the
// full objects and then projects, which is what the handler must reproduce.
func TestHandler_FieldSelector_StoredTableWithPartialMetadata(t *testing.T) {
	pods := []podSpec{
		{name: "web-1", namespace: "demo", nodeName: "node-a", phase: "Running"},
		{name: "web-2", namespace: "demo", nodeName: "node-b", phase: "Failed"},
	}
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/demo/pods":          listWithPods(pods),
		"/api/v1/namespaces/demo/pods?as=Table": partialMetadataTable(pods),
	})
	h := newHandler(store, time.Time{}, false)

	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"spec.nodeName=node-a", []string{"web-1"}},
		{"spec.nodeName=node-b", []string{"web-2"}},
		{"spec.nodeName=does-not-exist", nil},
		{"status.phase=Failed", []string{"web-2"}},
		// Metadata keys are present on the row objects, so they still filter
		// through the ordinary row-matching path.
		{"metadata.name=web-1", []string{"web-1"}},
	} {
		req := httptest.NewRequest(http.MethodGet, fsPodsPath+"?fieldSelector="+tc.query, nil)
		req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != 200 {
			t.Fatalf("[%s] status %d (body: %s)", tc.query, rw.Code, rw.Body.String())
		}
		got := tableRowNames(t, rw.Body.Bytes())
		if !equalStrings(got, tc.want) {
			t.Errorf("[%s] rows %v, want %v", tc.query, got, tc.want)
		}
	}
}

// TestHandler_FieldSelector_StoredTableFallsBackOnBadItem checks the escape
// hatch. When one list item's identity cannot be decoded there is no way to
// correlate it with a Table row, so kstore.ListIdentities fails closed and the
// handler must fall back to a computed Table — built from the already-filtered
// JSON list — rather than serve a stored Table with rows silently dropped.
func TestHandler_FieldSelector_StoredTableFallsBackOnBadItem(t *testing.T) {
	good := []podSpec{
		{name: "web-1", namespace: "demo", nodeName: "node-a", phase: "Running"},
	}
	// A list carrying one well-formed pod plus an item whose metadata.name is a
	// number, which decodes into neither K8sObject nor the identity-only shape.
	listBody, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "PodList",
		"metadata":   map[string]any{},
		"items": []json.RawMessage{
			json.RawMessage(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web-1",` +
				`"namespace":"demo"},"spec":{"nodeName":"node-a"},"status":{"phase":"Running"}}`),
			json.RawMessage(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":7,"namespace":"demo"}}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := buildTestStore(t, map[string][]byte{
		"/api/v1/namespaces/demo/pods":          listBody,
		"/api/v1/namespaces/demo/pods?as=Table": partialMetadataTable(good),
	})
	h := newHandler(store, time.Time{}, false)

	req := httptest.NewRequest(http.MethodGet, fsPodsPath+"?fieldSelector=spec.nodeName=node-a", nil)
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("status %d (body: %s)", rw.Code, rw.Body.String())
	}
	// Decoded leniently: the computed Table carries a row for the malformed item
	// too — its metadata.name is still the original number, so a strict
	// string-typed decode would fail here. That the row survives at all is the
	// point: the fallback trades captured column fidelity for not losing rows.
	var table struct {
		Kind string `json:"kind"`
		Rows []struct {
			Object struct {
				Metadata map[string]any `json:"metadata"`
			} `json:"object"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &table); err != nil {
		t.Fatalf("decoding table: %v\nbody: %s", err, rw.Body.String())
	}
	if table.Kind != "Table" {
		t.Fatalf("kind = %q, want Table", table.Kind)
	}
	var names []string
	for _, r := range table.Rows {
		names = append(names, fmt.Sprint(r.Object.Metadata["name"]))
	}
	found := false
	for _, n := range names {
		if n == "web-1" {
			found = true
		}
	}
	if !found {
		t.Errorf("rows %v: web-1 matches spec.nodeName=node-a and must not be dropped", names)
	}
}

// tableRowNames extracts metadata.name from each row's embedded object.
func tableRowNames(t *testing.T, body []byte) []string {
	t.Helper()
	var table struct {
		Kind string `json:"kind"`
		Rows []struct {
			Object struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"object"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(body, &table); err != nil {
		t.Fatalf("decoding table: %v\nbody: %s", err, body)
	}
	if table.Kind != "Table" {
		t.Fatalf("kind = %q, want Table", table.Kind)
	}
	var names []string
	for _, r := range table.Rows {
		names = append(names, r.Object.Metadata.Name)
	}
	return names
}

// TestHandler_FieldSelector_WatchFilters covers the watch path, which shares the
// conversion with list upstream. An informer watching one node's pods must not
// receive every pod.
func TestHandler_FieldSelector_WatchFilters(t *testing.T) {
	h := fieldSelectorTestStore(t)

	names := watchInitialNames(t, h, fsPodsPath+"?watch=true&timeoutSeconds=1&fieldSelector=spec.nodeName=node-a")
	if !equalStrings(names, []string{"web-1"}) {
		t.Errorf("watch initial events = %v, want [web-1]", names)
	}
	names = watchInitialNames(t, h, fsPodsPath+"?watch=true&timeoutSeconds=1&fieldSelector=spec.nodeName=does-not-exist")
	if len(names) != 0 {
		t.Errorf("watch initial events = %v, want none", names)
	}
}

// TestHandler_FieldSelector_WatchRejectsUnsupportedKey checks the watch path
// returns the same 400 as the list path rather than streaming everything.
func TestHandler_FieldSelector_WatchRejectsUnsupportedKey(t *testing.T) {
	h := fieldSelectorTestStore(t)
	req := httptest.NewRequest(http.MethodGet,
		fsPodsPath+"?watch=true&timeoutSeconds=1&fieldSelector=spec.bogus=x", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("watch with a bogus fieldSelector: status %d, want 400 (body: %s)",
			rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "field label not supported: spec.bogus") {
		t.Errorf("watch 400 body = %s", rw.Body.String())
	}
}

// TestOverlay_DeleteCollection_AcceptsPerKindFieldLabel is the write-path half
// of #339. deletecollection used to 400 on spec.nodeName — a key a real
// apiserver accepts for pods — so the two paths diverged from upstream in
// opposite directions. It must now be accepted, and match by value.
func TestOverlay_DeleteCollection_AcceptsPerKindFieldLabel(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock) // captures pod-base

	code, body := doReq(t, http.MethodDelete,
		srv.URL+podsPath+"?fieldSelector=spec.nodeName%3Dnode-1", "", "")
	if code != 200 {
		t.Fatalf("deletecollection with spec.nodeName: status %d, want 200 (body: %s)", code, body)
	}
	// pod-base carries no spec.nodeName, so it reads as the empty string and
	// does not match node-1 — accepted, and correctly matched nothing.
	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-base", "", ""); code != 200 {
		t.Errorf("pod-base should survive a non-matching fieldSelector: status %d", code)
	}
}

// watchInitialNames collects the object names from a watch stream's initial
// ADDED burst.
func watchInitialNames(t *testing.T, h *handler, url string) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("watch %s: status %d (body: %s)", url, rw.Code, rw.Body.String())
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(rw.Body.String()), "\n") {
		if line == "" {
			continue
		}
		var ev struct {
			Type   string `json:"type"`
			Object struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"object"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("decoding watch event %q: %v", line, err)
		}
		if ev.Type == "ADDED" {
			names = append(names, ev.Object.Metadata.Name)
		}
	}
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
