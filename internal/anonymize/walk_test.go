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

func TestWalkKeys(t *testing.T) {
	rename := func(path, key string) (string, bool) {
		if key == "old" {
			return "new", true
		}
		return "", false
	}

	t.Run("renames nested and array-contained keys, keeps values", func(t *testing.T) {
		obj := map[string]interface{}{
			"old": "v1",
			"list": []interface{}{
				map[string]interface{}{"old": map[string]interface{}{"old": "deep"}},
			},
			"keep": "x",
		}
		changed, err := walkKeys(obj, "", rename)
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v, want true, nil", changed, err)
		}
		if _, ok := obj["old"]; ok || obj["new"] != "v1" || obj["keep"] != "x" {
			t.Errorf("top level = %v", obj)
		}
		inner := obj["list"].([]interface{})[0].(map[string]interface{})
		deep := inner["new"].(map[string]interface{})
		if deep["new"] != "deep" {
			t.Errorf("nested = %v", inner)
		}
	})

	t.Run("paths use the original key names and [*] for arrays", func(t *testing.T) {
		var got []string
		obj := map[string]interface{}{"a": []interface{}{map[string]interface{}{"b": "x"}}}
		_, _ = walkKeys(obj, "", func(path, key string) (string, bool) {
			got = append(got, path)
			return "", false
		})
		want := map[string]bool{"a": true, "a[*].b": true}
		if len(got) != 2 || !want[got[0]] || !want[got[1]] {
			t.Errorf("paths = %v, want a and a[*].b", got)
		}
	})

	t.Run("a swap (A->B while B->C) doesn't clobber either value", func(t *testing.T) {
		obj := map[string]interface{}{"a": 1, "b": 2}
		changed, err := walkKeys(obj, "", func(path, key string) (string, bool) {
			switch key {
			case "a":
				return "b", true
			case "b":
				return "c", true
			}
			return "", false
		})
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		if obj["b"] != 1 || obj["c"] != 2 || len(obj) != 2 {
			t.Errorf("obj = %v, want b:1 c:2", obj)
		}
	})

	t.Run("two keys renamed onto the same name is an error, not silent data loss", func(t *testing.T) {
		obj := map[string]interface{}{"x": 1, "y": 2}
		_, err := walkKeys(obj, "", func(path, key string) (string, bool) { return "same", true })
		if err == nil {
			t.Fatal("want a collision error")
		}
	})

	t.Run("renaming onto an existing key that stays is an error", func(t *testing.T) {
		obj := map[string]interface{}{"old": 1, "new": 2}
		if _, err := walkKeys(obj, "", rename); err == nil {
			t.Fatal("want a collision error")
		}
	})

	t.Run("nothing to rename reports unchanged", func(t *testing.T) {
		obj := map[string]interface{}{"a": map[string]interface{}{"b": "c"}}
		if changed, err := walkKeys(obj, "", func(string, string) (string, bool) { return "", false }); changed || err != nil {
			t.Errorf("changed=%v err=%v", changed, err)
		}
	})
}
