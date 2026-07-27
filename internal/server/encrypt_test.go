package server

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

// serverEncryptTestPassphrase is a fixed test-only passphrase; not a real secret.
const serverEncryptTestPassphrase = "server-encrypt-test-passphrase" //nolint:gosec // test fixture

// buildEncryptedTestArchive mirrors buildTestArchive but writes an
// age-encrypted archive using a low-work-factor passphrase recipient (fast
// scrypt for tests; decryption reads the work factor from the file header, so
// the standard passphrase identity still decrypts it).
func buildEncryptedTestArchive(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	outPath := filepath.Join(dir, "test.kshrk")

	podList := `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[{"metadata":{"name":"nginx","namespace":"default"}}]}`
	now := time.Now().UTC()
	rec := &capture.Record{
		ID: "rec-001", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods",
		HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podList),
	}
	meta := &capture.CaptureMetadata{
		CaptureID: "e2e-encrypted-capture", KubernetesVersion: "v1.29.0",
		CapturedAt: now.Add(-time.Minute), CapturedUntil: now, RecordCount: 1, Encrypted: true,
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/pods": {
			APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}, Times: []time.Time{now},
		},
	}

	r, err := age.NewScryptRecipient(serverEncryptTestPassphrase)
	if err != nil {
		t.Fatalf("NewScryptRecipient: %v", err)
	}
	r.SetWorkFactor(10)
	sw, err := archive.NewEncryptedStreamWriter(outPath, []age.Recipient{r})
	if err != nil {
		t.Fatalf("NewEncryptedStreamWriter: %v", err)
	}
	// Release the file handle even if WriteRecord/Finish below t.Fatalf's, so
	// TempDir cleanup can't be blocked by an open file. Abort is a no-op after
	// a successful Finish.
	defer func() { _ = sw.Abort() }()
	if err := sw.WriteRecord(rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("archive Finish: %v", err)
	}
	return outPath
}

// pinnedClient builds an http.Client that verifies srv's self-signed
// certificate via its RootCAs pool, rather than skipping TLS verification.
func pinnedClient(t *testing.T, srv *Server) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(srv.CertPEM()) {
		t.Fatal("failed to parse server cert PEM into a pool")
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
		},
	}
}

// TestServer_Open_EncryptedArchive mirrors TestServer_Open_EndToEnd but against
// an encrypted archive: this is the long-lived-holder path (open/ui hold the
// archive for the whole server lifetime), so it's worth a full HTTP round trip
// rather than just testing the decrypt resolver in isolation.
func TestServer_Open_EncryptedArchive(t *testing.T) {
	archivePath := buildEncryptedTestArchive(t)
	identities, err := archive.IdentitiesFromPassphrase(serverEncryptTestPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}

	kubeconfigOut := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	srv, err := Open(OpenOptions{
		ArchivePath:   archivePath,
		KubeconfigOut: kubeconfigOut,
		Identities:    identities,
	})
	if err != nil {
		t.Fatalf("server.Open: %v", err)
	}
	defer srv.Shutdown()

	client := pinnedClient(t, srv)

	url := fmt.Sprintf("%s/api/v1/namespaces/default/pods", srv.Address())
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET pods: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var pods map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&pods); err != nil {
		t.Fatal(err)
	}
	items, ok := pods["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatal("expected at least one pod in items")
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] is not an object: %#v", items[0])
	}
	meta, ok := item["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata is not an object: %#v", item["metadata"])
	}
	if meta["name"] != "nginx" {
		t.Errorf("expected pod name=nginx, got %v", meta["name"])
	}
}

// TestServer_Open_EncryptedArchiveNoIdentities confirms Open fails clearly
// (not a raw zip-parse error) when the archive is encrypted and no key is
// supplied.
func TestServer_Open_EncryptedArchiveNoIdentities(t *testing.T) {
	archivePath := buildEncryptedTestArchive(t)

	_, err := Open(OpenOptions{ArchivePath: archivePath})
	if err == nil {
		t.Fatal("server.Open on encrypted archive with no identities succeeded, want error")
	}
	if !strings.Contains(err.Error(), "is encrypted") {
		t.Errorf("error = %q, want it to mention the archive is encrypted", err)
	}
}

// TestServer_Replay_EncryptedArchive confirms the replay path (used by
// `kshrk ui` in replay mode) also decrypts correctly.
func TestServer_Replay_EncryptedArchive(t *testing.T) {
	archivePath := buildEncryptedTestArchive(t)
	identities, err := archive.IdentitiesFromPassphrase(serverEncryptTestPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}

	srv, err := Replay(ReplayOptions{
		ArchivePath:   archivePath,
		KubeconfigOut: filepath.Join(t.TempDir(), "kubeconfig.yaml"),
		Identities:    identities,
	})
	if err != nil {
		t.Fatalf("server.Replay: %v", err)
	}
	defer srv.Shutdown()

	client := pinnedClient(t, srv)
	resp, err := client.Get(srv.Address() + "/api/v1/namespaces/default/pods")
	if err != nil {
		t.Fatalf("GET pods: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
