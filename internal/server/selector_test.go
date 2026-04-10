package server

import (
	"encoding/json"
	"testing"
)

func TestParseRequirements(t *testing.T) {
	cases := []struct {
		sel    string
		count  int
		op     string
		key    string
		values []string
	}{
		{"app=nginx", 1, "=", "app", []string{"nginx"}},
		{"app==nginx", 1, "=", "app", []string{"nginx"}},
		{"app!=nginx", 1, "!=", "app", []string{"nginx"}},
		{"app in (nginx,redis)", 1, "in", "app", []string{"nginx", "redis"}},
		{"app notin (nginx,redis)", 1, "notin", "app", []string{"nginx", "redis"}},
		{"app", 1, "exists", "app", nil},
		{"!app", 1, "doesnotexist", "app", nil},
	}
	for _, tc := range cases {
		reqs, err := parseRequirements(tc.sel)
		if err != nil {
			t.Errorf("parseRequirements(%q) error: %v", tc.sel, err)
			continue
		}
		if len(reqs) != tc.count {
			t.Errorf("parseRequirements(%q): got %d reqs, want %d", tc.sel, len(reqs), tc.count)
			continue
		}
		r := reqs[0]
		if r.key != tc.key {
			t.Errorf("[%q] key: got %q, want %q", tc.sel, r.key, tc.key)
		}
		if r.op != tc.op {
			t.Errorf("[%q] op: got %q, want %q", tc.sel, r.op, tc.op)
		}
		if tc.values != nil {
			for i, v := range tc.values {
				if i >= len(r.values) || r.values[i] != v {
					t.Errorf("[%q] values[%d]: got %v, want %q", tc.sel, i, r.values, v)
				}
			}
		}
	}
}

func TestMultiRequirement(t *testing.T) {
	reqs, err := parseRequirements("app=nginx,env=prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requirements, got %d", len(reqs))
	}
}

func TestInWithCommas(t *testing.T) {
	reqs, err := parseRequirements("app in (nginx,redis),env=prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requirements, got %d: %+v", len(reqs), reqs)
	}
}

func TestApplySelectors_LabelFilter(t *testing.T) {
	list := listWithPods([]podSpec{
		{name: "nginx", labels: map[string]string{"app": "nginx", "env": "prod"}},
		{name: "redis", labels: map[string]string{"app": "redis", "env": "prod"}},
		{name: "nginx-dev", labels: map[string]string{"app": "nginx", "env": "dev"}},
	})

	cases := []struct {
		sel       string
		wantNames []string
	}{
		{"app=nginx", []string{"nginx", "nginx-dev"}},
		{"app=redis", []string{"redis"}},
		{"app!=nginx", []string{"redis"}},
		{"env in (prod)", []string{"nginx", "redis"}},
		{"app notin (nginx,redis)", []string{}},
		{"app", []string{"nginx", "redis", "nginx-dev"}},
		{"!missing", []string{"nginx", "redis", "nginx-dev"}},
		{"app=nginx,env=prod", []string{"nginx"}},
		{"app=nginx,env=dev", []string{"nginx-dev"}},
	}

	for _, tc := range cases {
		filtered, err := applySelectors(list, tc.sel, "")
		if err != nil {
			t.Fatalf("[%q] error: %v", tc.sel, err)
		}
		names := itemNames(t, filtered)
		if !stringSliceEqual(names, tc.wantNames) {
			t.Errorf("[%q] got %v, want %v", tc.sel, names, tc.wantNames)
		}
	}
}

func TestApplySelectors_FieldFilter(t *testing.T) {
	list := listWithPods([]podSpec{
		{name: "nginx", namespace: "default", labels: nil},
		{name: "redis", namespace: "kube-system", labels: nil},
	})

	cases := []struct {
		sel       string
		wantNames []string
	}{
		{"metadata.name=nginx", []string{"nginx"}},
		{"metadata.name!=nginx", []string{"redis"}},
		{"metadata.namespace=kube-system", []string{"redis"}},
		{"metadata.namespace!=kube-system", []string{"nginx"}},
	}

	for _, tc := range cases {
		filtered, err := applySelectors(list, "", tc.sel)
		if err != nil {
			t.Fatalf("[%q] error: %v", tc.sel, err)
		}
		names := itemNames(t, filtered)
		if !stringSliceEqual(names, tc.wantNames) {
			t.Errorf("[%q] got %v, want %v", tc.sel, names, tc.wantNames)
		}
	}
}

