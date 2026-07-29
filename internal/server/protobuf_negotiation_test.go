package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	kstore "github.com/phenixblue/k8shark/internal/store"
)

// getWithAccept issues a GET with an Accept header and returns status, the
// response Content-Type, and the raw body.
func getWithAccept(t *testing.T, url, accept string) (int, string, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Accept", accept)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), b
}

// A protobuf-preferring client gets a protobuf response for a built-in list; a
// JSON client still gets JSON. (issue #150) End-to-end coverage for the
// handler.go ResponseWriter wrapper that internal/store's WantsProtobuf/
// jsonToProtobuf unit tests don't exercise on their own.
func TestProtobufResponseNegotiation(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{podsPath: {id: "s", at: from, body: podList("pod-base")}}, nil)
	h := newHandler(store, from, false)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// Protobuf-preferring client: protobuf response that decodes back to a List.
	code, ct, body := getWithAccept(t, srv.URL+podsPath, "application/vnd.kubernetes.protobuf,application/json")
	if code != http.StatusOK {
		t.Fatalf("protobuf GET: status %d, want 200", code)
	}
	if ct != kstore.ProtobufMediaType {
		t.Errorf("Content-Type = %q, want %q", ct, kstore.ProtobufMediaType)
	}
	if len(body) < 4 || string(body[:4]) != "k8s\x00" {
		t.Errorf("protobuf framing missing, got %q", body[:min(4, len(body))])
	}
	if obj, _, err := protobufSerializer.Decode(body, nil, nil); err != nil || obj == nil {
		t.Errorf("protobuf body did not decode: %v", err)
	}

	// JSON client: unchanged JSON response.
	code, ct, _ = getWithAccept(t, srv.URL+podsPath, "application/json")
	if code != http.StatusOK {
		t.Fatalf("json GET: status %d, want 200", code)
	}
	if ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}
