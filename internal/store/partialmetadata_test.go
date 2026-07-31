package store

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// kube-controller-manager's garbagecollector requests ownerReference targets
// metadata-only. Returning the full object made it fail to decode and retry
// forever (#329), so these pin both the negotiation and the projection.

func TestWantsPartialObjectMetadata(t *testing.T) {
	cases := []struct {
		name, accept, wantVer string
		want                  bool
	}{
		{"garbagecollector's header", "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1", "v1", true},
		{"v1beta1", "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1beta1", "v1beta1", true},
		{"protobuf first, json fallback", "application/vnd.kubernetes.protobuf;as=PartialObjectMetadata;g=meta.k8s.io;v=v1,application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1", "v1", true},
		{"missing v defaults to v1", "application/json;as=PartialObjectMetadata;g=meta.k8s.io", "v1", true},
		// as=Table uses the same parameter style and must not be mistaken for it.
		{"Table", "application/json;as=Table;g=meta.k8s.io;v=v1", "", false},
		{"wrong group", "application/json;as=PartialObjectMetadata;g=example.com;v=v1", "", false},
		{"plain json", "application/json", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/pods", nil)
			if tc.accept != "" {
				r.Header.Set("Accept", tc.accept)
			}
			gotVer, got := WantsPartialObjectMetadata(r)
			if got != tc.want || gotVer != tc.wantVer {
				t.Errorf("= (%q, %v), want (%q, %v)", gotVer, got, tc.wantVer, tc.want)
			}
		})
	}
}

func TestProjectPartialMetadata_SingleObject(t *testing.T) {
	body := []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"ns","uid":"u1"},"data":{"secret-ish":"value"}}`)
	got, ok := projectPartialMetadata(body, "v1")
	if !ok {
		t.Fatal("projection refused a plain object")
	}
	var out map[string]any
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["kind"] != "PartialObjectMetadata" || out["apiVersion"] != "meta.k8s.io/v1" {
		t.Errorf("kind/apiVersion = %v/%v", out["kind"], out["apiVersion"])
	}
	// The point of the projection: the payload is gone, the metadata isn't.
	if _, leaked := out["data"]; leaked {
		t.Error("data survived the metadata projection")
	}
	md, _ := out["metadata"].(map[string]any)
	if md["name"] != "cm" || md["namespace"] != "ns" || md["uid"] != "u1" {
		t.Errorf("metadata = %v", md)
	}
}

func TestProjectPartialMetadata_List(t *testing.T) {
	body := []byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"77"},"items":[
		{"metadata":{"name":"a","namespace":"ns"},"spec":{"nodeName":"n1"}},
		{"metadata":{"name":"b","namespace":"ns"},"spec":{"nodeName":"n2"}}
	]}`)
	got, ok := projectPartialMetadata(body, "v1")
	if !ok {
		t.Fatal("projection refused a list")
	}
	var out struct {
		Kind       string `json:"kind"`
		APIVersion string `json:"apiVersion"`
		Metadata   struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Kind != "PartialObjectMetadataList" || out.APIVersion != "meta.k8s.io/v1" {
		t.Errorf("kind/apiVersion = %s/%s", out.Kind, out.APIVersion)
	}
	// The list's own resourceVersion must survive, or paging and watch bookmarks
	// stop lining up.
	if out.Metadata.ResourceVersion != "77" {
		t.Errorf("list resourceVersion = %q, want 77", out.Metadata.ResourceVersion)
	}
	if len(out.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(out.Items))
	}
	for i, it := range out.Items {
		if it["kind"] != "PartialObjectMetadata" {
			t.Errorf("item %d kind = %v", i, it["kind"])
		}
		if _, leaked := it["spec"]; leaked {
			t.Errorf("item %d kept its spec", i)
		}
	}
}

// A Status must pass through untouched: a client asking for metadata still needs
// to decode a 404 as Status, not receive a projected object.
func TestProjectPartialMetadata_PassesThroughStatusAndNonObjects(t *testing.T) {
	for _, body := range []string{
		`{"kind":"Status","apiVersion":"v1","status":"Failure","code":404,"reason":"NotFound"}`,
		`{"paths":["/api","/apis"]}`, // no kind — discovery root
		`not json at all`,
		`{"kind":"ConfigMap","apiVersion":"v1"}`, // object with no metadata
	} {
		if _, ok := projectPartialMetadata([]byte(body), "v1"); ok {
			t.Errorf("projected a body it should have passed through: %s", body)
		}
	}
}

// The writer must leave a non-JSON content type alone.
func TestPartialMetadataResponseWriter_NonJSONPassesThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewPartialMetadataResponseWriter(rec, "v1")
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
	w.Flush()
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q, want ok", got)
	}
	if !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Accept") {
		t.Error("Vary: Accept not set; a cache could reuse this across Accept headers")
	}
}
