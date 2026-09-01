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
		if !rewriteResourceNameInObject(obj, "Node", allResourceCategories, aliasByCategory) {
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
		if !rewriteResourceNameInObject(obj, "Pod", allResourceCategories, aliasByCategory) {
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
		if !rewriteResourceNameInObject(obj, "Deployment", allResourceCategories, aliasByCategory) {
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
		if !rewriteResourceNameInObject(obj, "Pod", allResourceCategories, aliasByCategory) {
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
		if !rewriteResourceNameInObject(obj, "Node", allResourceCategories, aliasByCategory) {
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
		if !rewriteResourceNameInObject(obj, "Event", allResourceCategories, aliasByCategory) {
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
		if !rewriteResourceNameInObject(obj, "Event", allResourceCategories, aliasByCategory) {
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
		if !rewriteResourceNameInObject(obj, "Pod", enabled, aliasByCategory) {
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
		if rewriteResourceNameInObject(obj, "Namespace", allResourceCategories, aliasByCategory) {
			t.Error("want modified=false — Namespace is not a node/pod/workload kind")
		}
	})

	t.Run("object with no metadata at all is left alone, not a crash", func(t *testing.T) {
		obj := map[string]interface{}{"kind": "Status"}
		if rewriteResourceNameInObject(obj, "Status", allResourceCategories, aliasByCategory) {
			t.Error("want modified=false")
		}
	})
}

func TestRewriteResourceNameInRecord(t *testing.T) {
	t.Run("single object", func(t *testing.T) {
		rec := &capture.Record{
			ResponseBody: json.RawMessage(`{"kind":"Node","metadata":{"name":"worker-1"}}`),
		}
		changed, err := rewriteResourceNameInRecord(rec, allResourceCategories, aliasByCategory)
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
		changed, err := rewriteResourceNameInRecord(rec, allResourceCategories, aliasByCategory)
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
		changed, err := rewriteResourceNameInRecord(rec, allResourceCategories, aliasByCategory)
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
