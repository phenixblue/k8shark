package server

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsProtobufContentType(t *testing.T) {
	cases := map[string]bool{
		"application/vnd.kubernetes.protobuf":            true,
		"application/vnd.kubernetes.protobuf; charset=x": true,
		"Application/VND.Kubernetes.Protobuf":            true,
		"application/json":                               false,
		"":                                               false,
		"application/vnd.kubernetes.protobuf;stream=watch": true,
	}
	for ct, want := range cases {
		if got := isProtobufContentType(ct); got != want {
			t.Errorf("isProtobufContentType(%q) = %v, want %v", ct, got, want)
		}
	}
}

// doReqBytes issues a request with a raw (binary) body — protobuf can't go
// through the string-based doReq helper cleanly.
func doReqBytes(t *testing.T, method, url, ctype string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	return resp.StatusCode, b
}

func protobufPod(t *testing.T, name string) []byte {
	t.Helper()
	pod := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
	}
	var buf bytes.Buffer
	if err := protobufSerializer.Encode(pod, &buf); err != nil {
		t.Fatalf("encode protobuf pod: %v", err)
	}
	return buf.Bytes()
}

// A client-go/kubectl-style protobuf create body is decoded, stored, and
// round-trips like a JSON create. (issue #148)
func TestOverlay_ProtobufCreate(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	pb := protobufPod(t, "pb-pod")
	if len(pb) < 4 || string(pb[:4]) != "k8s\x00" {
		t.Fatalf("expected k8s protobuf framing, got %q", pb[:min(4, len(pb))])
	}

	code, created := doReqBytes(t, http.MethodPost, srv.URL+podsPath,
		"application/vnd.kubernetes.protobuf", pb)
	if code != http.StatusCreated {
		t.Fatalf("protobuf create: status %d, want 201\n%s", code, created)
	}
	if n := metaString(created, "name"); n != "pb-pod" {
		t.Errorf("created name = %q, want pb-pod\n%s", n, created)
	}

	// It round-trips on a subsequent GET.
	code, got := doReq(t, http.MethodGet, srv.URL+podsPath+"/pb-pod", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET pb-pod: status %d, want 200", code)
	}
	if n := metaString(got, "name"); n != "pb-pod" {
		t.Errorf("GET name = %q, want pb-pod", n)
	}

	// A malformed protobuf body is a clean 400, not a panic.
	code, _ = doReqBytes(t, http.MethodPost, srv.URL+podsPath,
		"application/vnd.kubernetes.protobuf", []byte("not-protobuf"))
	if code != http.StatusBadRequest {
		t.Errorf("garbage protobuf: status %d, want 400", code)
	}
}

// readObjectBody also serves replace (PUT); cover the protobuf overlayReplace
// path, not just create. (issue #148)
func TestOverlay_ProtobufReplace(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// "pod-base" exists in the replayed state; replace it via a protobuf PUT.
	pb := protobufPod(t, "pod-base")
	code, got := doReqBytes(t, http.MethodPut, srv.URL+podsPath+"/pod-base",
		"application/vnd.kubernetes.protobuf", pb)
	if code != http.StatusOK {
		t.Fatalf("protobuf replace: status %d, want 200\n%s", code, got)
	}
	if n := metaString(got, "name"); n != "pod-base" {
		t.Errorf("replaced name = %q, want pod-base\n%s", n, got)
	}

	// A malformed protobuf replace body is a clean 400.
	code, _ = doReqBytes(t, http.MethodPut, srv.URL+podsPath+"/pod-base",
		"application/vnd.kubernetes.protobuf", []byte("not-protobuf"))
	if code != http.StatusBadRequest {
		t.Errorf("garbage protobuf replace: status %d, want 400", code)
	}
}
