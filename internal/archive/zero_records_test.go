package archive_test

import (
	"path/filepath"
	"testing"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/inspect"
	"github.com/phenixblue/k8shark/internal/store"
)

// buildZeroRecordArchive writes a structurally valid, completely empty
// capture: Finish runs with a real (non-nil) but zero-entry index, and no
// records were ever written. A capture with all-empty resources (e.g. a
// namespace with nothing in it, or a duration too short for any poll to
// fire) produces exactly this.
func buildZeroRecordArchive(t *testing.T, path string) {
	t.Helper()
	sw, err := archive.NewStreamWriter(path)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	// Abort is a no-op once Finish has run, so this is a safe no-op on the
	// happy path and a real cleanup if a t.Fatalf below skips Finish.
	defer func() { _ = sw.Abort() }()
	meta := capture.CaptureMetadata{
		FormatVersion: capture.CurrentFormatVersion,
		CaptureID:     "zero-records",
		RecordCount:   0,
	}
	if err := sw.Finish(&meta, capture.Index{}, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

// TestArchive_ZeroRecords verifies that a capture with zero records — not
// corrupt, just empty — is read cleanly by every consumer: inspect.Run and
// store.LoadStore must succeed and report zero resources rather than
// panicking on a nil/empty index or record set (#248).
func TestArchive_ZeroRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.kshrk")
	buildZeroRecordArchive(t, path)

	t.Run("inspect.Run", func(t *testing.T) {
		report, err := inspect.Run(path, nil)
		if err != nil {
			t.Fatalf("inspect.Run: %v", err)
		}
		if report.RecordCount != 0 {
			t.Errorf("RecordCount = %d, want 0", report.RecordCount)
		}
		if len(report.Resources) != 0 {
			t.Errorf("Resources = %v, want empty", report.Resources)
		}
	})

	t.Run("store.LoadStore", func(t *testing.T) {
		ar, err := archive.Open(path)
		if err != nil {
			t.Fatalf("archive.Open: %v", err)
		}
		defer ar.Close()
		store, err := store.LoadStore(ar)
		if err != nil {
			t.Fatalf("store.LoadStore: %v", err)
		}
		// Deferred immediately (not just called at the end) so a t.Fatalf added
		// later in this subtest can't skip it and leave the enrichment
		// goroutine running when the deferred ar.Close() above runs.
		defer store.Close()
		if len(store.Index) != 0 {
			t.Errorf("Index = %v, want empty", store.Index)
		}
		if got := len(store.Resources()); got != 0 {
			t.Errorf("Resources() = %d entries, want 0", got)
		}
	})
}
