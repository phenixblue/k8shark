package anonymize

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/config"
)

// excludeLogRules keeps every category's occurrences in a Pod's log: one rule
// per category, since a rule names a category.
func excludeLogRules() []config.AnonymizeRule {
	var rules []config.AnonymizeRule
	for _, c := range allCategories {
		rules = append(rules, config.AnonymizeRule{Category: string(c), Kind: "Pod", FieldPath: "log", Exclude: true})
	}
	return rules
}

// allResponseBodies returns every record body in the archive at path,
// keyed by "<apiPath>#<seq>", as compact JSON text.
func allResponseBodies(t *testing.T, path string) map[string]string {
	t.Helper()
	ar, err := archive.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	idx, err := ar.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for apiPath, entry := range idx {
		for _, seq := range entry.Seqs {
			data, err := ar.ReadRecord(apiPath, seq)
			if err != nil {
				t.Fatal(err)
			}
			var rec capture.Record
			if err := json.Unmarshal(data, &rec); err != nil {
				t.Fatal(err)
			}
			out[apiPath+"#"+string(rune('0'+seq))] = string(rec.ResponseBody)
		}
	}
	return out
}

// residualLeakFixture models what scanning real anonymized captures found
// still present after the first full-sweep: managedFields keys, short
// workload names ("api") in labels / selector cells / kind-refs / Event
// names, the API server's host:port in /api discovery, and a Node's cached
// image names.
func residualLeakFixture() []*capture.Record {
	rec := func(id, path, body string) *capture.Record {
		return &capture.Record{ID: id, CapturedAt: fixedNow, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body)}
	}
	pod := `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"api-7d9f8-x2k4z","namespace":"team-backend",
		"labels":{"app":"api"},
		"ownerReferences":[{"kind":"ReplicaSet","name":"api-7d9f8"}],
		"managedFields":[{"manager":"kubelet","fieldsV1":{
			"f:spec":{"f:containers":{"k:{\"name\":\"api\"}":{".":{},"f:image":{}}}},
			"f:status":{"f:podIPs":{"k:{\"ip\":\"10.244.0.20\"}":{".":{},"f:ip":{}}}}}}]},
		"spec":{"nodeName":"worker-1","containers":[{"name":"api","image":"registry.internal.corp/team/api:v1"}]},
		"status":{"podIP":"10.244.0.20","podIPs":[{"ip":"10.244.0.20"}]}}`
	deploy := `{"kind":"Deployment","apiVersion":"apps/v1","metadata":{"name":"api","namespace":"team-backend"},
		"spec":{"selector":{"matchLabels":{"app":"api"}}}}`
	node := `{"kind":"Node","apiVersion":"v1","metadata":{"name":"worker-1","labels":{"kubernetes.io/hostname":"worker-1"}},
		"status":{"addresses":[{"type":"InternalIP","address":"172.18.0.2"},{"type":"Hostname","address":"worker-1"}],
		"images":[{"names":["registry.internal.corp/team/api@sha256:abc","registry.internal.corp/team/api:v1"]}]}}`
	ns := `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"team-backend"}}`
	discovery := `{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[{"clientCIDR":"0.0.0.0/0","serverAddress":"172.18.0.2:6443"}]}`
	deployTable := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
		"columnDefinitions":[{"name":"Name"},{"name":"Selector"},{"name":"Owner"}],
		"rows":[{"cells":["api","app=api,tier=web","deployment/api"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",
			"metadata":{"name":"api","namespace":"team-backend"}}}]}`
	events := `{"kind":"EventList","apiVersion":"v1","items":[{"kind":"Event","metadata":{"name":"api.18c4b942c8a33b9c","namespace":"team-backend"},
		"involvedObject":{"kind":"Deployment","name":"api","namespace":"team-backend"},
		"message":"Scaled up replica set api-7d9f8 from 0 to 1"}]}`
	// A pod's log is captured as plain text stored as a JSON *string*, not an
	// object — the most name/IP/URL-dense text in a capture, and the one
	// shape every object-decoding matcher used to skip.
	logText, _ := json.Marshal("2026-07-22T21:03:25Z INFO starting on node worker-1 as pod api-7d9f8-x2k4z in team-backend\n" +
		"2026-07-22T21:03:26Z INFO listening on 10.244.0.20:8080, upstream https://internal.corp.example/v1\n")
	return []*capture.Record{
		rec("r1", "/api/v1/namespaces/team-backend/pods/api-7d9f8-x2k4z", pod),
		rec("r8", "/api/v1/namespaces/team-backend/pods/api-7d9f8-x2k4z/log?container=api", string(logText)),
		rec("r2", "/apis/apps/v1/namespaces/team-backend/deployments/api", deploy),
		rec("r3", "/api/v1/nodes/worker-1", node),
		rec("r4", "/api/v1/namespaces/team-backend", ns),
		rec("r5", "/api", discovery),
		rec("r6", "/apis/apps/v1/namespaces/team-backend/deployments?as=Table", deployTable),
		rec("r7", "/api/v1/namespaces/team-backend/events", events),
	}
}

var allCategories = []Category{
	CategoryNamespace, CategoryNode, CategoryPod, CategoryWorkload,
	CategoryIP, CategoryURL, CategoryImage,
}

// Every original that could identify the cluster, in the exact textual
// forms the real captures leaked it in.
var residualLeakForms = []struct{ what, form string }{
	{"namespace", "team-backend"},
	{"node name", "worker-1"},
	{"pod name", "api-7d9f8-x2k4z"},
	{"replicaset name", "api-7d9f8"},
	{"pod IP (incl. managedFields key)", "10.244.0.20"},
	{"node IP / API server host", "172.18.0.2"},
	{"image registry", "registry.internal.corp"},
	{"short workload name as a label value", `"app":"api"`},
	{"short workload name in a managedFields key", `\"name\":\"api\"`},
	{"short workload name in a selector cell", "app=api"},
	{"short workload name as a kind/name ref", "deployment/api"},
	{"short workload name as an Event name", "api.18c4b942c8a33b9c"},
	{"short workload name as a bare cell / involvedObject name", `"api"`},
	{"hostname in a pod log (scheme://host)", "internal.corp.example"},
}

