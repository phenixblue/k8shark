package anonymize

import (
	"encoding/json"
	"testing"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestRewriteIPInRecord(t *testing.T) {
	t.Run("schema-aware field: status.podIP", func(t *testing.T) {
		rec := &capture.Record{
			ResponseBody: json.RawMessage(`{"kind":"Pod","status":{"podIP":"10.1.2.3"}}`),
		}
		changed, err := rewriteIPInRecord(rec, upper)
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
		if got := out["status"].(map[string]interface{})["podIP"]; got != "10.1.2.3-ALIASED" {
			t.Errorf("status.podIP = %v, want 10.1.2.3-ALIASED", got)
		}
	})

	t.Run("nested list: status.podIPs[*].ip", func(t *testing.T) {
		rec := &capture.Record{
			ResponseBody: json.RawMessage(`{"kind":"Pod","status":{"podIPs":[{"ip":"10.1.2.3"},{"ip":"fd00::1"}]}}`),
		}
		changed, err := rewriteIPInRecord(rec, upper)
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
		ips := out["status"].(map[string]interface{})["podIPs"].([]interface{})
		if got := ips[0].(map[string]interface{})["ip"]; got != "10.1.2.3-ALIASED" {
			t.Errorf("podIPs[0].ip = %v, want 10.1.2.3-ALIASED", got)
		}
		if got := ips[1].(map[string]interface{})["ip"]; got != "fd00::1-ALIASED" {
			t.Errorf("podIPs[1].ip = %v, want fd00::1-ALIASED", got)
		}
	})

	// The whole point of the full-tree approach: an IP literal sitting in an
	// annotation value (not any of the schema-aware field names) is caught
	// with no special-casing at all.
	t.Run("stray occurrence in an annotation is caught by the full-tree walk", func(t *testing.T) {
		rec := &capture.Record{
			ResponseBody: json.RawMessage(`{"kind":"Pod","metadata":{"annotations":{"note":"reachable at 10.1.2.3 for now"}}}`),
		}
		orig := string(rec.ResponseBody)
		// A bare exact-match alias func for this test: the annotation VALUE
		// as a whole is "reachable at 10.1.2.3 for now", which is not itself
		// a valid IP, so walkStrings won't touch it (whole-string match
		// only, not substring splicing — that's the URL category's job, not
		// IP's). This documents the boundary rather than asserting a
		// substring-splice behavior IP doesn't implement.
		changed, err := rewriteIPInRecord(rec, upper)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Error("want changed=false — IP matching is whole-string only, not substring splicing inside free text")
		}
		if string(rec.ResponseBody) != orig {
			t.Error("body must be byte-identical when nothing was rewritten")
		}
	})

	t.Run("a whole-value annotation that is exactly an IP is caught", func(t *testing.T) {
		rec := &capture.Record{
			ResponseBody: json.RawMessage(`{"kind":"Pod","metadata":{"annotations":{"external-ip":"203.0.113.5"}}}`),
		}
		changed, err := rewriteIPInRecord(rec, upper)
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
		ann := out["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
		if got := ann["external-ip"]; got != "203.0.113.5-ALIASED" {
			t.Errorf("annotations[external-ip] = %v, want 203.0.113.5-ALIASED", got)
		}
	})

	t.Run("a CIDR string is out of scope and left untouched", func(t *testing.T) {
		rec := &capture.Record{
			ResponseBody: json.RawMessage(`{"kind":"Node","spec":{"podCIDR":"10.244.0.0/16"}}`),
		}
		orig := string(rec.ResponseBody)
		changed, err := rewriteIPInRecord(rec, upper)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Error("want changed=false — a CIDR is not a bare IP literal")
		}
		if string(rec.ResponseBody) != orig {
			t.Error("body must be byte-identical when nothing was rewritten")
		}
	})

	t.Run("a record with no IP occurrence at all is left untouched", func(t *testing.T) {
		body := `{"kind":"Namespace","metadata":{"name":"prod"}}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		orig := string(rec.ResponseBody)
		changed, err := rewriteIPInRecord(rec, upper)
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

	t.Run("a Table-format body is left untouched, not an error", func(t *testing.T) {
		body := `{"kind":"Table","apiVersion":"meta.k8s.io/v1","rows":[{"cells":["10.1.2.3","Running"]}]}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		// The design plan calls out Table cells as a stray-occurrence
		// location, but Table rows[*].cells is a []interface{} of mixed
		// scalar types decoded generically — walkStrings still visits a
		// string cell like any other string leaf, so this is caught too,
		// unlike the schema-aware categories' documented Table-cell gap.
		changed, err := rewriteIPInRecord(rec, upper)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Fatal("want changed=true — a Table cell is just a string leaf to the full-tree walk")
		}
		var out map[string]interface{}
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		cells := out["rows"].([]interface{})[0].(map[string]interface{})["cells"].([]interface{})
		if got := cells[0]; got != "10.1.2.3-ALIASED" {
			t.Errorf("cells[0] = %v, want 10.1.2.3-ALIASED", got)
		}
		if got := cells[1]; got != "Running" {
			t.Errorf("cells[1] = %v, want unchanged Running", got)
		}
	})
}
