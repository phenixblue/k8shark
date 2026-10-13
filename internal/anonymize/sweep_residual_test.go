package anonymize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/phenixblue/k8shark/internal/capture"
)

// Locates the single occurrence of needle in s and reports whether ctx
// accepts it, so each case below reads as "does this string qualify".
func ctxAt(t *testing.T, ctx func(string, int, int) bool, s, needle string) bool {
	t.Helper()
	i := strings.Index(s, needle)
	if i < 0 {
		t.Fatalf("%q not in %q", needle, s)
	}
	return ctx(s, i, i+len(needle))
}

func TestIPContext(t *testing.T) {
	cases := []struct {
		s, ip string
		want  bool
	}{
		{"172.18.0.2:6443", "172.18.0.2", true}, // host:port — the /api serverAddress leak
		{"ip:10.1.2.3", "10.1.2.3", true},
		{"dialing 10.1.2.3 timed out", "10.1.2.3", true},
		{"dialing 10.1.2.3.", "10.1.2.3", true},       // sentence-ending period
		{"10.1.2.3/24", "10.1.2.3", true},             // CIDR-style suffix
		{"110.1.2.3 is unrelated", "10.1.2.3", false}, // truncated fragment
		{"10.1.2.34", "10.1.2.3", false},
		{"10.1.2.3.4", "10.1.2.3", false},
		{"x.10.1.2.3", "10.1.2.3", false},
		// IPv6 stays strict: "::1" must not match inside "fe80::1".
		{"connecting to fe80::1 now", "::1", false},
		{"connecting to ::1 now", "::1", true},
	}
	for _, tc := range cases {
		if got := ctxAt(t, ipContext, tc.s, tc.ip); got != tc.want {
			t.Errorf("ipContext(%q, %q) = %v, want %v", tc.s, tc.ip, got, tc.want)
		}
	}
}

func TestShortNameContext(t *testing.T) {
	cases := []struct {
		name, s, needle string
		want            bool
	}{
		{"whole leaf (a label value)", "api", "api", true},
		{"quoted, as in a managedFields key", `k:{"name":"api"}`, "api", true},
		{"single-quoted", "pod 'api' failed", "api", true},
		{"kubectl kind/name ref", "deployment/api", "api", true},
		{"kind.group/name ref", "replicaset.apps/api", "api", true},
		{"event generated name", "api.18c4b942c8a33b9c", "api", true},
		{"selector cell (kubectl SELECTOR column)", "app=api", "api", true},
		{"selector list", "app=api,tier=web", "api", true},
		{"comma-separated list", "web,api,db", "api", true},
		{"query-style value", "name=api&x=1", "api", true},
		{"word in free text", "Scaled up api now", "api", true},
		{"word before trailing punctuation", "Created api, then web", "api", true},
		{"word at sentence end", "Scaled up api.", "api", true},
		// Structural text that merely contains the letters must never match.
		{"URL path", "/api/v1/pods", "api", false},
		{"hyphenated compound", "kube-api-access-6c5pr", "api", false},
		{"dotted identifier", "io.k8s.api.core.v1.Pod", "api", false},
		{"inside a longer word", "capital letters", "api", false},
		{"no whitespace, not a ref", "foo/api/bar", "api", false},
		{"slash ref with a non-kind prefix", "/etc/api", "api", false},
		{"short hex tail isn't an event name", "api.abc", "api", false},
		{"name followed by hyphen in prose", "see api-gateway docs", "api", false},
		{"selector value that is only a prefix", "app=api-gateway", "api", false},
		{"underscore compound", "my_api_x", "api", false},
	}
	for _, tc := range cases {
		if got := ctxAt(t, shortNameContext, tc.s, tc.needle); got != tc.want {
			t.Errorf("%s: shortNameContext(%q) = %v, want %v", tc.name, tc.s, got, tc.want)
		}
	}
}

func TestBuildSweepCandidates_ShortNamesRouteToShortGroup(t *testing.T) {
	cs, err := buildSweepCandidates(
		emptyTracker(CategoryNamespace), emptyTracker(CategoryNode), emptyTracker(CategoryPod),
		trackerWith(CategoryWorkload, upper, "api", "db", "x", "orders-api"),
		emptyTracker(CategoryIP), emptyTracker(CategoryURL))
	if err != nil {
		t.Fatal(err)
	}
	if cs.shortGroup == nil || cs.nameGroup == nil {
		t.Fatalf("shortGroup=%v nameGroup=%v, want both non-nil", cs.shortGroup, cs.nameGroup)
	}
	for _, want := range []string{"api", "db"} {
		if _, ok := cs.shortGroup.byOriginal[want]; !ok {
			t.Errorf("%q missing from shortGroup", want)
		}
	}
	if _, ok := cs.shortGroup.byOriginal["x"]; ok {
		t.Error("a 1-character name must be ignored entirely")
	}
	if _, ok := cs.nameGroup.byOriginal["api"]; ok {
		t.Error("a 3-character name must not be in the plain-substring group")
	}
	if _, ok := cs.nameGroup.byOriginal["orders-api"]; !ok {
		t.Error("orders-api missing from nameGroup")
	}
}