func assertNoLeaks(t *testing.T, path string) {
	t.Helper()
	for rec, body := range allResponseBodies(t, path) {
		for _, l := range residualLeakForms {
			if strings.Contains(body, l.form) {
				t.Errorf("%s: %s still present (%s):\n  %s", rec, l.what, l.form, body)
			}
		}
	}
}

// With --full-sweep, nothing identifying survives anywhere in the archive —
// including keys, short names, host:port, and a Node's image cache.
func TestArchive_FullSweepLeavesNoResidualLeaks(t *testing.T) {
	src := buildAnonymizeTestArchive(t, residualLeakFixture())
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	if _, err := Archive(src, dst, Options{Categories: allCategories, Salt: []byte("residual-leaks-salt"), FullSweep: true}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	assertNoLeaks(t, dst)

	// ...and structure survived: the managedFields schema keys, the Pod's
	// own fields, and discovery's port.
	bodies := allResponseBodies(t, dst)
	var pod, disc string
	for k, v := range bodies {
		switch {
		case strings.Contains(v, `"kind":"Pod"`):
			pod = v
		case strings.Contains(v, `"kind":"APIVersions"`):
			disc = v
		}
		_ = k
	}
	for _, structural := range []string{`"f:spec"`, `"f:containers"`, `"f:podIPs"`, `"f:ip"`, `"f:image"`, `"managedFields"`} {
		if !strings.Contains(pod, structural) {
			t.Errorf("structural key %s missing from the swept Pod: %s", structural, pod)
		}
	}
	if !strings.Contains(disc, `:6443"`) {
		t.Errorf("discovery lost its port: %s", disc)
	}
}

// Without --full-sweep, the two fixes that don't depend on it must still
// hold: the API server's host:port (ip category) and a Node's image cache
// (image category).
func TestArchive_NonSweepFixesWork(t *testing.T) {
	src := buildAnonymizeTestArchive(t, residualLeakFixture())
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	if _, err := Archive(src, dst, Options{Categories: allCategories, Salt: []byte("residual-leaks-salt-nosweep")}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	for rec, body := range allResponseBodies(t, dst) {
		if strings.Contains(body, "172.18.0.2") {
			t.Errorf("%s: API server / node IP still present without a sweep: %s", rec, body)
		}
		if strings.Contains(body, "registry.internal.corp") {
			t.Errorf("%s: image registry still present without a sweep: %s", rec, body)
		}
	}
}

// The log record above must actually have been rewritten (not merely absent
// of originals because the fixture changed), and must stay a JSON string.
func TestArchive_PodLogTextIsRewrittenAndStaysAString(t *testing.T) {
	src := buildAnonymizeTestArchive(t, residualLeakFixture())
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	if _, err := Archive(src, dst, Options{Categories: allCategories, Salt: []byte("pod-log-salt"), FullSweep: true, Verify: true}); err != nil {
		t.Fatal(err)
	}
	found := 0
	for k, body := range allResponseBodies(t, dst) {
		if !strings.Contains(k, "/log?container=") {
			continue
		}
		found++
		var text string
		if err := json.Unmarshal([]byte(body), &text); err != nil {
			t.Fatalf("%s: a log body must remain a JSON string, got %s", k, body)
		}
		if !strings.Contains(text, "INFO starting on node ") || !strings.Contains(text, ":8080, upstream https://") {
			t.Errorf("log text lost its structure: %q", text)
		}
	}
	if found != 1 {
		t.Fatalf("found %d log records, want 1", found)
	}
}

// Without a sweep, only the URL matcher can reach log text (it looks for
// scheme://host anywhere in any string); discovered names need --full-sweep.
func TestArchive_PodLogURLHostRewrittenWithoutSweep(t *testing.T) {
	src := buildAnonymizeTestArchive(t, residualLeakFixture())
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	if _, err := Archive(src, dst, Options{Categories: allCategories, Salt: []byte("pod-log-salt")}); err != nil {
		t.Fatal(err)
	}
	for k, body := range allResponseBodies(t, dst) {
		if strings.Contains(k, "/log?container=") && strings.Contains(body, "internal.corp.example") {
			t.Errorf("%s: URL host survived in log text without a sweep: %s", k, body)
		}
	}
}

// An exclude rule scoped to a Pod's log keeps it, and the leak check respects
// the same rule rather than reporting a deliberate keep.
func TestArchive_PodLogExcludeRuleKeepsItAndIsNotReportedAsLeak(t *testing.T) {
	src := buildAnonymizeTestArchive(t, residualLeakFixture())
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	res, err := Archive(src, dst, Options{
		Categories: allCategories, Salt: []byte("pod-log-salt"), FullSweep: true, Verify: true,
		Rules: excludeLogRules(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ResidualOccurrences != 0 {
		t.Errorf("residual = %d %+v, want 0: an excluded log is a deliberate keep", res.ResidualOccurrences, res.ResidualLocations)
	}
	kept := false
	for k, body := range allResponseBodies(t, dst) {
		if strings.Contains(k, "/log?container=") && strings.Contains(body, "worker-1") && strings.Contains(body, "internal.corp.example") {
			kept = true
		}
	}
	if !kept {
		t.Error("the excluded log should still carry its original text")
	}
}
