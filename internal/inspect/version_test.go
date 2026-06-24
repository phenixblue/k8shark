package inspect

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

func TestRun_ReportsFormatVersion_LegacyNormalizedToOne(t *testing.T) {
	// buildArchive writes no format_version (pre-versioning archive); inspect
	// should report it as version 1.
	path := buildArchive(t, []*capture.Record{rec("r1", "/api/v1/namespaces/default/pods")})
	report, err := Run(path)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.FormatVersion != 1 {
		t.Errorf("FormatVersion = %d, want 1", report.FormatVersion)
	}
}

func TestRun_RejectsFutureFormatVersion(t *testing.T) {
	path := buildArchiveWithVersion(t, capture.CurrentFormatVersion+1)
	if _, err := Run(path); err == nil {
		t.Fatal("expected Run to reject an archive with a newer format version")
	}
}

// buildArchiveWithVersion writes a minimal archive stamped with the given
// format version.
func buildArchiveWithVersion(t *testing.T, version int) string {
	t.Helper()
	now := time.Date(2026, 4, 9, 8, 0, 0, 0, time.UTC)
	r := rec("r1", "/api/v1/namespaces/default/pods")
	idx := capture.Index{
		r.APIPath: {APIPath: r.APIPath, Seqs: []int{0}, Times: []time.Time{now}},
	}
	meta := &capture.CaptureMetadata{
		FormatVersion: version,
		CaptureID:     "future-archive",
		CapturedAt:    now,
		CapturedUntil: now,
		RecordCount:   1,
	}
	out := filepath.Join(t.TempDir(), "future.kshrk")
	sw, err := archive.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	if err := sw.WriteRecord(r); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return out
}
