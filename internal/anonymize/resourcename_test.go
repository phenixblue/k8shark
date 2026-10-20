package anonymize

import (
	"encoding/json"
	"testing"

	"github.com/phenixblue/k8shark/internal/capture"
)

// allResourceCategories enables node, pod, and workload — the common case
// for tests that aren't specifically checking the enabled-gating behavior.
var allResourceCategories = map[Category]bool{
	CategoryNode:     true,
	CategoryPod:      true,
	CategoryWorkload: true,
}

func aliasByCategory(cat Category, original string) string {
	return string(cat) + ":" + original + "-ALIASED"
}

func TestRewriteResourceNameInObject(t *testing.T) {
	t.Run("Node aliases its own metadata.name", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":     "Node",
			"metadata": map[string]interface{}{"name": "worker-1"},
		}
		if !rewriteResourceNameInObject(obj, "Node", allResourceCategories, noExclusions, aliasByCategory) {
			t.Fatal("want modified=true")
		}
		meta := obj["metadata"].(map[string]interface{})
		if got, want := meta["name"], "node:worker-1-ALIASED"; got != want {
			t.Errorf("metadata.name = %v, want %v", got, want)
		}
	})

	t.Run("Pod aliases its own metadata.name and spec.nodeName, using different categories", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":     "Pod",
			"metadata": map[string]interface{}{"name": "web-1", "namespace": "prod"},
			"spec":     map[string]interface{}{"nodeName": "worker-1"},
		}
		if !rewriteResourceNameInObject(obj, "Pod", allResourceCategories, noExclusions, aliasByCategory) {
			t.Fatal("want modified=true")
		}
		meta := obj["metadata"].(map[string]interface{})
		spec := obj["spec"].(map[string]interface{})
		if got, want := meta["name"], "pod:web-1-ALIASED"; got != want {
			t.Errorf("metadata.name = %v, want %v", got, want)
		}
		if got, want := spec["nodeName"], "node:worker-1-ALIASED"; got != want {
			t.Errorf("spec.nodeName = %v, want %v", got, want)
		}
	})

	t.Run("Deployment aliases its own metadata.name as workload", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":     "Deployment",
			"metadata": map[string]interface{}{"name": "web", "namespace": "prod"},
		}
		if !rewriteResourceNameInObject(obj, "Deployment", allResourceCategories, noExclusions, aliasByCategory) {
			t.Fatal("want modified=true")
		}
		meta := obj["metadata"].(map[string]interface{})
		if got, want := meta["name"], "workload:web-ALIASED"; got != want {
			t.Errorf("metadata.name = %v, want %v", got, want)
		}
	})

	t.Run("ownerReferences[*].name is aliased by the reference's own kind", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind": "Pod",
			"metadata": map[string]interface{}{
				"name": "web-abc123",
				"ownerReferences": []interface{}{
					map[string]interface{}{"kind": "ReplicaSet", "name": "web-rs"},
				},
			},
		}
		if !rewriteResourceNameInObject(obj, "Pod", allResourceCategories, noExclusions, aliasByCategory) {
			t.Fatal("want modified=true")
		}
		meta := obj["metadata"].(map[string]interface{})
		refs := meta["ownerReferences"].([]interface{})
		ref := refs[0].(map[string]interface{})
		if got, want := ref["name"], "workload:web-rs-ALIASED"; got != want {
			t.Errorf("ownerReferences[0].name = %v, want %v", got, want)
		}
	})

	t.Run("Node status.addresses aliases only the Hostname entry, not InternalIP", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":     "Node",
			"metadata": map[string]interface{}{"name": "worker-1"},
			"status": map[string]interface{}{
				"addresses": []interface{}{
					map[string]interface{}{"type": "InternalIP", "address": "10.0.0.5"},
					map[string]interface{}{"type": "Hostname", "address": "worker-1"},
				},
			},
		}
		if !rewriteResourceNameInObject(obj, "Node", allResourceCategories, noExclusions, aliasByCategory) {
			t.Fatal("want modified=true")
		}
		addrs := obj["status"].(map[string]interface{})["addresses"].([]interface{})
		internalIP := addrs[0].(map[string]interface{})
		hostname := addrs[1].(map[string]interface{})
		if got, want := internalIP["address"], "10.0.0.5"; got != want {
			t.Errorf("InternalIP address = %v, want unchanged %v", got, want)
		}
		if got, want := hostname["address"], "node:worker-1-ALIASED"; got != want {
			t.Errorf("Hostname address = %v, want %v", got, want)
		}
	})

	t.Run("core/v1 Event aliases involvedObject.name and source.host", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":           "Event",
			"metadata":       map[string]interface{}{"name": "web-1.abc", "namespace": "prod"},
			"involvedObject": map[string]interface{}{"kind": "Pod", "name": "web-1", "namespace": "prod"},
			"source":         map[string]interface{}{"component": "kubelet", "host": "worker-1"},
		}
		if !rewriteResourceNameInObject(obj, "Event", allResourceCategories, noExclusions, aliasByCategory) {
			t.Fatal("want modified=true")
		}
		involved := obj["involvedObject"].(map[string]interface{})
		source := obj["source"].(map[string]interface{})
		if got, want := involved["name"], "pod:web-1-ALIASED"; got != want {
			t.Errorf("involvedObject.name = %v, want %v", got, want)
		}
		if got, want := source["host"], "node:worker-1-ALIASED"; got != want {
			t.Errorf("source.host = %v, want %v", got, want)
		}
	})

	// events.k8s.io/v1 is what a real cluster actually emits by default —
	// missing this would alias core/v1 Events correctly while leaving the
	// real pod/node names sitting in regarding.name and
	// deprecatedSource.host on the majority of Events a real capture
	// contains. Mirrors namespace_test.go's identical events.k8s.io/v1
	// coverage.
	t.Run("events.k8s.io/v1 Event aliases regarding.name, related.name, and deprecatedSource.host", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":             "Event",
			"metadata":         map[string]interface{}{"name": "web-1.abc", "namespace": "prod"},
			"regarding":        map[string]interface{}{"kind": "Pod", "name": "web-1", "namespace": "prod"},
			"related":          map[string]interface{}{"kind": "ReplicaSet", "name": "web-1-rs", "namespace": "prod"},
			"deprecatedSource": map[string]interface{}{"component": "kubelet", "host": "worker-1"},
		}
		if !rewriteResourceNameInObject(obj, "Event", allResourceCategories, noExclusions, aliasByCategory) {
			t.Fatal("want modified=true")
		}
		regarding := obj["regarding"].(map[string]interface{})
		related := obj["related"].(map[string]interface{})
		depSource := obj["deprecatedSource"].(map[string]interface{})
		if got, want := regarding["name"], "pod:web-1-ALIASED"; got != want {
			t.Errorf("regarding.name = %v, want %v", got, want)
		}
		if got, want := related["name"], "workload:web-1-rs-ALIASED"; got != want {
			t.Errorf("related.name = %v, want %v", got, want)
		}
		if got, want := depSource["host"], "node:worker-1-ALIASED"; got != want {
			t.Errorf("deprecatedSource.host = %v, want %v", got, want)
		}
	})

	t.Run("enabled gates by category — pod alone must not alias spec.nodeName", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":     "Pod",
			"metadata": map[string]interface{}{"name": "web-1"},
			"spec":     map[string]interface{}{"nodeName": "worker-1"},
		}
		enabled := map[Category]bool{CategoryPod: true}
		if !rewriteResourceNameInObject(obj, "Pod", enabled, noExclusions, aliasByCategory) {
			t.Fatal("want modified=true for the pod name itself")
		}
		meta := obj["metadata"].(map[string]interface{})
		spec := obj["spec"].(map[string]interface{})
		if got, want := meta["name"], "pod:web-1-ALIASED"; got != want {
			t.Errorf("metadata.name = %v, want %v", got, want)
		}
		if got, want := spec["nodeName"], "worker-1"; got != want {
			t.Errorf("spec.nodeName = %v, want unchanged %v — node category was not enabled", got, want)
		}
	})

	t.Run("namespace category is out of scope for this function", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":     "Namespace",
			"metadata": map[string]interface{}{"name": "prod"},
		}
		if rewriteResourceNameInObject(obj, "Namespace", allResourceCategories, noExclusions, aliasByCategory) {
			t.Error("want modified=false — Namespace is not a node/pod/workload kind")
		}
	})

	t.Run("object with no metadata at all is left alone, not a crash", func(t *testing.T) {
		obj := map[string]interface{}{"kind": "Status"}
		if rewriteResourceNameInObject(obj, "Status", allResourceCategories, noExclusions, aliasByCategory) {
			t.Error("want modified=false")
		}
	})
}

