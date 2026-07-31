package inspect

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

func buildArchive(t *testing.T, records []*capture.Record) string {
	t.Helper()
	dir := t.TempDir()

	idx := capture.Index{}
	for _, r := range records {
		if e, ok := idx[r.APIPath]; ok {
			seq := len(e.Seqs)
			e.Seqs = append(e.Seqs, seq)
			e.Times = append(e.Times, r.CapturedAt)
		} else {
			idx[r.APIPath] = &capture.IndexEntry{
				APIPath: r.APIPath,
				Seqs:    []int{0},
				Times:   []time.Time{r.CapturedAt},
			}
		}
	}

	meta := &capture.CaptureMetadata{
		CaptureID:         "inspect-test-id",
		CapturedAt:        time.Date(2026, 4, 9, 8, 0, 0, 0, time.UTC),
		CapturedUntil:     time.Date(2026, 4, 9, 8, 10, 0, 0, time.UTC),
		KubernetesVersion: "v1.29.0",
		ServerAddress:     "https://127.0.0.1:6443",
		RecordCount:       len(records),
	}

	outPath := filepath.Join(dir, "test.kshrk")
	sw, err := archive.NewStreamWriter(outPath)
	if err != nil {
		t.Fatalf("buildArchive NewStreamWriter: %v", err)
	}
	for _, r := range records {
		if _, err := sw.WriteRecord(r); err != nil {
			t.Fatalf("buildArchive WriteRecord: %v", err)
		}
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("buildArchive Finish: %v", err)
	}
	return outPath
}

func rec(id, apiPath string) *capture.Record {
	body, _ := json.Marshal(map[string]any{"kind": "List", "items": []any{}})
	return &capture.Record{
		ID:           id,
		APIPath:      apiPath,
		CapturedAt:   time.Now(),
		ResponseCode: 200,
		ResponseBody: body,
	}
}

func secretRec(id, ns, name string) *capture.Record {
	val := base64.StdEncoding.EncodeToString([]byte("s3cr3t"))
	body, _ := json.Marshal(map[string]any{
		"kind":       "Secret",
		"apiVersion": "v1",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"data":       map[string]string{"password": val},
	})
	return &capture.Record{
		ID:           id,
		APIPath:      "/api/v1/namespaces/" + ns + "/secrets",
		CapturedAt:   time.Now(),
		ResponseCode: 200,
		ResponseBody: body,
	}
}

