package archive

import (
	"errors"
	"io"
	"testing"

	"github.com/phenixblue/k8shark/internal/archive/format"
)

// failingWriter returns an error from every Write, simulating a broken pipe
// (e.g. `kshrk capture --out -` piped into a process that exits early).
type failingWriter struct{ err error }

func (w *failingWriter) Write(p []byte) (int, error) { return 0, w.err }

// TestNDJSONWriter_WriteRecord_FailedEncodeDoesNotAdvanceState verifies that
// when the underlying io.Writer fails mid-Encode, WriteRecord doesn't credit
// the record as written: UncompressedBytes and RecordCount must not advance,
// and the per-path seq counter must not be consumed — a record that was never
// actually written must not occupy a seq number or be counted in size
// reporting, mirroring the rule that WriteRecord's returned seq is only
// meaningful on success.
func TestNDJSONWriter_WriteRecord_FailedEncodeDoesNotAdvanceState(t *testing.T) {
	wantErr := errors.New("broken pipe")
	w := NewNDJSONWriter(&failingWriter{err: wantErr})

	rec := &format.Record{
		ID: "rec-1", APIPath: "/api/v1/namespaces/default/pods",
		HTTPMethod: "GET", ResponseCode: 200,
		ResponseBody: []byte(`{"apiVersion":"v1","kind":"PodList","items":[]}`),
	}
	if _, err := w.WriteRecord(rec); !errors.Is(err, wantErr) {
		t.Fatalf("WriteRecord error = %v, want %v", err, wantErr)
	}
	if got := w.RecordCount(); got != 0 {
		t.Errorf("RecordCount() = %d, want 0 after a failed write", got)
	}
	if got := w.UncompressedBytes(); got != 0 {
		t.Errorf("UncompressedBytes() = %d, want 0 — a record that failed to write must not be counted", got)
	}
	if got := w.pathSeq["/api/v1/namespaces/default/pods"]; got != 0 {
		t.Errorf("pathSeq[...] = %d, want 0 — a failed write must not consume a seq number", got)
	}
}

// TestNDJSONWriter_WriteRecord_NilRecord verifies a nil *format.Record
// returns a clean error rather than panicking on a nil-pointer dereference —
// a regression risk introduced when WriteRecord's parameter was retyped from
// any to *format.Record (#233).
func TestNDJSONWriter_WriteRecord_NilRecord(t *testing.T) {
	w := NewNDJSONWriter(io.Discard)
	if _, err := w.WriteRecord(nil); err == nil {
		t.Fatal("WriteRecord(nil) succeeded, want error")
	}
}
