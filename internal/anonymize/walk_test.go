package anonymize

import (
	"encoding/json"
	"sort"
	"testing"
)

func TestWalkStrings_PathConstruction(t *testing.T) {
	body := `{
		"metadata": {"name": "web-1", "namespace": "prod"},
		"spec": {
			"containers": [
				{"name": "app", "image": "nginx"},
				{"name": "sidecar", "image": "envoy"}
			]
		},
		"nested": {"list": [{"deep": {"value": "x"}}]}
	}`
	var obj interface{}
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatal(err)
	}

	var gotPaths []string
	walkStrings(obj, "", func(path, s string) (string, bool) {
		gotPaths = append(gotPaths, path+"="+s)
		return "", false
	})
	sort.Strings(gotPaths)

	want := []string{
		"metadata.name=web-1",
		"metadata.namespace=prod",
		"nested.list[*].deep.value=x",
		"spec.containers[*].image=envoy",
		"spec.containers[*].image=nginx",
		"spec.containers[*].name=app",
		"spec.containers[*].name=sidecar",
	}
	sort.Strings(want)
	if len(gotPaths) != len(want) {
		t.Fatalf("got %d paths, want %d\ngot:  %v\nwant: %v", len(gotPaths), len(want), gotPaths, want)
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Errorf("path[%d] = %q, want %q", i, gotPaths[i], want[i])
		}
	}
}

func TestWalkStrings_ReplacesAndReportsChanged(t *testing.T) {
	obj := map[string]interface{}{
		"a": "keep",
		"b": "replace-me",
		"list": []interface{}{
			"replace-me",
			"keep",
		},
	}
	changed := walkStrings(obj, "", func(path, s string) (string, bool) {
		if s == "replace-me" {
			return "replaced", true
		}
		return "", false
	})
	if !changed {
		t.Fatal("want changed=true")
	}
	if obj["a"] != "keep" {
		t.Errorf("a = %v, want unchanged keep", obj["a"])
	}
	if obj["b"] != "replaced" {
		t.Errorf("b = %v, want replaced", obj["b"])
	}
	list := obj["list"].([]interface{})
	if list[0] != "replaced" || list[1] != "keep" {
		t.Errorf("list = %v, want [replaced keep]", list)
	}
}

func TestWalkStrings_NoMatchReportsUnchanged(t *testing.T) {
	obj := map[string]interface{}{"a": "x"}
	changed := walkStrings(obj, "", func(path, s string) (string, bool) {
		return "", false
	})
	if changed {
		t.Error("want changed=false when visit never matches")
	}
}