func TestRewriteResourceNameInRecord(t *testing.T) {
	t.Run("single object", func(t *testing.T) {
		rec := &capture.Record{
			ResponseBody: json.RawMessage(`{"kind":"Node","metadata":{"name":"worker-1"}}`),
		}
		changed, err := rewriteResourceNameInRecord(rec, allResourceCategories, noExclusions, aliasByCategory)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("want changed=true")
		}
		var out map[string]interface{}
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		meta := out["metadata"].(map[string]interface{})
		if got, want := meta["name"], "node:worker-1-ALIASED"; got != want {
			t.Errorf("metadata.name = %v, want %v", got, want)
		}
	})

	t.Run("list response rewrites every item, using each item's own kind", func(t *testing.T) {
		body := `{"kind":"PodList","items":[
			{"metadata":{"name":"web-1"},"spec":{"nodeName":"worker-1"}},
			{"metadata":{"name":"web-2"},"spec":{"nodeName":"worker-2"}}
		]}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		changed, err := rewriteResourceNameInRecord(rec, allResourceCategories, noExclusions, aliasByCategory)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("want changed=true")
		}
		var out struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Spec struct {
					NodeName string `json:"nodeName"`
				} `json:"spec"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Items) != 2 {
			t.Fatalf("got %d items, want 2", len(out.Items))
		}
		if out.Items[0].Metadata.Name != "pod:web-1-ALIASED" || out.Items[1].Metadata.Name != "pod:web-2-ALIASED" {
			t.Errorf("item names = %q, %q", out.Items[0].Metadata.Name, out.Items[1].Metadata.Name)
		}
		if out.Items[0].Spec.NodeName != "node:worker-1-ALIASED" || out.Items[1].Spec.NodeName != "node:worker-2-ALIASED" {
			t.Errorf("item nodeNames = %q, %q", out.Items[0].Spec.NodeName, out.Items[1].Spec.NodeName)
		}
	})

	t.Run("a record with no recognized occurrence at all is left untouched", func(t *testing.T) {
		body := `{"kind":"Namespace","metadata":{"name":"prod"}}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		orig := string(rec.ResponseBody)
		changed, err := rewriteResourceNameInRecord(rec, allResourceCategories, noExclusions, aliasByCategory)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Error("want changed=false")
		}
		if string(rec.ResponseBody) != orig {
			t.Error("body must be byte-identical when nothing was rewritten")
		}
	})

	type tableOut struct {
		Rows []struct {
			Cells  []string `json:"cells"`
			Object struct {
				Metadata struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"metadata"`
			} `json:"object"`
		} `json:"rows"`
	}

	// `kubectl get pods` renders the printed Name cell, not the embedded
	// object — both must carry the alias, and a column this category doesn't
	// own (Namespace, Node) must be left alone: --full-sweep remains the
	// only way to reach those.
	t.Run("a Pod Table response aliases rows[*].object.metadata.name and mirrors only the Name cell", func(t *testing.T) {
		body := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
			"columnDefinitions":[{"name":"Name"},{"name":"Namespace"},{"name":"Node"}],
			"rows":[{"cells":["web-1","prod","worker-1"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",
				"metadata":{"name":"web-1","namespace":"prod"}}}]}`
		rec := &capture.Record{APIPath: "/api/v1/namespaces/prod/pods?as=Table", ResponseBody: json.RawMessage(body)}
		changed, err := rewriteResourceNameInRecord(rec, allResourceCategories, noExclusions, aliasByCategory)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("want changed=true")
		}
		var out tableOut
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		row := out.Rows[0]
		if row.Object.Metadata.Name != "pod:web-1-ALIASED" {
			t.Errorf("rows[0].object.metadata.name = %q, want pod:web-1-ALIASED", row.Object.Metadata.Name)
		}
		if row.Cells[0] != "pod:web-1-ALIASED" {
			t.Errorf("rows[0].cells[0] (Name) = %q, want pod:web-1-ALIASED", row.Cells[0])
		}
		if row.Cells[1] != "prod" {
			t.Errorf("rows[0].cells[1] (Namespace) = %q, want unchanged — not this category's cell", row.Cells[1])
		}
		if row.Cells[2] != "worker-1" {
			t.Errorf("rows[0].cells[2] (Node) = %q, want unchanged — only --full-sweep reaches non-Name columns", row.Cells[2])
		}
	})

	t.Run("a Node Table response aliases its Name cell under the node category", func(t *testing.T) {
		body := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
			"columnDefinitions":[{"name":"Name"},{"name":"Status"}],
			"rows":[{"cells":["worker-1","Ready"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",
				"metadata":{"name":"worker-1"}}}]}`
		rec := &capture.Record{APIPath: "/api/v1/nodes?as=Table", ResponseBody: json.RawMessage(body)}
		if _, err := rewriteResourceNameInRecord(rec, allResourceCategories, noExclusions, aliasByCategory); err != nil {
			t.Fatal(err)
		}
		var out tableOut
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		if got := out.Rows[0].Cells[0]; got != "node:worker-1-ALIASED" {
			t.Errorf("cells[0] = %q, want node:worker-1-ALIASED", got)
		}
	})

	t.Run("a category not in enabled leaves its Table untouched", func(t *testing.T) {
		body := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
			"columnDefinitions":[{"name":"Name"}],
			"rows":[{"cells":["web-1"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"web-1"}}}]}`
		rec := &capture.Record{APIPath: "/api/v1/namespaces/prod/pods?as=Table", ResponseBody: json.RawMessage(body)}
		orig := string(rec.ResponseBody)
		changed, err := rewriteResourceNameInRecord(rec, map[Category]bool{CategoryNode: true}, noExclusions, aliasByCategory)
		if err != nil {
			t.Fatal(err)
		}
		if changed || string(rec.ResponseBody) != orig {
			t.Error("--categories node alone must not alias a Pod Table's names")
		}
	})

	t.Run("an exclude rule on metadata.name leaves both the object and the Name cell real", func(t *testing.T) {
		// Nothing else on a bare Node row is aliasable, so the excluded
		// metadata.name means rewriteObject reports no change at all and
		// the cell is never touched — one rule protects both
		// representations, unlike --full-sweep's separately-swept cells.
		body := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
			"columnDefinitions":[{"name":"Name"}],
			"rows":[{"cells":["worker-1"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"worker-1"}}}]}`
		rec := &capture.Record{APIPath: "/api/v1/nodes?as=Table", ResponseBody: json.RawMessage(body)}
		excluded := func(cat Category, kind, path string) bool {
			return cat == CategoryNode && kind == "Node" && path == "metadata.name"
		}
		if _, err := rewriteResourceNameInRecord(rec, allResourceCategories, excluded, aliasByCategory); err != nil {
			t.Fatal(err)
		}
		var out tableOut
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		if out.Rows[0].Object.Metadata.Name != "worker-1" || out.Rows[0].Cells[0] != "worker-1" {
			t.Errorf("object=%q cell=%q, want both left real", out.Rows[0].Object.Metadata.Name, out.Rows[0].Cells[0])
		}
	})

	t.Run("a cells[*] exclude rule keeps the printed cell real while the object is aliased", func(t *testing.T) {
		body := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
			"columnDefinitions":[{"name":"Name"}],
			"rows":[{"cells":["worker-1"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"worker-1"}}}]}`
		rec := &capture.Record{APIPath: "/api/v1/nodes?as=Table", ResponseBody: json.RawMessage(body)}
		excluded := func(cat Category, kind, path string) bool {
			return cat == CategoryNode && kind == "Node" && path == "cells[*]"
		}
		if _, err := rewriteResourceNameInRecord(rec, allResourceCategories, excluded, aliasByCategory); err != nil {
			t.Fatal(err)
		}
		var out tableOut
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		if out.Rows[0].Object.Metadata.Name != "node:worker-1-ALIASED" {
			t.Errorf("object name = %q, want aliased", out.Rows[0].Object.Metadata.Name)
		}
		if out.Rows[0].Cells[0] != "worker-1" {
			t.Errorf("cell = %q, want real — cells[*] is excluded", out.Rows[0].Cells[0])
		}
	})

	t.Run("a Table for a resource type outside the recognized set is left untouched", func(t *testing.T) {
		body := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
			"columnDefinitions":[{"name":"Name"}],
			"rows":[{"cells":["svc-1"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"svc-1"}}}]}`
		rec := &capture.Record{APIPath: "/api/v1/services?as=Table", ResponseBody: json.RawMessage(body)}
		orig := string(rec.ResponseBody)
		changed, err := rewriteResourceNameInRecord(rec, allResourceCategories, noExclusions, aliasByCategory)
		if err != nil {
			t.Fatal(err)
		}
		if changed || string(rec.ResponseBody) != orig {
			t.Error("a Service Table has no resource-name category; want it byte-identical")
		}
	})
}

