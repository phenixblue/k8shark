package server_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/diff"
	"github.com/phenixblue/k8shark/internal/inspect"
	"github.com/phenixblue/k8shark/internal/redact"
	"github.com/phenixblue/k8shark/internal/server"
	"github.com/phenixblue/k8shark/internal/transitions"
)

// buildFutureFormatArchive writes a structurally valid archive whose
// FormatVersion is one newer than this build understands, so every entry
// point below should reject it rather than parse it under v1 assumptions.
func buildFutureFormatArchive(t *testing.T, path string) {
	t.Helper()
	sw, err := archive.NewStreamWriter(path)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	// Abort is a no-op once Finish has run, so this is a safe no-op on the
	// happy path and a real cleanup if a t.Fatalf below skips Finish.
	defer func() { _ = sw.Abort() }()
	rec := capture.Record{
		ID: "rec-1", CapturedAt: time.Now().UTC(), APIPath: "/api/v1/namespaces/default/pods",
		HTTPMethod: "GET", ResponseCode: 200, ResponseBody: []byte(`{"kind":"PodList","items":[]}`),
	}
	seq, err := sw.WriteRecord(&rec)
	if err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	meta := capture.CaptureMetadata{
		FormatVersion: capture.CurrentFormatVersion + 1,
		CaptureID:     "future-format",
		CapturedAt:    rec.CapturedAt.Add(-time.Minute),
		CapturedUntil: rec.CapturedAt,
	}
	index := capture.Index{
		"/api/v1/namespaces/default/pods": {
			APIPath: "/api/v1/namespaces/default/pods",
			Seqs:    []int{seq},
			Times:   []time.Time{rec.CapturedAt},
		},
	}
	if err := sw.Finish(&meta, index, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

// TestAllReaders_RejectFutureFormatVersion is a regression test for #250: a
// future-format archive that every other subcommand correctly rejects with
// an "upgrade kshrk" error was, at the time of filing, parsed by
// transitions.LoadTransitions under v1 assumptions instead — producing
// silently wrong output rather than a clear error. This asserts every public
// entry point that opens an archive rejects it the same way.
func TestAllReaders_RejectFutureFormatVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future-format.kshrk")
	buildFutureFormatArchive(t, path)

	assertUpgradeError := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error for a future-format archive, got nil")
		}
		if !strings.Contains(err.Error(), "upgrade") {
			t.Errorf("error = %v, want it to mention \"upgrade\"", err)
		}
	}

	t.Run("inspect.Run", func(t *testing.T) {
		_, err := inspect.Run(path, nil)
		assertUpgradeError(t, err)
	})

	t.Run("server.LoadStore", func(t *testing.T) {
		ar, err := archive.Open(path)
		if err != nil {
			t.Fatalf("archive.Open: %v", err)
		}
		defer ar.Close()
		_, err = server.LoadStore(ar)
		assertUpgradeError(t, err)
	})

	t.Run("redact.Archive", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "out.kshrk")
		_, err := redact.Archive(path, dst, redact.Options{})
		assertUpgradeError(t, err)
	})

	t.Run("transitions.LoadTransitions", func(t *testing.T) {
		_, err := transitions.LoadTransitions(path, transitions.FilterOpts{}, nil)
		assertUpgradeError(t, err)
	})

	t.Run("diff.Run", func(t *testing.T) {
		_, err := diff.Run(diff.Options{Archive: path, BeforeAt: "0s", AfterAt: "0s"})
		assertUpgradeError(t, err)
	})
}
