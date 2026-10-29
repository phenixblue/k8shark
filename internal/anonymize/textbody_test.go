package anonymize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/phenixblue/k8shark/internal/capture"
)

func TestStringBody(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		ok               bool
	}{
		{"a JSON string (how a pod log is stored)", `"line one\nline two\n"`, "line one\nline two\n", true},
		{"surrounding whitespace", "  \"x\"\n", "x", true},
		{"an object", `{"kind":"Pod"}`, "", false},
		{"an array", `["a"]`, "", false},
		{"a number", `42`, "", false},
		{"empty", ``, "", false},
		{"a truncated string", `"abc`, "", false},
	} {
		got, ok := stringBody(json.RawMessage(tc.body))
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: stringBody(%q) = (%q, %v), want (%q, %v)", tc.name, tc.body, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTextBodyKind(t *testing.T) {
	if got := textBodyKind("/api/v1/namespaces/ns/pods/p/log?container=c"); got != "Pod" {
		t.Errorf("pod log kind = %q, want Pod", got)
	}
	if got := textBodyKind("/api/v1/something"); got != "" {
		t.Errorf("non-pod kind = %q, want empty", got)
	}
}

func TestRewriteURLInRecord_PodLogText(t *testing.T) {
	body, _ := json.Marshal("connecting to https://db.internal.corp:5432/x and http://10.1.2.3/y\n")
	rec := &capture.Record{APIPath: "/api/v1/namespaces/n/pods/p/log?container=c", ResponseBody: body}
	changed, err := rewriteURLInRecord(rec, noExclusions, upper)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v, want true, nil", changed, err)
	}
	var text string
	if err := json.Unmarshal(rec.ResponseBody, &text); err != nil {
		t.Fatalf("a log body must stay a JSON string: %v (%s)", err, rec.ResponseBody)
	}
	if want := "connecting to https://db.internal.corp-ALIASED:5432/x and http://10.1.2.3-ALIASED/y\n"; text != want {
		t.Errorf("text = %q, want %q", text, want)
	}

	t.Run("an exclude rule on the Pod's log keeps it", func(t *testing.T) {
		rec := &capture.Record{APIPath: "/api/v1/namespaces/n/pods/p/log?container=c", ResponseBody: body}
		excluded := func(cat Category, kind, path string) bool {
			return cat == CategoryURL && kind == "Pod" && path == "log"
		}
		if changed, _ := rewriteURLInRecord(rec, excluded, upper); changed {
			t.Error("want changed=false")
		}
	})
	t.Run("a log with no URL is left byte-identical", func(t *testing.T) {
		plain, _ := json.Marshal("just text <and> & symbols\n")
		rec := &capture.Record{APIPath: "/api/v1/namespaces/n/pods/p/log?container=c", ResponseBody: plain}
		if changed, _ := rewriteURLInRecord(rec, noExclusions, upper); changed || string(rec.ResponseBody) != string(plain) {
			t.Errorf("changed=%v body=%s, want untouched", changed, rec.ResponseBody)
		}
	})
}

func TestSweepRecord_PodLogText(t *testing.T) {
	cs := sweepSet(t, []string{"team-backend"}, []string{"worker-1"}, nil, nil, []string{"10.244.0.20"})
	body, _ := json.Marshal("pod on worker-1 in team-backend at 10.244.0.20:8080\n")
	rec := &capture.Record{APIPath: "/api/v1/namespaces/n/pods/p/log?container=c", ResponseBody: body}
	changed, n, err := sweepRecord(rec, cs, noExclusions)
	if err != nil || !changed || n != 3 {
		t.Fatalf("changed=%v n=%d err=%v, want true, 3, nil", changed, n, err)
	}
	var text string
	if err := json.Unmarshal(rec.ResponseBody, &text); err != nil {
		t.Fatal(err)
	}
	for _, orig := range []string{"worker-1", "team-backend", "10.244.0.20"} {
		if strings.Contains(text, orig) {
			t.Errorf("%q survived: %q", orig, text)
		}
	}
	if !strings.HasSuffix(text, ":8080\n") {
		t.Errorf("the port/newline were damaged: %q", text)
	}
}

func TestSweepPathQuery(t *testing.T) {
	cs := sweepSet(t, nil, nil, nil, []string{"orders-api", "api"}, nil)
	for _, tc := range []struct{ in, want string }{
		{"/api/v1/namespaces/n/pods/p/log?container=orders-api", "/api/v1/namespaces/n/pods/p/log?container=MORDERS-API"},
		{"/api/v1/namespaces/n/pods/p/log?container=api&previous=true", "/api/v1/namespaces/n/pods/p/log?container=MAPI&previous=true"},
		{"/api/v1/pods", "/api/v1/pods"},                   // no query: the path itself is the path rewriters' job
		{"/api/v1/pods?as=Table", "/api/v1/pods?as=Table"}, // nothing in the query is a name
		{"/api/v1/namespaces/n/pods/p/log?container=sidecar", "/api/v1/namespaces/n/pods/p/log?container=sidecar"},
	} {
		if got := sweepPathQuery(tc.in, cs, noExclusions); got != tc.want {
			t.Errorf("sweepPathQuery(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := sweepPathQuery("/x?container=api", nil, noExclusions); got != "/x?container=api" {
		t.Errorf("nil candidates must be a no-op, got %q", got)
	}
}
