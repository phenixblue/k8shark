package anonymize

import (
	"encoding/json"
	"testing"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestRewriteNamespaceInObject(t *testing.T) {
	alias := upper // from apipath_test.go: s + "-ALIASED"

	t.Run("Namespace object aliases its own metadata.name", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":     "Namespace",
			"metadata": map[string]interface{}{"name": "prod"},
		}
		if !rewriteNamespaceInObject(obj, "Namespace", noExclusions, alias) {
			t.Fatal("want modified=true")
		}
		meta := obj["metadata"].(map[string]interface{})
		if got := meta["name"]; got != "prod-ALIASED" {
			t.Errorf("metadata.name = %v, want prod-ALIASED", got)
		}
		if _, has := meta["namespace"]; has {
			t.Error("a Namespace object must not gain a metadata.namespace field")
		}
	})

	t.Run("namespaced object aliases its own metadata.namespace, not metadata.name", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":     "Pod",
			"metadata": map[string]interface{}{"name": "web-1", "namespace": "prod"},
		}
		if !rewriteNamespaceInObject(obj, "Pod", noExclusions, alias) {
			t.Fatal("want modified=true")
		}
		meta := obj["metadata"].(map[string]interface{})
		if got := meta["namespace"]; got != "prod-ALIASED" {
			t.Errorf("metadata.namespace = %v, want prod-ALIASED", got)
		}
		if got := meta["name"]; got != "web-1" {
			t.Errorf("metadata.name = %v, want unchanged web-1 — the pod's own name is not a namespace occurrence", got)
		}
	})

	t.Run("core/v1 Event aliases both its own metadata.namespace and involvedObject.namespace", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":           "Event",
			"metadata":       map[string]interface{}{"name": "web-1.abc", "namespace": "prod"},
			"involvedObject": map[string]interface{}{"kind": "Pod", "name": "web-1", "namespace": "prod"},
		}
		if !rewriteNamespaceInObject(obj, "Event", noExclusions, alias) {
			t.Fatal("want modified=true")
		}
		meta := obj["metadata"].(map[string]interface{})
		involved := obj["involvedObject"].(map[string]interface{})
		if got := meta["namespace"]; got != "prod-ALIASED" {
			t.Errorf("metadata.namespace = %v, want prod-ALIASED", got)
		}
		if got := involved["namespace"]; got != "prod-ALIASED" {
			t.Errorf("involvedObject.namespace = %v, want prod-ALIASED", got)
		}
	})

	// events.k8s.io/v1 is the Events API a real cluster actually emits by
	// default (core/v1 Event is the older, still-supported shape) — missing
	// this would anonymize metadata.namespace and the API path correctly
	// while leaving the real namespace name sitting in regarding.namespace
	// on the majority of Events a real capture contains.
	t.Run("events.k8s.io/v1 Event aliases regarding.namespace and related.namespace", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":      "Event",
			"metadata":  map[string]interface{}{"name": "web-1.abc", "namespace": "prod"},
			"regarding": map[string]interface{}{"kind": "Pod", "name": "web-1", "namespace": "prod"},
			"related":   map[string]interface{}{"kind": "ReplicaSet", "name": "web-1-rs", "namespace": "prod"},
		}
		if !rewriteNamespaceInObject(obj, "Event", noExclusions, alias) {
			t.Fatal("want modified=true")
		}
		regarding := obj["regarding"].(map[string]interface{})
		related := obj["related"].(map[string]interface{})
		if got := regarding["namespace"]; got != "prod-ALIASED" {
			t.Errorf("regarding.namespace = %v, want prod-ALIASED", got)
		}
		if got := related["namespace"]; got != "prod-ALIASED" {
			t.Errorf("related.namespace = %v, want prod-ALIASED", got)
		}
		if got := regarding["name"]; got != "web-1" {
			t.Errorf("regarding.name = %v, want unchanged web-1", got)
		}
	})

	t.Run("cluster-scoped object with no metadata.namespace is left alone", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind":     "Node",
			"metadata": map[string]interface{}{"name": "worker-1"},
		}
		if rewriteNamespaceInObject(obj, "Node", noExclusions, alias) {
			t.Error("want modified=false for an object with no namespace occurrence")
		}
		meta := obj["metadata"].(map[string]interface{})
		if got := meta["name"]; got != "worker-1" {
			t.Errorf("metadata.name = %v, want unchanged worker-1", got)
		}
	})

	t.Run("object with no metadata at all is left alone, not a crash", func(t *testing.T) {
		obj := map[string]interface{}{"kind": "Status"}
		if rewriteNamespaceInObject(obj, "Status", noExclusions, alias) {
			t.Error("want modified=false")
		}
	})
}