func TestSpliceCandidatesIn_KeyMode(t *testing.T) {
	cands := []sweepCandidate{
		{Category: CategoryWorkload, Original: "redis", Alias: "ALIASED"},
		{Category: CategoryNamespace, Original: "data", Alias: "NS"},
	}
	g, err := buildCandidateGroup(cands, nameBoundaryReject)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, key, want string
	}{
		{"embedded in a managedFields k: key", `k:{"name":"redis"}`, `k:{"name":"ALIASED"}`},
		{"a key that is exactly a candidate is structural, not renamed", "data", "data"},
		{"f: schema field names are never object names", "f:data", "f:data"},
		{"k: with the same name is still swept", `k:{"name":"data"}`, `k:{"name":"NS"}`},
		{"substring of a longer key (ConfigMap data key)", "redis.conf", "ALIASED.conf"},
	}
	for _, tc := range cases {
		got, _, _ := spliceCandidatesIn(tc.key, g, noExclusions, "Pod", "metadata.managedFields", true)
		if got != tc.want {
			t.Errorf("%s: %q -> %q, want %q", tc.name, tc.key, got, tc.want)
		}
	}
	// The same strings as *values* aren't subject to either key-only rule.
	if got, _, _ := spliceCandidatesIn("data", g, noExclusions, "ConfigMap", "x", false); got != "NS" {
		t.Errorf("value %q -> %q, want NS (whole-value matches are fine for values)", "data", got)
	}
}

// maskAlias replaces every digit with D and upper-cases letters behind an M
// prefix, so an alias never contains its original as a substring (the
// package's `upper` helper appends instead, which would hide a leak from a
// "no original left" assertion).
func maskAlias(s string) string {
	return "M" + strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9':
			return 'D'
		case r >= 'a' && r <= 'z':
			return r - 32
		}
		return r
	}, s)
}

func sweepSet(t *testing.T, ns, node, pod, workload, ip []string) *sweepCandidateSet {
	t.Helper()
	cs, err := buildSweepCandidates(
		trackerWith(CategoryNamespace, maskAlias, ns...), trackerWith(CategoryNode, maskAlias, node...),
		trackerWith(CategoryPod, maskAlias, pod...), trackerWith(CategoryWorkload, maskAlias, workload...),
		trackerWith(CategoryIP, maskAlias, ip...), emptyTracker(CategoryURL))
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func sweepJSON(t *testing.T, apiPath, body string, cs *sweepCandidateSet, excluded excludedFunc) string {
	t.Helper()
	rec := &capture.Record{APIPath: apiPath, ResponseBody: json.RawMessage(body)}
	if _, _, err := sweepRecord(rec, cs, excluded); err != nil {
		t.Fatal(err)
	}
	return string(rec.ResponseBody)
}

// What the residual-leak scan found left in a fully swept archive:
// managedFields keys carrying pod IPs and container names.
func TestSweepRecord_ManagedFieldsKeys(t *testing.T) {
	cs := sweepSet(t, nil, nil, nil, []string{"redis"}, []string{"10.244.0.20"})
	body := `{"kind":"Pod","metadata":{"name":"x","managedFields":[{"fieldsV1":{
		"f:spec":{"f:containers":{"k:{\"name\":\"redis\"}":{".":{},"f:image":{}}}},
		"f:status":{"f:podIPs":{"k:{\"ip\":\"10.244.0.20\"}":{".":{},"f:ip":{}}}}}}]}}`
	out := sweepJSON(t, "/api/v1/namespaces/ns/pods/x", body, cs, noExclusions)
	for _, leak := range []string{"redis", "10.244.0.20"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q still present: %s", leak, out)
		}
	}
	for _, structural := range []string{`"f:spec"`, `"f:containers"`, `"f:podIPs"`, `"f:ip"`, `"f:image"`} {
		if !strings.Contains(out, structural) {
			t.Errorf("structural key %s was damaged: %s", structural, out)
		}
	}
	if !strings.Contains(out, `MREDIS`) || !strings.Contains(out, `MDD.DDD.D.DD`) {
		t.Errorf("expected the aliases in the keys: %s", out)
	}
}

