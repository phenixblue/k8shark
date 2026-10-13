package anonymize

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// End-to-end through Archive(), WITHOUT --full-sweep: what `kubectl get
// pods -A` / `get nodes` / `get ns` actually render (a captured ?as=Table
// response's printed cells) must carry the same alias as the plain-JSON
// view of the same objects. Before rewriteTableRows, schema-aware matching
// never reached a Table at all, so the single most common verification
// command showed the real name even though the run reported success.
func tableFixtureRecords() []*capture.Record {
	podTable := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
		"columnDefinitions":[{"name":"Name"},{"name":"Ready"},{"name":"Status"}],
		"rows":[{"cells":["web-1","1/1","Running"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",
			"metadata":{"name":"web-1","namespace":"prod"}}}]}`
	podTableAll := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
		"columnDefinitions":[{"name":"Namespace"},{"name":"Name"},{"name":"Ready"}],
		"rows":[{"cells":["prod","web-1","1/1"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",
			"metadata":{"name":"web-1","namespace":"prod"}}}]}`
	nodeTable := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
		"columnDefinitions":[{"name":"Name"},{"name":"Status"}],
		"rows":[{"cells":["worker-1","Ready"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",
			"metadata":{"name":"worker-1"}}}]}`
	nsTable := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
		"columnDefinitions":[{"name":"Name"},{"name":"Status"}],
		"rows":[{"cells":["prod","Active"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",
			"metadata":{"name":"prod"}}}]}`
	pod := `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"web-1","namespace":"prod"},"spec":{"nodeName":"worker-1"}}`
	node := `{"kind":"Node","apiVersion":"v1","metadata":{"name":"worker-1"}}`
	ns := `{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"prod"}}`
	rec := func(id, path, body string) *capture.Record {
		return &capture.Record{ID: id, CapturedAt: fixedNow, APIPath: path, HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body)}
	}
	return []*capture.Record{
		rec("r1", "/api/v1/namespaces/prod/pods/web-1", pod),
		rec("r2", "/api/v1/nodes/worker-1", node),
		rec("r3", "/api/v1/namespaces/prod", ns),
		rec("r4", "/api/v1/namespaces/prod/pods?as=Table", podTable),
		rec("r5", "/api/v1/pods?as=Table", podTableAll),
		rec("r6", "/api/v1/nodes?as=Table", nodeTable),
		rec("r7", "/api/v1/namespaces?as=Table", nsTable),
	}
}

func TestArchive_TableCellsAliasedWithoutFullSweep(t *testing.T) {
	for _, fullSweep := range []bool{false, true} {
		name := "without full-sweep"
		if fullSweep {
			name = "with full-sweep (schema-aware result must not be double-aliased)"
		}
		t.Run(name, func(t *testing.T) {
			src := buildAnonymizeTestArchive(t, tableFixtureRecords())
			dst := filepath.Join(t.TempDir(), "out.kshrk")
			salt := []byte("table-cells-test-salt")
			if _, err := Archive(src, dst, Options{
				Categories: []Category{CategoryNamespace, CategoryNode, CategoryPod},
				Salt:       salt,
				FullSweep:  fullSweep,
			}); err != nil {
				t.Fatalf("Archive: %v", err)
			}

			a := NewAliaser(salt)
			nsAlias := a.Alias(CategoryNamespace, "prod")
			nodeAlias := a.Alias(CategoryNode, "worker-1")
			podAlias := a.Alias(CategoryPod, "web-1")

			ar, err := archive.Open(dst)
			if err != nil {
				t.Fatal(err)
			}
			defer ar.Close()

			type table struct {
				Rows []struct {
					Cells  []string `json:"cells"`
					Object struct {
						Metadata struct {
							Name      string `json:"name"`
							Namespace string `json:"namespace"`
						} `json:"metadata"`
					} `json:"object"`
				} `json:"rows"`
			}
			read := func(path string) table {
				t.Helper()
				data, err := ar.ReadRecord(path, 0)
				if err != nil {
					t.Fatalf("ReadRecord(%s): %v", path, err)
				}
				var rec capture.Record
				if err := json.Unmarshal(data, &rec); err != nil {
					t.Fatal(err)
				}
				var out table
				if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
					t.Fatal(err)
				}
				return out
			}
			check := func(label, got, want string) {
				t.Helper()
				if got != want {
					t.Errorf("%s = %q, want %q", label, got, want)
				}
			}

			// Namespaced pods Table (Name first, no Namespace column).
			pods := read("/api/v1/namespaces/" + nsAlias + "/pods?as=Table")
			check("pods Table object.metadata.name", pods.Rows[0].Object.Metadata.Name, podAlias)
			check("pods Table object.metadata.namespace", pods.Rows[0].Object.Metadata.Namespace, nsAlias)
			check("pods Table Name cell", pods.Rows[0].Cells[0], podAlias)

			// `get pods -A` (Namespace column FIRST, Name second): both
			// printed columns mirror the right field by column name, not
			// position.
			all := read("/api/v1/pods?as=Table")
			check("pods -A Namespace cell", all.Rows[0].Cells[0], nsAlias)
			check("pods -A Name cell", all.Rows[0].Cells[1], podAlias)
			check("pods -A untouched Ready cell", all.Rows[0].Cells[2], "1/1")

			nodes := read("/api/v1/nodes?as=Table")
			check("nodes Table Name cell", nodes.Rows[0].Cells[0], nodeAlias)
			check("nodes Table object.metadata.name", nodes.Rows[0].Object.Metadata.Name, nodeAlias)
			check("nodes Table untouched Status cell", nodes.Rows[0].Cells[1], "Ready")

			nss := read("/api/v1/namespaces?as=Table")
			check("namespaces Table Name cell", nss.Rows[0].Cells[0], nsAlias)
			check("namespaces Table object.metadata.name", nss.Rows[0].Object.Metadata.Name, nsAlias)
			check("namespaces Table untouched Status cell", nss.Rows[0].Cells[1], "Active")
		})
	}
}

// Only the requested categories' cells are touched: --categories node alone
// must leave a Pod Table's namespace (and pod name) real everywhere.
func TestArchive_TableCellsRespectRequestedCategories(t *testing.T) {
	src := buildAnonymizeTestArchive(t, tableFixtureRecords())
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("table-cells-category-gating-salt")
	if _, err := Archive(src, dst, Options{Categories: []Category{CategoryNode}, Salt: salt}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	ar, err := archive.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer ar.Close()
	// /api/v1/pods?as=Table has no node name in it and no path segment is
	// aliased under the node category, so it's still stored at its original
	// path — and must be byte-for-byte what we put in.
	data, err := ar.ReadRecord("/api/v1/pods?as=Table", 0)
	if err != nil {
		t.Fatal(err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Rows []struct {
			Cells []string `json:"cells"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.Rows[0].Cells; got[0] != "prod" || got[1] != "web-1" {
		t.Errorf("cells = %v, want namespace/pod name untouched under --categories node", got)
	}
}