func TestApplySelectors_EmptySelector(t *testing.T) {
	list := listWithPods([]podSpec{{name: "nginx", labels: map[string]string{"app": "nginx"}}})
	out, err := applySelectors(list, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Should be identical (same pointer content).
	if string(out) != string(list) {
		t.Error("empty selectors should return body unchanged")
	}
}

func TestApplySelectors_NotAList(t *testing.T) {
	body := []byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"nginx"}}`)
	out, err := applySelectors(body, "app=nginx", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Error("non-list body should be returned unchanged")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

type podSpec struct {
	name      string
	namespace string
	labels    map[string]string
}

func listWithPods(pods []podSpec) []byte {
	items := make([]map[string]any, 0, len(pods))
	for _, p := range pods {
		ns := p.namespace
		if ns == "" {
			ns = "default"
		}
		items = append(items, map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":      p.name,
				"namespace": ns,
				"labels":    p.labels,
			},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "PodList",
		"metadata":   map[string]any{},
		"items":      items,
	})
	return body
}

func itemNames(t *testing.T, body []byte) []string {
	t.Helper()
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("itemNames unmarshal: %v\nbody: %s", err, body)
	}
	names := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		names = append(names, it.Metadata.Name)
	}
	return names
}

func stringSliceEqual(a, b []string) bool {
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

// ── filterTableRows tests ─────────────────────────────────────────────────────

// tableWithPods builds a meta.k8s.io/v1 Table JSON from a slice of podSpecs,
// mirroring the structure the mock server stores from a real cluster capture.
func tableWithPods(pods []podSpec) []byte {
	rows := make([]map[string]any, 0, len(pods))
	for _, p := range pods {
		ns := p.namespace
		if ns == "" {
			ns = "default"
		}
		rows = append(rows, map[string]any{
			"cells": []any{p.name, ns, "1d"},
			"object": map[string]any{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]any{
					"name":      p.name,
					"namespace": ns,
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
			{"name": "Namespace", "type": "string"},
			{"name": "Age", "type": "date"},
		},
		"rows": rows,
	})
	return body
}

// tableRowNames extracts metadata.name from each row's embedded object.
func tableRowNames(t *testing.T, body []byte) []string {
	t.Helper()
	var table struct {
		Rows []struct {
			Object struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"object"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(body, &table); err != nil {
		t.Fatalf("tableRowNames unmarshal: %v\nbody: %s", err, body)
	}
	names := make([]string, 0, len(table.Rows))
	for _, r := range table.Rows {
		names = append(names, r.Object.Metadata.Name)
	}
	return names
}

func TestFilterTableRows_LabelFilter(t *testing.T) {
	table := tableWithPods([]podSpec{
		{name: "nginx-1", labels: map[string]string{"app": "nginx", "env": "prod"}},
		{name: "redis-1", labels: map[string]string{"app": "redis", "env": "prod"}},
		{name: "nginx-dev", labels: map[string]string{"app": "nginx", "env": "dev"}},
	})

	cases := []struct {
		sel       string
		wantNames []string
	}{
		{"app=nginx", []string{"nginx-1", "nginx-dev"}},
		{"app=redis", []string{"redis-1"}},
		{"app!=nginx", []string{"redis-1"}},
		{"env in (prod)", []string{"nginx-1", "redis-1"}},
		{"app notin (nginx,redis)", []string{}},
		{"app", []string{"nginx-1", "redis-1", "nginx-dev"}},
		{"!missing", []string{"nginx-1", "redis-1", "nginx-dev"}},
		{"app=nginx,env=prod", []string{"nginx-1"}},
	}

	for _, tc := range cases {
		filtered, err := filterTableRows(table, tc.sel, "")
		if err != nil {
			t.Fatalf("[%q] error: %v", tc.sel, err)
		}
		names := tableRowNames(t, filtered)
		if !stringSliceEqual(names, tc.wantNames) {
			t.Errorf("[%q] got %v, want %v", tc.sel, names, tc.wantNames)
		}
	}
}

func TestFilterTableRows_FieldFilter(t *testing.T) {
	table := tableWithPods([]podSpec{
		{name: "nginx", namespace: "default"},
		{name: "redis", namespace: "kube-system"},
	})

	cases := []struct {
		sel       string
		wantNames []string
	}{
		{"metadata.name=nginx", []string{"nginx"}},
		{"metadata.name!=nginx", []string{"redis"}},
		{"metadata.namespace=kube-system", []string{"redis"}},
	}

	for _, tc := range cases {
		filtered, err := filterTableRows(table, "", tc.sel)
		if err != nil {
			t.Fatalf("[%q] error: %v", tc.sel, err)
		}
		names := tableRowNames(t, filtered)
		if !stringSliceEqual(names, tc.wantNames) {
			t.Errorf("[%q] got %v, want %v", tc.sel, names, tc.wantNames)
		}
	}
}

func TestFilterTableRows_EmptySelector(t *testing.T) {
	table := tableWithPods([]podSpec{{name: "nginx", labels: map[string]string{"app": "nginx"}}})
	out, err := filterTableRows(table, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(table) {
		t.Error("empty selectors should return body unchanged")
	}
}

func TestFilterTableRows_NotATable(t *testing.T) {
	body := []byte(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"nginx"}}`)
	out, err := filterTableRows(body, "app=nginx", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Error("non-Table body should be returned unchanged")
	}
}