func TestSweepRecord_ShortNames(t *testing.T) {
	cs := sweepSet(t, nil, nil, nil, []string{"api"}, nil)
	body := `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"x","labels":{"app":"api"},
		"annotations":{"note":"scaled api now"}},
		"spec":{"volumes":[{"name":"kube-api-access-6c5pr"}],"path":"/api/v1/pods","dotted":"io.k8s.api.core"}}`
	out := sweepJSON(t, "/api/v1/namespaces/ns/pods/x", body, cs, noExclusions)
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	meta := got["metadata"].(map[string]interface{})
	if meta["labels"].(map[string]interface{})["app"] != "MAPI" {
		t.Errorf("label app = %v, want aliased", meta["labels"])
	}
	if meta["annotations"].(map[string]interface{})["note"] != "scaled MAPI now" {
		t.Errorf("annotation = %v, want the free-text word aliased", meta["annotations"])
	}
	spec := got["spec"].(map[string]interface{})
	if spec["volumes"].([]interface{})[0].(map[string]interface{})["name"] != "kube-api-access-6c5pr" {
		t.Errorf("volume name was mangled: %v", spec["volumes"])
	}
	if spec["path"] != "/api/v1/pods" || spec["dotted"] != "io.k8s.api.core" {
		t.Errorf("structural strings were mangled: path=%v dotted=%v", spec["path"], spec["dotted"])
	}
	if got["apiVersion"] != "v1" {
		t.Errorf("apiVersion = %v", got["apiVersion"])
	}
}

func TestSweepRecord_ShortNamesInTableAndEvents(t *testing.T) {
	cs := sweepSet(t, nil, nil, nil, []string{"api"}, nil)
	table := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
		"columnDefinitions":[{"name":"Name","description":"the api server's name for it"}],
		"rows":[{"cells":["deployment/api","api"],"object":{"kind":"PartialObjectMetadata","metadata":{"name":"api.18c4b942c8a33b9c"}}}]}`
	out := sweepJSON(t, "/apis/apps/v1/deployments?as=Table", table, cs, noExclusions)
	if strings.Contains(out, `"deployment/api"`) || strings.Contains(out, `"api"`) || strings.Contains(out, "api.18c4") {
		t.Errorf("short name leaked through a Table: %s", out)
	}
	if !strings.Contains(out, "the api server's name for it") {
		t.Errorf("columnDefinitions prose must not be swept for short names: %s", out)
	}
}

// OpenAPI documents hold no cluster data; sweeping their prose would only
// mangle schema text (and each "api" in a description is an English word).
func TestSweepRecord_SkipsOpenAPIDocuments(t *testing.T) {
	cs := sweepSet(t, nil, nil, nil, []string{"api", "redis"}, []string{"127.0.0.1"})
	body := `{"swagger":"2.0","definitions":{"io.k8s.api.core.v1.Pod":{"description":"the api, for example 127.0.0.1 or redis"}}}`
	for _, p := range []string{"/openapi/v2", "/openapi/v3/apis/apps/v1"} {
		rec := &capture.Record{APIPath: p, ResponseBody: json.RawMessage(body)}
		changed, _, err := sweepRecord(rec, cs, noExclusions)
		if err != nil || changed || string(rec.ResponseBody) != body {
			t.Errorf("%s: changed=%v err=%v, want the document untouched", p, changed, err)
		}
	}
}

// /api discovery's real serverAddress, "<ip>:<port>", used to survive even a
// full sweep: ipBoundaryReject refuses a ':' neighbor (right for IPv6, wrong
// for an IPv4 literal, which has no colons of its own).
func TestSweepRecord_HostPort(t *testing.T) {
	cs := sweepSet(t, nil, nil, nil, nil, []string{"172.18.0.2"})
	out := sweepJSON(t, "/api", `{"kind":"APIVersions","serverAddressByClientCIDRs":[{"clientCIDR":"0.0.0.0/0","serverAddress":"172.18.0.2:6443"}]}`, cs, noExclusions)
	if strings.Contains(out, "172.18.0.2") || !strings.Contains(out, "MDDD.DD.D.D:6443") {
		t.Errorf("serverAddress = %s, want the host aliased and the port kept", out)
	}
}

func TestSweepRecord_KeyRenameCollisionFailsLoudly(t *testing.T) {
	cs := sweepSet(t, nil, nil, nil, []string{"redis"}, nil)
	// Distinct originals never alias to the same string, so force the only
	// way a rename can collide: a key that already equals another key's
	// post-rename name.
	body := `{"kind":"ConfigMap","data":{"a.redis":"1","a.MREDIS":"2"}}`
	rec := &capture.Record{ResponseBody: json.RawMessage(body)}
	if _, _, err := sweepRecord(rec, cs, noExclusions); err == nil {
		t.Error("want an error rather than silently dropping one of two colliding keys")
	}
}

func TestSweepRecord_ExcludeRuleCoversKeyAtThatPath(t *testing.T) {
	cs := sweepSet(t, nil, nil, nil, []string{"redis"}, nil)
	body := `{"kind":"ConfigMap","data":{"redis.conf":"x"}}`
	excluded := func(cat Category, kind, path string) bool {
		return cat == CategoryWorkload && kind == "ConfigMap" && path == "data.redis.conf"
	}
	out := sweepJSON(t, "/api/v1/namespaces/ns/configmaps/c", body, cs, excluded)
	if !strings.Contains(out, `"redis.conf"`) {
		t.Errorf("an exclude rule naming the field's path must also keep its key: %s", out)
	}
}
