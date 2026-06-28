package diagnose

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
)

func buildDiagStore(t *testing.T, bodies map[string]string) *server.CaptureStore {
	t.Helper()
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	out := filepath.Join(t.TempDir(), "diag.kshrk")
	sw, err := archive.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	idx := capture.Index{}
	i := 0
	for path, body := range bodies {
		rec := &capture.Record{ID: path, CapturedAt: now, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body)}
		if err := sw.WriteRecord(rec); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
		idx[path] = &capture.IndexEntry{APIPath: path, Seqs: []int{0}, Times: []time.Time{now}}
		i++
	}
	meta := &capture.CaptureMetadata{
		FormatVersion: capture.CurrentFormatVersion, CaptureID: "diag-test",
		KubernetesVersion: "v1.30.0", CapturedAt: now.Add(-time.Minute), CapturedUntil: now, RecordCount: i,
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	ar, err := archive.Open(out)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	t.Cleanup(func() { ar.Close() })
	store, err := server.LoadStore(ar)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	return store
}

// fullFixture exercises every PR-1 rule plus a healthy pod (which must yield
// nothing) and two crashloopers under one owner (which must group to count=2).
func fullFixture() map[string]string {
	pods := `{"kind":"PodList","apiVersion":"v1","items":[
	  {"metadata":{"name":"web-1"},"status":{"phase":"Running","containerStatuses":[{"name":"app","ready":true,"restartCount":0,"state":{"running":{}}}]}},
	  {"metadata":{"name":"web-rs-aaa","ownerReferences":[{"kind":"ReplicaSet","name":"web-rs"}]},"status":{"phase":"Running","containerStatuses":[{"name":"app","ready":false,"restartCount":5,"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off"}}}]}},
	  {"metadata":{"name":"web-rs-bbb","ownerReferences":[{"kind":"ReplicaSet","name":"web-rs"}]},"status":{"phase":"Running","containerStatuses":[{"name":"app","ready":false,"restartCount":6,"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off"}}}]}},
	  {"metadata":{"name":"cache-1"},"status":{"phase":"Running","containerStatuses":[{"name":"c","ready":false,"restartCount":3,"state":{"terminated":{"reason":"OOMKilled","exitCode":137}}}]}},
	  {"metadata":{"name":"api-1"},"status":{"phase":"Pending","containerStatuses":[{"name":"c","ready":false,"restartCount":0,"state":{"waiting":{"reason":"ImagePullBackOff","message":"cannot pull"}}}]}},
	  {"metadata":{"name":"job-1"},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/3 nodes available: insufficient cpu"}]}}
	]}`
	pvcs := `{"kind":"PersistentVolumeClaimList","apiVersion":"v1","items":[
	  {"metadata":{"name":"data-claim"},"spec":{"storageClassName":"missing-sc"},"status":{"phase":"Pending"}}
	]}`
	nodes := `{"kind":"NodeList","apiVersion":"v1","items":[
	  {"metadata":{"name":"node-1"},"status":{"nodeInfo":{"kubeletVersion":"v1.24.0"}}}
	]}`
	return map[string]string{
		"/api/v1/namespaces/prod/pods":                   pods,
		"/api/v1/namespaces/prod/persistentvolumeclaims": pvcs,
		"/api/v1/nodes": nodes,
	}
}

func ruleIDs(fs []Finding) map[string]Finding {
	m := map[string]Finding{}
	for _, f := range fs {
		m[f.RuleID] = f
	}
	return m
}

func TestRun_AllRules(t *testing.T) {
	store := buildDiagStore(t, fullFixture())
	rep := Run(store, Options{})

	by := ruleIDs(rep.Findings)
	for _, want := range []string{
		"pod.crashloopbackoff", "pod.oomkilled", "pod.image-pull",
		"scheduling.unschedulable", "storage.pvc-unbound", "cluster.version-skew",
	} {
		if _, ok := by[want]; !ok {
			t.Errorf("missing expected finding %q; got %v", want, keys(by))
		}
	}
	// Healthy pod must not produce a finding.
	if len(rep.Findings) != 6 {
		t.Errorf("expected 6 findings, got %d: %v", len(rep.Findings), keys(by))
	}
	// Two crashloopers share an owner → grouped.
	if c := by["pod.crashloopbackoff"].Count; c != 2 {
		t.Errorf("crashloop count = %d, want 2", c)
	}
	// Severities.
	if by["pod.oomkilled"].Severity != SeverityCritical {
		t.Errorf("oomkilled severity = %q", by["pod.oomkilled"].Severity)
	}
	if by["storage.pvc-unbound"].Severity != SeverityWarning {
		t.Errorf("pvc severity = %q", by["storage.pvc-unbound"].Severity)
	}
	// Summary.
	if rep.Summary.Critical != 3 || rep.Summary.Warning != 3 {
		t.Errorf("summary = %+v, want 3 critical / 3 warning", rep.Summary)
	}
	// Sorted: critical findings come first.
	if rep.Findings[0].Severity != SeverityCritical {
		t.Errorf("first finding not critical: %+v", rep.Findings[0])
	}
}

func TestRun_SeverityFilter(t *testing.T) {
	store := buildDiagStore(t, fullFixture())
	rep := Run(store, Options{MinSeverity: SeverityCritical})
	for _, f := range rep.Findings {
		if f.Severity != SeverityCritical {
			t.Errorf("severity filter leaked %q", f.Severity)
		}
	}
	if rep.Summary.Warning != 0 || rep.Summary.Critical != 3 {
		t.Errorf("filtered summary = %+v", rep.Summary)
	}
}

func TestRun_CategoryFilter(t *testing.T) {
	store := buildDiagStore(t, fullFixture())
	rep := Run(store, Options{Category: "storage"})
	if len(rep.Findings) != 1 || rep.Findings[0].RuleID != "storage.pvc-unbound" {
		t.Errorf("category filter = %v", keys(ruleIDs(rep.Findings)))
	}
}

func TestSeverityAtLeast(t *testing.T) {
	if !SeverityAtLeast(SeverityCritical, SeverityWarning) {
		t.Error("critical should satisfy >= warning")
	}
	if SeverityAtLeast(SeverityInfo, SeverityWarning) {
		t.Error("info should not satisfy >= warning")
	}
}

func keys(m map[string]Finding) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
