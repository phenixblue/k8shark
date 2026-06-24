package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/phenixblue/k8shark/internal/archive"
	"github.com/phenixblue/k8shark/internal/capture"
)

func TestParseReplayAt(t *testing.T) {
	start := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	end := time.Date(2026, 4, 10, 10, 10, 0, 0, time.UTC)

	t.Run("empty selects latest", func(t *testing.T) {
		got, err := parseReplayAt(start, end, "")
		if err != nil || !got.IsZero() {
			t.Fatalf("got (%v, %v), want (zero, nil)", got, err)
		}
	})
	t.Run("rfc3339 within window", func(t *testing.T) {
		got, err := parseReplayAt(start, end, "2026-04-10T10:05:00Z")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !got.Equal(time.Date(2026, 4, 10, 10, 5, 0, 0, time.UTC)) {
			t.Errorf("got %v", got)
		}
	})
	t.Run("relative duration off end", func(t *testing.T) {
		got, err := parseReplayAt(start, end, "-2m")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if !got.Equal(end.Add(-2 * time.Minute)) {
			t.Errorf("got %v, want %v", got, end.Add(-2*time.Minute))
		}
	})
	t.Run("before start errors", func(t *testing.T) {
		if _, err := parseReplayAt(start, end, "2026-04-10T09:00:00Z"); err == nil {
			t.Error("expected error for time before capture start")
		}
	})
	t.Run("after end errors", func(t *testing.T) {
		if _, err := parseReplayAt(start, end, "2026-04-10T11:00:00Z"); err == nil {
			t.Error("expected error for time after capture end")
		}
	})
	t.Run("garbage errors", func(t *testing.T) {
		if _, err := parseReplayAt(start, end, "not-a-time"); err == nil {
			t.Error("expected error for unparseable --at")
		}
	})
}

func TestServeRoot(t *testing.T) {
	t.Run("root redirects to v2", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		serveRoot(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("status = %d, want 302", w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/v2/" {
			t.Errorf("Location = %q, want /v2/", loc)
		}
	})
	t.Run("unknown path 404s", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/nope", nil)
		w := httptest.NewRecorder()
		serveRoot(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})
}

// TestOpen_ClosesArchiveOnShutdown is a regression test for the file-handle
// leak introduced when the UI moved to the held-open ZIP archive: Shutdown must
// release the archive's underlying file descriptor. We assert this by checking
// that a second Close on the archive errors — proving Shutdown already closed it
// (closing an already-closed *os.File returns os.ErrClosed).
func TestOpen_ClosesArchiveOnShutdown(t *testing.T) {
	path := writeMinimalArchive(t)
	srv, err := Open(OpenOptions{ArchivePath: path, Port: "0"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if srv.archive == nil {
		t.Fatal("Server did not retain the archive; it can never be closed")
	}

	srv.Shutdown()

	if err := srv.archive.Close(); err == nil {
		t.Error("archive still open after Shutdown; expected it to be closed (file-handle leak)")
	}
}

// TestOpen_ServesV2 is an end-to-end check that Open wires the v2 dashboard:
// "/" redirects to /v2/ and the v2 API responds.
func TestOpen_ServesV2(t *testing.T) {
	path := writeMinimalArchive(t)
	srv, err := Open(OpenOptions{ArchivePath: path, Port: "0"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer srv.Shutdown()

	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // don't follow, so we can assert the 302
		},
	}

	resp, err := client.Get(srv.Address() + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/v2/" {
		t.Errorf("GET / = %d %q, want 302 -> /v2/", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, err = client.Get(srv.Address() + "/v2/api/capture")
	if err != nil {
		t.Fatalf("GET /v2/api/capture: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v2/api/capture = %d, want 200", resp.StatusCode)
	}
	var info struct {
		CaptureID string `json:"capture_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode capture info: %v", err)
	}
	if info.CaptureID != "ui-open-test" {
		t.Errorf("capture_id = %q, want ui-open-test", info.CaptureID)
	}
}

func writeMinimalArchive(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "capture.khsrk")
	now := time.Date(2026, 4, 10, 10, 0, 0, 0, time.UTC)
	podList := `{"apiVersion":"v1","kind":"PodList","items":[{"metadata":{"name":"p","namespace":"default"}}]}`
	recs := []*capture.Record{
		{ID: "r1", CapturedAt: now, APIPath: "/api/v1/namespaces/default/pods", HTTPMethod: "GET", ResponseCode: 200, ResponseBody: json.RawMessage(podList)},
	}
	idx := capture.Index{
		"/api/v1/namespaces/default/pods": {APIPath: "/api/v1/namespaces/default/pods", Seqs: []int{0}, Times: []time.Time{now}},
	}
	meta := &capture.CaptureMetadata{CaptureID: "ui-open-test", CapturedAt: now.Add(-5 * time.Minute), CapturedUntil: now, RecordCount: len(recs)}

	sw, err := archive.NewStreamWriter(out)
	if err != nil {
		t.Fatalf("NewStreamWriter: %v", err)
	}
	for _, r := range recs {
		if err := sw.WriteRecord(r); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
	}
	if err := sw.Finish(meta, idx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return out
}