func TestRewriteResourceNameInPath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		enabled map[Category]bool
		want    string
		wantOK  bool
	}{
		{
			name:    "cluster-scoped node GET",
			path:    "/api/v1/nodes/worker-1",
			enabled: allResourceCategories,
			want:    "/api/v1/nodes/node:worker-1-ALIASED",
			wantOK:  true,
		},
		{
			name:    "namespaced pod GET",
			path:    "/api/v1/namespaces/prod/pods/web-1",
			enabled: allResourceCategories,
			want:    "/api/v1/namespaces/prod/pods/pod:web-1-ALIASED",
			wantOK:  true,
		},
		{
			name:    "group/version workload GET",
			path:    "/apis/apps/v1/namespaces/prod/deployments/web",
			enabled: allResourceCategories,
			want:    "/apis/apps/v1/namespaces/prod/deployments/workload:web-ALIASED",
			wantOK:  true,
		},
		{
			name:    "subresource path — only the name segment moves",
			path:    "/api/v1/namespaces/prod/pods/web-1/log",
			enabled: allResourceCategories,
			want:    "/api/v1/namespaces/prod/pods/pod:web-1-ALIASED/log",
			wantOK:  true,
		},
		{
			name:    "namespaced pod list — nothing follows the resource type",
			path:    "/api/v1/namespaces/prod/pods",
			enabled: allResourceCategories,
			want:    "/api/v1/namespaces/prod/pods",
			wantOK:  false,
		},
		{
			name:    "all-namespaces pod list",
			path:    "/api/v1/pods",
			enabled: allResourceCategories,
			want:    "/api/v1/pods",
			wantOK:  false,
		},
		{
			name:    "cluster-scoped list — no name segment",
			path:    "/api/v1/nodes",
			enabled: allResourceCategories,
			want:    "/api/v1/nodes",
			wantOK:  false,
		},
		{
			name:    "unrecognized resource type",
			path:    "/api/v1/namespaces/prod/configmaps/my-config",
			enabled: allResourceCategories,
			want:    "/api/v1/namespaces/prod/configmaps/my-config",
			wantOK:  false,
		},
		{
			name:    "recognized resource type but its category is not enabled",
			path:    "/api/v1/nodes/worker-1",
			enabled: map[Category]bool{CategoryPod: true, CategoryWorkload: true},
			want:    "/api/v1/nodes/worker-1",
			wantOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rewriteResourceNameInPath(tc.path, tc.enabled, aliasByCategory)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("rewriteResourceNameInPath(%q) = (%q, %v), want (%q, %v)",
					tc.path, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// A namespace value that happens to equal a real resource-type segment (e.g.
// a namespace literally named "pods") must not be mistaken for the
// resource-type segment itself — this is exactly the failure mode a
// literal-segment search (like rewriteNamespaceInPath's) would fall into,
// which is why this function instead walks the path's actual structural
// position.
func TestRewriteResourceNameInPath_NamespaceValueEqualToResourceTypeIsNotConfused(t *testing.T) {
	path := "/api/v1/namespaces/pods/pods/web-1"
	got, ok := rewriteResourceNameInPath(path, allResourceCategories, aliasByCategory)
	want := "/api/v1/namespaces/pods/pods/pod:web-1-ALIASED"
	if !ok || got != want {
		t.Fatalf("rewriteResourceNameInPath(%q) = (%q, %v), want (%q, true)", path, got, ok, want)
	}
}

func TestTableRowKindFromAPIPath(t *testing.T) {
	cases := []struct {
		path     string
		wantKind string
		wantOK   bool
	}{
		{"/api/v1/nodes?as=Table", "Node", true},
		{"/api/v1/nodes?as=TableSchema", "Node", true},
		{"/api/v1/namespaces/basic-demo/pods?as=Table", "Pod", true},
		{"/apis/apps/v1/namespaces/basic-demo/deployments?as=Table", "Deployment", true},
		{"/apis/apps/v1/replicasets?as=TableSchema", "ReplicaSet", true},
		{"/api/v1/namespaces?as=TableSchema", "Namespace", true},
		// A cluster-scoped list with no query string at all, just in case
		// one is ever passed through — not just the Table-suffixed form.
		{"/api/v1/nodes", "Node", true},
		// Outside resourceTypeKind's small, maintained set: every other
		// native kind (Service here) and any CRD must report ok=false, not
		// a wrong guess.
		{"/api/v1/services?as=TableSchema", "", false},
		{"/apis/portworx.io/v1/storagenodes?as=TableSchema", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		kind, ok := tableRowKindFromAPIPath(tc.path)
		if kind != tc.wantKind || ok != tc.wantOK {
			t.Errorf("tableRowKindFromAPIPath(%q) = (%q, %v), want (%q, %v)", tc.path, kind, ok, tc.wantKind, tc.wantOK)
		}
	}
}
