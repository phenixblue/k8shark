package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	archivepkg "github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/spf13/cobra"
)

func TestRunDiff_ExitCodeOnDiff(t *testing.T) {
	before := buildDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"before"}}]}`)
	after := buildDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"after"}}]}`)

	cmd := newTestDiffCommand()
	if err := cmd.Flags().Set("before", before); err != nil {
		t.Fatalf("set before flag: %v", err)
	}
	if err := cmd.Flags().Set("after", after); err != nil {
		t.Fatalf("set after flag: %v", err)
	}

	err := runDiff(cmd, nil)
	if err == nil {
		t.Fatal("expected diff exit error, got nil")
	}
	exitErr, ok := err.(exitError)
	if !ok {
		t.Fatalf("expected exitError, got %T", err)
	}
	if exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit code 1, got %d", exitErr.ExitCode())
	}
}

func TestRunDiff_JSONOutputNoDiff(t *testing.T) {
	archivePath := buildDiffArchive(t, `{"kind":"PodList","items":[]}`)
	cmd := newTestDiffCommand()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	if err := cmd.Flags().Set("before", archivePath); err != nil {
		t.Fatalf("set before flag: %v", err)
	}
	if err := cmd.Flags().Set("after", archivePath); err != nil {
		t.Fatalf("set after flag: %v", err)
	}
	if err := cmd.Flags().Set("output", "json"); err != nil {
		t.Fatalf("set output flag: %v", err)
	}

	err := runDiff(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var result struct {
		Changes []any `json:"changes"`
	}
	if jerr := json.Unmarshal(buf.Bytes(), &result); jerr != nil {
		t.Fatalf("invalid json output: %v", jerr)
	}
	if len(result.Changes) != 0 {
		t.Fatalf("expected no changes, got %d", len(result.Changes))
	}
}

func newTestDiffCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().String("before", "", "")
	cmd.Flags().String("after", "", "")
	cmd.Flags().String("archive", "", "")
	cmd.Flags().String("from", "", "")
	cmd.Flags().String("to", "", "")
	cmd.Flags().String("resource", "", "")
	cmd.Flags().String("namespace", "", "")
	cmd.Flags().StringP("output", "o", "text", "")
	addDecryptFlags(cmd)
	// PersistentFlags on a standalone command aren't merged into Flags() until
	// execution via cmd.Execute(); since tests call runDiff directly, merge
	// them explicitly so resolveDecryptIdentities can read them.
	cmd.Flags().AddFlagSet(cmd.PersistentFlags())
	return cmd
}

// buildEncryptedDiffArchive mirrors buildDiffArchive but writes an
// age-encrypted archive using a low-work-factor passphrase recipient.
func buildEncryptedDiffArchive(t *testing.T, body, passphrase string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "capture.kshrk")
	now := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	rec := &capture.Record{ID: "rec-1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body)}
	meta := &capture.CaptureMetadata{CaptureID: "test-capture", CapturedAt: now, CapturedUntil: now, RecordCount: 1, Encrypted: true}
	index := capture.Index{
		"/api/v1/namespaces/default/pods": {APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}, Times: []time.Time{now}},
	}
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		t.Fatalf("NewScryptRecipient: %v", err)
	}
	r.SetWorkFactor(10)
	sw, err := archivepkg.NewEncryptedStreamWriter(out, []age.Recipient{r})
	if err != nil {
		t.Fatalf("NewEncryptedStreamWriter: %v", err)
	}
	// Release the file handle even if WriteRecord/Finish below t.Fatalf's, so
	// TempDir cleanup can't be blocked by an open file. Abort is a no-op after
	// a successful Finish.
	defer func() { _ = sw.Abort() }()
	if _, err := sw.WriteRecord(rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if err := sw.Finish(meta, index, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return out
}

// TestRunDiff_EncryptedBeforeOnly exercises the M3 fix where only one side of
// a two-archive diff is encrypted: --before is encrypted, --after is
// plaintext. The shared --decrypt-passphrase-file must still be tried against
// the encrypted side and the diff must run end to end.
func TestRunDiff_EncryptedBeforeOnly(t *testing.T) {
	const passphrase = "diff-encrypt-test-passphrase"
	before := buildEncryptedDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"before"}}]}`, passphrase)
	after := buildDiffArchive(t, `{"kind":"PodList","items":[{"metadata":{"name":"after"}}]}`)

	passFile := filepath.Join(t.TempDir(), "pass.txt")
	if err := os.WriteFile(passFile, []byte(passphrase), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cmd := newTestDiffCommand()
	if err := cmd.Flags().Set("before", before); err != nil {
		t.Fatalf("set before flag: %v", err)
	}
	if err := cmd.Flags().Set("after", after); err != nil {
		t.Fatalf("set after flag: %v", err)
	}
	if err := cmd.Flags().Set("output", "json"); err != nil {
		t.Fatalf("set output flag: %v", err)
	}
	if err := cmd.Flags().Set("decrypt-passphrase-file", passFile); err != nil {
		t.Fatalf("set decrypt-passphrase-file flag: %v", err)
	}
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)

	err := runDiff(cmd, nil)
	if err == nil {
		t.Fatal("expected a diff (exit error) between before/after pod names")
	}
	if _, ok := err.(exitError); !ok {
		t.Fatalf("expected exitError (a real diff was found), got %T: %v", err, err)
	}
}

func buildDiffArchive(t *testing.T, body string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "capture.kshrk")
	now := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	rec := &capture.Record{ID: "rec-1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(body)}
	meta := &capture.CaptureMetadata{CaptureID: "test-capture", CapturedAt: now, CapturedUntil: now, RecordCount: 1}
	index := capture.Index{
		"/api/v1/namespaces/default/pods": {APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}, Times: []time.Time{now}},
	}
	sw, err := archivepkg.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	if _, err := sw.WriteRecord(rec); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if err := sw.Finish(meta, index, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return out
}
