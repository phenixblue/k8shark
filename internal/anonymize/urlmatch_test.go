package anonymize

import (
	"encoding/json"
	"testing"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestSpliceURLHosts(t *testing.T) {
	cases := []struct {
		name   string
		s      string
		want   string
		wantOK bool
	}{
		{
			name:   "https URL with port and path — only the host moves",
			s:      "https://webhook.example.com:8443/validate",
			want:   "https://webhook.example.com-ALIASED:8443/validate",
			wantOK: true,
		},
		{
			name:   "bare in-cluster service name, no dots",
			s:      "https://webhook-svc:8443/validate",
			want:   "https://webhook-svc-ALIASED:8443/validate",
			wantOK: true,
		},
		{
			name:   "http, no port, no path",
			s:      "http://example.com",
			want:   "http://example.com-ALIASED",
			wantOK: true,
		},
		{
			name:   "embedded in a longer sentence",
			s:      "see https://status.example.com/health for details",
			want:   "see https://status.example.com-ALIASED/health for details",
			wantOK: true,
		},
		{
			name:   "two distinct URLs in one string, each aliased independently",
			s:      "primary https://a.example.com, backup https://b.example.com",
			want:   "primary https://a.example.com-ALIASED, backup https://b.example.com-ALIASED",
			wantOK: true,
		},
		{
			name:   "URL with userinfo — the host is aliased, not the username",
			s:      "https://user:pass@host.example.com/path",
			want:   "https://user:pass@host.example.com-ALIASED/path",
			wantOK: true,
		},
		{
			name:   "a literal '@' inside the path is not mistaken for userinfo",
			s:      "https://host.example.com/path@notuserinfo",
			want:   "https://host.example.com-ALIASED/path@notuserinfo",
			wantOK: true,
		},
		{
			name:   "bracketed IPv6 literal host — brackets are removed, not preserved",
			s:      "https://[fd00::1]:6443/api",
			want:   "https://fd00::1-ALIASED:6443/api",
			wantOK: true,
		},
		{
			name:   "bracketed IPv6 literal, no port",
			s:      "wss://[2001:db8::5678]/ws",
			want:   "wss://2001:db8::5678-ALIASED/ws",
			wantOK: true,
		},
		{
			name:   "no scheme at all — not matched, by design",
			s:      "webhook-svc.default.svc.cluster.local",
			want:   "webhook-svc.default.svc.cluster.local",
			wantOK: false,
		},
		{
			name:   "no URL anywhere",
			s:      "just some plain text",
			want:   "just some plain text",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := spliceURLHosts(tc.s, upper)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("spliceURLHosts(%q) = (%q, %v), want (%q, %v)", tc.s, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestRewriteURLInObject(t *testing.T) {
	t.Run("Ingress aliases spec.rules[*].host and spec.tls[*].hosts[*]", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind": "Ingress",
			"spec": map[string]interface{}{
				"rules": []interface{}{
					map[string]interface{}{"host": "app.example.com"},
				},
				"tls": []interface{}{
					map[string]interface{}{"hosts": []interface{}{"app.example.com", "www.example.com"}},
				},
			},
		}
		if !rewriteURLInObject(obj, "Ingress", upper) {
			t.Fatal("want modified=true")
		}
		spec := obj["spec"].(map[string]interface{})
		rule := spec["rules"].([]interface{})[0].(map[string]interface{})
		if got := rule["host"]; got != "app.example.com-ALIASED" {
			t.Errorf("rules[0].host = %v, want app.example.com-ALIASED", got)
		}
		tlsHosts := spec["tls"].([]interface{})[0].(map[string]interface{})["hosts"].([]interface{})
		if got := tlsHosts[0]; got != "app.example.com-ALIASED" {
			t.Errorf("tls[0].hosts[0] = %v, want app.example.com-ALIASED", got)
		}
		if got := tlsHosts[1]; got != "www.example.com-ALIASED" {
			t.Errorf("tls[0].hosts[1] = %v, want www.example.com-ALIASED", got)
		}
	})

	t.Run("Service aliases spec.externalName", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind": "Service",
			"spec": map[string]interface{}{"externalName": "db.upstream.example.com", "type": "ExternalName"},
		}
		if !rewriteURLInObject(obj, "Service", upper) {
			t.Fatal("want modified=true")
		}
		spec := obj["spec"].(map[string]interface{})
		if got := spec["externalName"]; got != "db.upstream.example.com-ALIASED" {
			t.Errorf("spec.externalName = %v, want db.upstream.example.com-ALIASED", got)
		}
		if got := spec["type"]; got != "ExternalName" {
			t.Errorf("spec.type = %v, want unchanged ExternalName", got)
		}
	})

	t.Run("a ClusterIP Service with no externalName is left alone", func(t *testing.T) {
		obj := map[string]interface{}{
			"kind": "Service",
			"spec": map[string]interface{}{"clusterIP": "10.0.0.5"},
		}
		if rewriteURLInObject(obj, "Service", upper) {
			t.Error("want modified=false")
		}
	})

	t.Run("an object with no spec at all is left alone, not a crash", func(t *testing.T) {
		obj := map[string]interface{}{"kind": "Status"}
		if rewriteURLInObject(obj, "Status", upper) {
			t.Error("want modified=false")
		}
	})
}

func TestRewriteURLInRecord(t *testing.T) {
	t.Run("schema-aware field plus a full-tree URL in an annotation, in the same record", func(t *testing.T) {
		body := `{"kind":"Ingress","metadata":{"annotations":{"cert-manager.io/issuer-url":"https://acme.example.com/directory"}},
			"spec":{"rules":[{"host":"app.example.com"}]}}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		changed, err := rewriteURLInRecord(rec, upper)
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
		rule := out["spec"].(map[string]interface{})["rules"].([]interface{})[0].(map[string]interface{})
		if got := rule["host"]; got != "app.example.com-ALIASED" {
			t.Errorf("spec.rules[0].host = %v, want app.example.com-ALIASED", got)
		}
		ann := out["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
		if got := ann["cert-manager.io/issuer-url"]; got != "https://acme.example.com-ALIASED/directory" {
			t.Errorf("annotation URL = %v, want host-only splice", got)
		}
	})

	t.Run("list response rewrites every item, using each item's own kind", func(t *testing.T) {
		body := `{"kind":"IngressList","items":[
			{"metadata":{"name":"a"},"spec":{"rules":[{"host":"a.example.com"}]}},
			{"metadata":{"name":"b"},"spec":{"rules":[{"host":"b.example.com"}]}}
		]}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		changed, err := rewriteURLInRecord(rec, upper)
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
		items := out["items"].([]interface{})
		host0 := items[0].(map[string]interface{})["spec"].(map[string]interface{})["rules"].([]interface{})[0].(map[string]interface{})["host"]
		host1 := items[1].(map[string]interface{})["spec"].(map[string]interface{})["rules"].([]interface{})[0].(map[string]interface{})["host"]
		if host0 != "a.example.com-ALIASED" || host1 != "b.example.com-ALIASED" {
			t.Errorf("item hosts = %q, %q", host0, host1)
		}
	})

	t.Run("a record with no URL occurrence at all is left untouched", func(t *testing.T) {
		body := `{"kind":"Namespace","metadata":{"name":"prod"}}`
		rec := &capture.Record{ResponseBody: json.RawMessage(body)}
		orig := string(rec.ResponseBody)
		changed, err := rewriteURLInRecord(rec, upper)
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
