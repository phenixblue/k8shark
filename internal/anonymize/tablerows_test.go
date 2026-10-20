package anonymize

import (
	"encoding/json"
	"testing"
)

func decodeObj(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestTableCellIndex(t *testing.T) {
	obj := decodeObj(t, `{"columnDefinitions":[{"name":"Name"},{"name":"Namespace"},{"name":"Ready"}]}`)
	for _, tc := range []struct {
		col    string
		want   int
		wantOK bool
	}{
		{"Name", 0, true},
		{"Namespace", 1, true},
		{"Ready", 2, true},
		{"name", 0, false}, // case-sensitive, exactly like the apiserver's own column names
		{"Node", 0, false},
	} {
		got, ok := tableCellIndex(obj, tc.col)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("tableCellIndex(%q) = (%d, %v), want (%d, %v)", tc.col, got, ok, tc.want, tc.wantOK)
		}
	}

	t.Run("no columnDefinitions at all", func(t *testing.T) {
		if _, ok := tableCellIndex(decodeObj(t, `{"rows":[]}`), "Name"); ok {
			t.Error("want ok=false")
		}
	})
	t.Run("a malformed columnDefinitions entry is skipped, not a panic", func(t *testing.T) {
		obj := decodeObj(t, `{"columnDefinitions":["oops",{"name":"Name"}]}`)
		if got, ok := tableCellIndex(obj, "Name"); !ok || got != 1 {
			t.Errorf("got (%d, %v), want (1, true)", got, ok)
		}
	})
}

func TestRewriteTableRows(t *testing.T) {
	rewrite := func(item map[string]interface{}, kind string) bool {
		meta, _ := item["metadata"].(map[string]interface{})
		if meta == nil || kind != "Pod" {
			return false
		}
		meta["name"] = "ALIASED"
		return true
	}

	t.Run("mirrorCell runs only for rows whose object changed, with the resolved Kind", func(t *testing.T) {
		obj := decodeObj(t, `{"kind":"Table","rows":[
			{"cells":["a"],"object":{"metadata":{"name":"a"}}},
			{"cells":["b"]}
		]}`)
		var gotKinds []string
		changed := rewriteTableRows(obj, "/api/v1/pods?as=Table", rewrite,
			func(_, _, _ map[string]interface{}, rowKind string) { gotKinds = append(gotKinds, rowKind) })
		if !changed {
			t.Fatal("want changed=true")
		}
		if len(gotKinds) != 1 || gotKinds[0] != "Pod" {
			t.Errorf("mirrorCell calls = %v, want exactly one with kind Pod (the objectless row is skipped)", gotKinds)
		}
	})

	t.Run("a nil mirrorCell is allowed", func(t *testing.T) {
		obj := decodeObj(t, `{"rows":[{"object":{"metadata":{"name":"a"}}}]}`)
		if !rewriteTableRows(obj, "/api/v1/pods?as=Table", rewrite, nil) {
			t.Error("want changed=true")
		}
	})

	t.Run("an unrecognized resource type does nothing and never calls rewriteObject", func(t *testing.T) {
		obj := decodeObj(t, `{"rows":[{"object":{"metadata":{"name":"a"}}}]}`)
		called := false
		changed := rewriteTableRows(obj, "/api/v1/services?as=Table",
			func(map[string]interface{}, string) bool { called = true; return true }, nil)
		if changed || called {
			t.Errorf("changed=%v called=%v, want both false", changed, called)
		}
	})

	t.Run("no rows, or rows that aren't an array, is a no-op", func(t *testing.T) {
		for _, body := range []string{`{"kind":"Table"}`, `{"kind":"Table","rows":null}`, `{"kind":"Table","rows":"x"}`} {
			if rewriteTableRows(decodeObj(t, body), "/api/v1/pods?as=TableSchema", rewrite, nil) {
				t.Errorf("body %s: want changed=false", body)
			}
		}
	})

	t.Run("a cells array shorter than the Name column index is not a panic", func(t *testing.T) {
		rec := decodeObj(t, `{"columnDefinitions":[{"name":"Status"},{"name":"Name"}],
			"rows":[{"cells":["Running"],"object":{"metadata":{"name":"a"}}}]}`)
		// Reuse the production Name-cell mirror shape: idx(1) >= len(cells)(1).
		rewriteTableRows(rec, "/api/v1/pods?as=Table", rewrite,
			func(tableObj, row, rowObj map[string]interface{}, _ string) {
				cells, _ := row["cells"].([]interface{})
				if idx, ok := tableCellIndex(tableObj, "Name"); ok && idx < len(cells) {
					cells[idx] = "should-not-happen"
				}
			})
		if got := rec["rows"].([]interface{})[0].(map[string]interface{})["cells"].([]interface{}); len(got) != 1 || got[0] != "Running" {
			t.Errorf("cells = %v, want untouched", got)
		}
	})
}
