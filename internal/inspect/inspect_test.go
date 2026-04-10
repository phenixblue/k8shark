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
			e.RecordIDs = append(e.RecordIDs, r.ID)
			e.Times = append(e.Times, r.CapturedAt)
		} else {
			idx[r.APIPath] = &capture.IndexEntry{
				APIPath:   r.APIPath,
				RecordIDs: []string{r.ID},
				Times:     []time.Time{r.CapturedAt},
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

	outPath := filepath.Join(dir, "test.tar.gz")
	if err := archive.Write(outPath, meta, records, idx); err != nil {
		t.Fatalf("buildArchive: %v", err)
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

	report, err := Run(archivePath)
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
	report, err := Run(archivePath)
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

	// Manually inject a Table index entry into the archive would require
	// re-building the archive; instead verify that summariseResources skips "?"
	// paths via the unit function directly.
	idx := capture.Index{
		"/api/v1/namespaces/default/pods": {
			APIPath:   "/api/v1/namespaces/default/pods",
			RecordIDs: []string{"r1"},
			Times:     []time.Time{time.Now()},
		},
		"/api/v1/namespaces/default/pods?as=Table": {
			APIPath:   "/api/v1/namespaces/default/pods?as=Table",
			RecordIDs: []string{"r2"},
			Times:     []time.Time{time.Now()},
		},
	}
	summaries := summariseResources(idx)
	if len(summaries) != 1 {
		t.Errorf("expected 1 summary (Table key excluded), got %d", len(summaries))
	}
	_ = archivePath
}

func TestRun_SortedOutput(t *testing.T) {
	archivePath := buildArchive(t, []*capture.Record{
		rec("r1", "/apis/apps/v1/namespaces/default/deployments"),
		rec("r2", "/api/v1/namespaces/default/pods"),
		rec("r3", "/api/v1/nodes"),
	})
	report, err := Run(archivePath)
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
