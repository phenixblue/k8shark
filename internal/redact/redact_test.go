package redact

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

// buildTestArchive writes a minimal k8shark archive containing the given records
// and returns its file path (inside a temp dir cleaned up by t.Cleanup).
func buildTestArchive(t *testing.T, records []*capture.Record) string {
	t.Helper()
	dir := t.TempDir()

	idx := capture.Index{}
	for _, r := range records {
		e := &capture.IndexEntry{
			APIPath:   r.APIPath,
			RecordIDs: []string{r.ID},
			Times:     []time.Time{r.CapturedAt},
		}
		idx[r.APIPath] = e
	}

	meta := &capture.CaptureMetadata{
		CaptureID:         "test-id",
		CapturedAt:        time.Now().Add(-time.Minute),
		CapturedUntil:     time.Now(),
		KubernetesVersion: "v1.29.0",
		ServerAddress:     "https://127.0.0.1:6443",
		RecordCount:       len(records),
	}

	outPath := filepath.Join(dir, "test.tar.gz")
	if err := archive.Write(outPath, meta, records, idx); err != nil {
		t.Fatalf("buildTestArchive: %v", err)
	}
	return outPath
}

func secretRecord(id, ns, name string, data map[string]string, stringData map[string]string) *capture.Record {
	obj := map[string]any{
		"kind":       "Secret",
		"apiVersion": "v1",
		"metadata": map[string]any{
			"name":      name,
			"namespace": ns,
		},
	}
	if data != nil {
		obj["data"] = data
	}
	if stringData != nil {
		obj["stringData"] = stringData
	}
	body, _ := json.Marshal(obj)
	apiPath := "/api/v1/namespaces/" + ns + "/secrets"
	return &capture.Record{
		ID:           id,
		APIPath:      apiPath,
		CapturedAt:   time.Now(),
		ResponseCode: 200,
		ResponseBody: body,
	}
}

func podRecord(id, ns string) *capture.Record {
	obj := map[string]any{
		"kind":       "PodList",
		"apiVersion": "v1",
		"items":      []any{},
	}
	body, _ := json.Marshal(obj)
	apiPath := "/api/v1/namespaces/" + ns + "/pods"
	return &capture.Record{
		ID:           id,
		APIPath:      apiPath,
		CapturedAt:   time.Now(),
		ResponseCode: 200,
		ResponseBody: body,
	}
}

func TestRedact_SecretDataRedacted(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("my-password"))
	src := buildTestArchive(t, []*capture.Record{
		secretRecord("r1", "default", "db-creds", map[string]string{"password": encoded}, nil),
	})
	dst := src + "-redacted.tar.gz"
	t.Cleanup(func() { os.Remove(dst) })

	n, err := Archive(src, dst, nil)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 redacted record, got %d", n)
	}

	// Verify the output archive has the value replaced.
	tmpDir := t.TempDir()
	if err := archive.Open(dst, tmpDir); err != nil {
		t.Fatalf("opening redacted archive: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tmpDir, "k8shark-capture", "records", "r1.json"))
	if err != nil {
		t.Fatalf("reading record: %v", err)
	}
	var rec capture.Record
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parsing record: %v", err)
	}
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(rec.ResponseBody, &obj)
	var dataMap map[string]string
	_ = json.Unmarshal(obj["data"], &dataMap)

	want := base64.StdEncoding.EncodeToString([]byte("REDACTED"))
	if dataMap["password"] != want {
		t.Errorf("expected data[password]=%q, got %q", want, dataMap["password"])
	}
}

func TestRedact_StringDataRedacted(t *testing.T) {
	src := buildTestArchive(t, []*capture.Record{
		secretRecord("r2", "default", "plain-secret", nil, map[string]string{"token": "s3cr3t"}),
	})
	dst := src + "-redacted.tar.gz"
	t.Cleanup(func() { os.Remove(dst) })

	_, err := Archive(src, dst, nil)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}

	tmpDir := t.TempDir()
	if err := archive.Open(dst, tmpDir); err != nil {
		t.Fatalf("opening redacted archive: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tmpDir, "k8shark-capture", "records", "r2.json"))
	if err != nil {
		t.Fatalf("reading record: %v", err)
	}
	var rec capture.Record
	_ = json.Unmarshal(data, &rec)
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(rec.ResponseBody, &obj)
	var sdMap map[string]string
	_ = json.Unmarshal(obj["stringData"], &sdMap)

	if sdMap["token"] != "REDACTED" {
		t.Errorf("expected stringData[token]=REDACTED, got %q", sdMap["token"])
	}
}

func TestRedact_AllowList(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("keep-me"))
	src := buildTestArchive(t, []*capture.Record{
		secretRecord("r3", "default", "allowed-secret", map[string]string{"key": encoded}, nil),
	})
	dst := src + "-redacted.tar.gz"
	t.Cleanup(func() { os.Remove(dst) })

	n, err := Archive(src, dst, map[string]bool{"default/allowed-secret": true})
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 redacted records (all allowlisted), got %d", n)
	}

	tmpDir := t.TempDir()
	if err := archive.Open(dst, tmpDir); err != nil {
		t.Fatalf("opening redacted archive: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tmpDir, "k8shark-capture", "records", "r3.json"))
	if err != nil {
		t.Fatalf("reading record: %v", err)
	}
	var rec capture.Record
	_ = json.Unmarshal(data, &rec)
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(rec.ResponseBody, &obj)
	var dataMap map[string]string
	_ = json.Unmarshal(obj["data"], &dataMap)

	if dataMap["key"] != encoded {
		t.Errorf("expected original value preserved for allowlisted secret, got %q", dataMap["key"])
	}
}

func TestRedact_NonSecretUnchanged(t *testing.T) {
	src := buildTestArchive(t, []*capture.Record{
		podRecord("r4", "default"),
	})
	dst := src + "-redacted.tar.gz"
	t.Cleanup(func() { os.Remove(dst) })

	n, err := Archive(src, dst, nil)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 redacted (no secrets), got %d", n)
	}
}
