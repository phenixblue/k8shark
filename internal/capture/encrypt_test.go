package capture

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/config"
)

// encryptTestPassphrase is a fixed test-only passphrase; not a real secret.
const encryptTestPassphrase = "capture-encrypt-test-passphrase" //nolint:gosec // test fixture

// fastScryptRecipients builds a passphrase recipient with a deliberately low
// scrypt work factor. The production default (logN=18) is CPU-heavy and, when
// several package test binaries run in parallel under CI, can starve the
// capture goroutines enough that the short poll window elapses with no
// records — making an otherwise-correct round-trip test flaky. Decryption
// reads the work factor from the file header, so the standard passphrase
// identity still decrypts these archives.
func fastScryptRecipients(t *testing.T, passphrase string) []age.Recipient {
	t.Helper()
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		t.Fatalf("NewScryptRecipient: %v", err)
	}
	r.SetWorkFactor(10)
	return []age.Recipient{r}
}

// TestEngine_CaptureEncrypted verifies that a capture run with encryption
// recipients writes an age-encrypted archive: a plain Open must fail because
// the archive is encrypted, OpenWithIdentities with the matching passphrase
// must succeed, and the decrypted metadata must record Encrypted=true.
func TestEngine_CaptureEncrypted(t *testing.T) {
	fake := fakeK8sServer(t)
	defer fake.Close()

	outFile := filepath.Join(t.TempDir(), "capture.kshrk")
	cfg := &config.Config{
		DurationRaw: "2s",
		Duration:    2 * time.Second,
		Output:      outFile,
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "500ms", Interval: 500 * time.Millisecond},
		},
	}

	eng := newEngineWith(cfg, fake.Client(), fake.URL, false)
	eng.SetEncryption(fastScryptRecipients(t, encryptTestPassphrase))
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	if fi, err := os.Stat(outFile); err != nil || fi.Size() == 0 {
		t.Fatalf("output archive missing or empty: %v", err)
	}

	// A plain Open must refuse the encrypted archive with a clear message.
	if _, err := archive.Open(outFile); err == nil {
		t.Fatal("archive.Open on encrypted capture succeeded, want error")
	}

	identities, err := archive.IdentitiesFromPassphrase(encryptTestPassphrase)
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	ar, err := archive.OpenWithIdentities(outFile, identities)
	if err != nil {
		t.Fatalf("OpenWithIdentities: %v", err)
	}
	defer ar.Close()

	var meta CaptureMetadata
	if err := ar.ReadMetadata(&meta); err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if !meta.Encrypted {
		t.Error("decrypted metadata.Encrypted = false, want true")
	}
	var idx Index
	if err := ar.ReadIndex(&idx); err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if _, ok := idx["/api/v1/namespaces/default/pods"]; !ok {
		t.Error("pod path missing from decrypted index")
	}
}

// TestEngine_CaptureEncrypted_WrongPassphrase confirms that opening an
// encrypted capture with the wrong passphrase fails with a clean message.
func TestEngine_CaptureEncrypted_WrongPassphrase(t *testing.T) {
	fake := fakeK8sServer(t)
	defer fake.Close()

	outFile := filepath.Join(t.TempDir(), "capture.kshrk")
	cfg := &config.Config{
		DurationRaw: "1s",
		Duration:    1 * time.Second,
		Output:      outFile,
		Resources: []config.Resource{
			{Version: "v1", Resource: "pods", Namespaces: []string{"default"}, IntervalRaw: "500ms", Interval: 500 * time.Millisecond},
		},
	}
	eng := newEngineWith(cfg, fake.Client(), fake.URL, false)
	eng.SetEncryption(fastScryptRecipients(t, encryptTestPassphrase))
	if _, err := eng.Run(); err != nil {
		t.Fatalf("engine.Run() error: %v", err)
	}

	wrong, err := archive.IdentitiesFromPassphrase("not-the-passphrase")
	if err != nil {
		t.Fatalf("IdentitiesFromPassphrase: %v", err)
	}
	if _, err := archive.OpenWithIdentities(outFile, wrong); err == nil {
		t.Fatal("OpenWithIdentities with wrong passphrase succeeded, want error")
	}
}
