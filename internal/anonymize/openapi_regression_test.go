package anonymize

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/server"
)

// openAPIRegressionClient mirrors internal/ui/v2/overlay_test.go's
// overlayTestClient: the mock server's TLS cert is self-signed and this test
// only needs to reach it locally, not validate its chain.
var openAPIRegressionClient = &http.Client{
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // #nosec G402 -- test only
	Timeout:   10 * time.Second,
}

// TestArchive_URLCategoryDoesNotCorruptOpenAPIDocumentation is an end-to-end
// regression test for a real bug found anonymizing examples/basic-workloads:
// before isOpenAPIDocumentPath/tableColumnDefinitionsPrefix existed
// (urlmatch.go), the url category's full-tree scan discovered dozens of
// unrelated hostnames (kubernetes.io, k8s.io, git.k8s.io, ...) from the API
// server's own built-in OpenAPI schema description text and Table
// columnDefinitions — not real cluster infrastructure. Combined with
// --full-sweep, that noise corrupted structurally load-bearing strings like
// "apiVersion":"meta.k8s.io/v1" (an unrelated substring match on the
// discovered "k8s.io" candidate), which broke `kubectl get` against the
// replayed archive.
func TestArchive_URLCategoryDoesNotCorruptOpenAPIDocumentation(t *testing.T) {
	const fixture = "../../examples/basic-workloads/capture.kshrk"

	dst := filepath.Join(t.TempDir(), "out.kshrk")
	salt := []byte("openapi-doc-regression-test-salt")

	result, err := Archive(fixture, dst, Options{
		Categories: []Category{CategoryURL},
		Salt:       salt,
		FullSweep:  true,
	})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	// The only genuine url-category occurrence in this fixture is the mock
	// server's own recorded address; every doc-URL host that used to be
	// discovered from OpenAPI/Table schema text must be gone.
	urlMapping := result.Mapping[CategoryURL]
	if _, ok := urlMapping["127.0.0.1"]; !ok {
		t.Errorf("url mapping missing the legitimate ServerAddress occurrence \"127.0.0.1\": %v", urlMapping)
	}
	if len(urlMapping) != 1 {
		t.Errorf("url mapping has %d entries, want exactly 1 (ServerAddress only) — got %v", len(urlMapping), urlMapping)
	}

	// Compare every record's top-level apiVersion against the original
	// archive: none may have changed. This is the exact field the reported
	// corruption mangled ("meta.k8s.io/v1" -> "meta.<alias>/v1").
	origVersions := recordAPIVersions(t, fixture)
	gotVersions := recordAPIVersions(t, dst)
	if len(origVersions) == 0 {
		t.Fatal("test fixture sanity check failed: no apiVersion fields found at all")
	}
	for key, want := range origVersions {
		got, ok := gotVersions[key]
		if !ok {
			t.Errorf("%s: record missing from anonymized output", key)
			continue
		}
		if got != want {
			t.Errorf("%s: apiVersion = %q, want unchanged %q", key, got, want)
		}
	}

	// Live end-to-end replay check: open the anonymized archive through the
	// real mock server and request /api/v1/nodes exactly the way `kubectl
	// get nodes` does (an Accept header requesting the Table format) — the
	// concrete failure mode the bug report described as "blank NAME column,
	// AGE: <unknown>".
	srv, err := server.Open(server.OpenOptions{
		ArchivePath:   dst,
		KubeconfigOut: filepath.Join(t.TempDir(), "kubeconfig.yaml"),
	})
	if err != nil {
		t.Fatalf("server.Open on anonymized archive: %v", err)
	}
	defer srv.Shutdown()

	req, err := http.NewRequest(http.MethodGet, srv.Address()+"/api/v1/nodes", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io, application/json")
	resp, err := openAPIRegressionClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/nodes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/nodes = %d, want 200", resp.StatusCode)
	}
	var table struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
		Rows       []struct {
			Cells []interface{} `json:"cells"`
		} `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&table); err != nil {
		t.Fatalf("decoding Table response: %v", err)
	}
	if table.APIVersion != "meta.k8s.io/v1" {
		t.Errorf("Table apiVersion = %q, want %q — this is the exact corruption the bug reported", table.APIVersion, "meta.k8s.io/v1")
	}
	if len(table.Rows) == 0 || len(table.Rows[0].Cells) == 0 {
		t.Fatal("Table has no rows/cells — replay produced an empty result")
	}
	if name, _ := table.Rows[0].Cells[0].(string); name == "" {
		t.Error("node Table's NAME cell is blank — reproduces the reported `kubectl get nodes` corruption")
	}
}

// recordAPIVersions reads every record in the archive at path and returns
// its top-level "apiVersion" string keyed by "<apiPath>#<seq>", for records
// that have one.
func recordAPIVersions(t *testing.T, path string) map[string]string {
	t.Helper()
	ar, err := archive.Open(path)
	if err != nil {
		t.Fatalf("archive.Open(%s): %v", path, err)
	}
	defer ar.Close()

	idx, err := ar.ReadIndex()
	if err != nil {
		t.Fatalf("ReadIndex(%s): %v", path, err)
	}

	versions := make(map[string]string)
	for apiPath, entry := range idx {
		for _, seq := range entry.Seqs {
			data, err := ar.ReadRecord(apiPath, seq)
			if err != nil {
				t.Fatalf("ReadRecord(%s, %d): %v", apiPath, seq, err)
			}
			var rec capture.Record
			if err := json.Unmarshal(data, &rec); err != nil {
				t.Fatalf("unmarshal record %s seq %d: %v", apiPath, seq, err)
			}
			var body struct {
				APIVersion string `json:"apiVersion"`
			}
			if err := json.Unmarshal(rec.ResponseBody, &body); err != nil {
				continue // non-JSON or non-object body; not relevant here
			}
			if body.APIVersion == "" {
				continue
			}
			versions[fmt.Sprintf("%s#%d", apiPath, seq)] = body.APIVersion
		}
	}
	return versions
}