func TestRewriteNamespaceInRecord(t *testing.T) {
	alias := upper

	t.Run("single object", func(t *testing.T) {
		rec := &capture.Record{
			ResponseBody: json.RawMessage(`{"kind":"Namespace","metadata":{"name":"prod"}}`),
		}
		changed, err := rewriteNamespaceInRecord(rec, noExclusions, alias)
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
		if got := meta["name"]; got != "prod-ALIASED" {
			t.Errorf("metadata.name = %v, want prod-ALIASED", got)
		}
	})

	t.Run("list response rewrites every item, using each item's own kind", func(t *testing.T) {
		body := `{"kind":"PodList","items":[
			{"metadata":{"name":"web-1","namespace":"prod"}},
			{"metadata":{"name":"web-2","namespace":"staging"}}
		]}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		changed, err := rewriteNamespaceInRecord(rec, noExclusions, alias)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("want changed=true")
		}
		var out struct {
			Items []struct {
				Metadata struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Items) != 2 {
			t.Fatalf("got %d items, want 2", len(out.Items))
		}
		if out.Items[0].Metadata.Namespace != "prod-ALIASED" {
			t.Errorf("item 0 namespace = %q, want prod-ALIASED", out.Items[0].Metadata.Namespace)
		}
		if out.Items[1].Metadata.Namespace != "staging-ALIASED" {
			t.Errorf("item 1 namespace = %q, want staging-ALIASED", out.Items[1].Metadata.Namespace)
		}
		if out.Items[0].Metadata.Name != "web-1" || out.Items[1].Metadata.Name != "web-2" {
			t.Errorf("item names must survive unchanged, got %q and %q", out.Items[0].Metadata.Name, out.Items[1].Metadata.Name)
		}
	})

	t.Run("a Table-format body is left untouched, not an error", func(t *testing.T) {
		body := `{"kind":"Table","apiVersion":"meta.k8s.io/v1","rows":[{"cells":["web-1","Running"]}]}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		orig := string(rec.ResponseBody)
		changed, err := rewriteNamespaceInRecord(rec, noExclusions, alias)
		if err != nil {
			t.Fatalf("Table-format body should not error, got: %v", err)
		}
		// A Table body IS valid JSON (decodes into map[string]interface{}
		// fine — it just has "rows" instead of "items", and no top-level
		// metadata.namespace), so this exercises the "decodes but has
		// nothing to rewrite" path, not the "doesn't decode as JSON at all"
		// path. Both should leave the body untouched; this covers the one
		// this package will most often actually see, since discovery/OpenAPI
		// documents (the true non-object case) are rare in a real capture.
		if changed {
			t.Error("want changed=false for a Table-format body")
		}
		if string(rec.ResponseBody) != orig {
			t.Error("body must be byte-identical when nothing was rewritten")
		}
	})

	t.Run("a record with no namespace occurrence at all is left untouched", func(t *testing.T) {
		body := `{"kind":"Node","metadata":{"name":"worker-1"}}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		orig := string(rec.ResponseBody)
		changed, err := rewriteNamespaceInRecord(rec, noExclusions, alias)
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
