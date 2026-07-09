package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWantsProtobuf(t *testing.T) {
	cases := map[string]bool{
		"application/vnd.kubernetes.protobuf,application/json": true,
		"application/vnd.kubernetes.protobuf":                  true,
		"application/json":                                     false,
		"":                                                     false,
		"application/json;as=Table;v=v1;g=meta.k8s.io":                      false,
		"application/vnd.kubernetes.protobuf;q=0,application/json":          false, // protobuf explicitly refused
		"application/json;q=0.9,application/vnd.kubernetes.protobuf;q=1":    true,  // protobuf preferred
		"application/json,application/vnd.kubernetes.protobuf;q=0.8":        false, // json preferred (q=1 > 0.8)
		"application/vnd.kubernetes.protobuf;stream=watch,application/json": true,  // params ignored for the base type
		"application/json,application/vnd.kubernetes.protobuf":              false, // equal q → header order wins (json first)
		"application/vnd.kubernetes.protobuf,application/json;q=0.9":        true,  // protobuf first and higher q
		"application/json;as=Table;v=v1;g=meta.k8s.io,application/json":     false, // Table-first, no protobuf offered
	}
	for accept, want := range cases {
		r, _ := http.NewRequest(http.MethodGet, "/api/v1/pods", nil)
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		if got := wantsProtobuf(r); got != want {
			t.Errorf("wantsProtobuf(Accept=%q) = %v, want %v", accept, got, want)
		}
	}
}

func TestJSONToProtobuf(t *testing.T) {
	// A built-in type round-trips to protobuf (k8s\x00-framed).
	pb, ok := jsonToProtobuf([]byte(`{"apiVersion":"v1","kind":"ConfigMapList","metadata":{},"items":[]}`))
	if !ok {
		t.Fatal("jsonToProtobuf(ConfigMapList) ok=false, want true")
	}
	if len(pb) < 4 || string(pb[:4]) != "k8s\x00" {
		t.Errorf("protobuf framing missing, got %q", pb[:min(4, len(pb))])
	}
	// A non-scheme body (e.g. an OpenAPI doc) is left alone.
	if _, ok := jsonToProtobuf([]byte(`{"swagger":"2.0","info":{}}`)); ok {
		t.Error("jsonToProtobuf(swagger doc) ok=true, want false (should pass through as JSON)")
	}
}

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
// JSON client still gets JSON. (issue #150)
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
	if ct != protobufMediaType {
		t.Errorf("Content-Type = %q, want %q", ct, protobufMediaType)
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