func TestRun_BasicSummary(t *testing.T) {
	archivePath := buildArchive(t, []*capture.Record{
		rec("r1", "/api/v1/namespaces/default/pods"),
		rec("r2", "/api/v1/namespaces/kube-system/pods"),
		rec("r3", "/apis/apps/v1/namespaces/default/deployments"),
		rec("r4", "/api/v1/nodes"),
		secretRec("r5", "default", "my-secret"),
	})

	report, err := Run(archivePath, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.CaptureID != "inspect-test-id" {
		t.Errorf("unexpected CaptureID: %q", report.CaptureID)
	}
	if report.KubernetesVersion != "v1.29.0" {
		t.Errorf("unexpected KubernetesVersion: %q", report.KubernetesVersion)
	}

	// Expect 4 distinct resource types (pods, deployments, nodes, secrets).
	if len(report.Resources) != 4 {
		t.Errorf("expected 4 resource summaries, got %d: %v", len(report.Resources), report.Resources)
	}

	// Pods: namespaced, 2 namespaces.
	var podsFound bool
	for _, rs := range report.Resources {
		if rs.Resource == "pods" && rs.Group == "" {
			podsFound = true
			if !rs.Namespaced {
				t.Error("pods should be namespaced")
			}
			if len(rs.Namespaces) != 2 {
				t.Errorf("pods: expected 2 namespaces, got %v", rs.Namespaces)
			}
			if rs.Records != 2 {
				t.Errorf("pods: expected 2 records, got %d", rs.Records)
			}
		}
	}
	if !podsFound {
		t.Error("pods resource not found in report")
	}

	// Nodes: cluster-scoped.
	for _, rs := range report.Resources {
		if rs.Resource == "nodes" {
			if rs.Namespaced {
				t.Error("nodes should not be namespaced")
			}
			if len(rs.Namespaces) != 0 {
				t.Errorf("nodes: expected empty namespaces, got %v", rs.Namespaces)
			}
		}
	}
}

func TestRun_ArchiveSize(t *testing.T) {
	archivePath := buildArchive(t, []*capture.Record{
		rec("r1", "/api/v1/namespaces/default/pods"),
	})
	report, err := Run(archivePath, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	fi, _ := os.Stat(archivePath)
	if report.ArchiveSize != fi.Size() {
		t.Errorf("ArchiveSize mismatch: got %d, want %d", report.ArchiveSize, fi.Size())
	}
}

func TestRun_TableKeysSkipped(t *testing.T) {
	// An index key with "?as=Table" should not produce a spurious resource entry.
	archivePath := buildArchive(t, []*capture.Record{
		rec("r1", "/api/v1/namespaces/default/pods"),
	})
	ar, err := archive.Open(archivePath)
	if err != nil {
		t.Fatalf("archive.Open: %v", err)
	}
	defer ar.Close()

	// Manually inject a Table index entry; verify that summarizeResources skips
	// paths containing "?" via the unit function directly.
	idx := capture.Index{
		"/api/v1/namespaces/default/pods": {
			APIPath: "/api/v1/namespaces/default/pods",
			Seqs:    []int{0},
			Times:   []time.Time{time.Now()},
		},
		"/api/v1/namespaces/default/pods?as=Table": {
			APIPath: "/api/v1/namespaces/default/pods?as=Table",
			Seqs:    []int{0},
			Times:   []time.Time{time.Now()},
		},
	}
	summaries := summarizeResources(ar, idx)
	if len(summaries) != 1 {
		t.Errorf("expected 1 summary (Table key excluded), got %d", len(summaries))
	}
}

func TestRun_SortedOutput(t *testing.T) {
	archivePath := buildArchive(t, []*capture.Record{
		rec("r1", "/apis/apps/v1/namespaces/default/deployments"),
		rec("r2", "/api/v1/namespaces/default/pods"),
		rec("r3", "/api/v1/nodes"),
	})
	report, err := Run(archivePath, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i := 1; i < len(report.Resources); i++ {
		a := report.Resources[i-1]
		b := report.Resources[i]
		sa := a.Group + "/" + a.Version + "/" + a.Resource
		sb := b.Group + "/" + b.Version + "/" + b.Resource
		if sa > sb {
			t.Errorf("resources not sorted: %q > %q", sa, sb)
		}
	}
}

// TestRun_NamespacedFromItems_NotPathShape covers a namespaced resource captured
// via the *cluster-wide* path (/api/v1/pods rather than
// /api/v1/namespaces/x/pods).
//
// That form is the one to use on a large cluster — it costs a single LIST
// instead of one per namespace, which on an 80-namespace cluster is a 44x
// difference in request volume. Namespacedness was derived from whether the
// request path contained a /namespaces/ segment, so precisely the recommended
// configuration reported `"namespaced": false` for every namespaced resource,
// and omitted `namespaces` entirely. Found against a real 80-namespace cluster,
// where 9 of 11 captured resources were mislabeled.
func TestRun_NamespacedFromItems_NotPathShape(t *testing.T) {
	now := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	path := buildArchive(t, []*capture.Record{
		{
			// Cluster-wide pods LIST: no namespace in the path, but every item
			// carries one.
			ID: "pods-0", CapturedAt: now, APIPath: "/api/v1/pods",
			HTTPMethod: "GET", ResponseCode: 200,
			ResponseBody: json.RawMessage(`{"kind":"PodList","apiVersion":"v1","items":[
				{"metadata":{"name":"a","namespace":"team-one"}},
				{"metadata":{"name":"b","namespace":"team-two"}},
				{"metadata":{"name":"c","namespace":"team-one"}}
			]}`),
		},
		{
			// A genuinely cluster-scoped resource must stay namespaced=false.
			ID: "nodes-0", CapturedAt: now, APIPath: "/api/v1/nodes",
			HTTPMethod: "GET", ResponseCode: 200,
			ResponseBody: json.RawMessage(`{"kind":"NodeList","apiVersion":"v1","items":[
				{"metadata":{"name":"node-1"}}
			]}`),
		},
	})

	rep, err := Run(path, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	find := func(resource string) *ResourceSummary {
		for i := range rep.Resources {
			if rep.Resources[i].Resource == resource {
				return &rep.Resources[i]
			}
		}
		t.Fatalf("no summary for %q; got %+v", resource, rep.Resources)
		return nil
	}

	pods := find("pods")
	if !pods.Namespaced {
		t.Error("pods: namespaced=false for a cluster-wide LIST whose items all carry a namespace")
	}
	if len(pods.Namespaces) != 2 || pods.Namespaces[0] != "team-one" || pods.Namespaces[1] != "team-two" {
		t.Errorf("pods: namespaces = %v, want [team-one team-two] deduped and sorted", pods.Namespaces)
	}
	if pods.Items != 3 {
		t.Errorf("pods: item_count = %d, want 3", pods.Items)
	}

	nodes := find("nodes")
	if nodes.Namespaced {
		t.Error("nodes: namespaced=true for a cluster-scoped resource whose items carry no namespace")
	}
	if len(nodes.Namespaces) != 0 {
		t.Errorf("nodes: namespaces = %v, want empty", nodes.Namespaces)
	}
}
