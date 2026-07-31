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

		// q-values, mirroring WantsProtobuf. Returning on the first syntactic
		// match would project even where the client said not to.
		{"metadata clause disabled with q=0", "application/json, application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1;q=0", "", false},
		{"metadata outranked by a higher-q plain clause", "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1;q=0.5, application/json;q=0.9", "", false},
		{"metadata wins on higher q despite coming second", "application/json;q=0.5, application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1;q=0.9", "v1", true},
		{"equal q keeps the earlier clause (plain first)", "application/json;q=0.8, application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1;q=0.8", "", false},
		{"equal q keeps the earlier clause (metadata first)", "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1;q=0.8, application/json;q=0.8", "v1", true},
		{"unrelated media type does not outrank", "text/plain;q=1.0, application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1;q=0.5", "v1", true},

		// Only versions we can name are servable. Echoing an unknown v= into
		// apiVersion would hand back `meta.k8s.io/<typo>` while claiming the
		// request was honored.
		{"unknown version loses negotiation", "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v99", "", false},
		{"typo'd version loses negotiation", "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1beta", "", false},
		{"unknown version does not shadow a good clause", "application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v99;q=0.9, application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1;q=0.5", "v1", true},
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

// A list containing an item that can't be projected must pass through whole
// rather than come back shorter. The garbagecollector is the main consumer of
// this projection, and an item silently missing from its input can read as "the
// owner is gone" — so a lossy projection is worse than none.
func TestProjectPartialMetadata_ListRefusesRatherThanDropItems(t *testing.T) {
	cases := map[string]string{
		"item missing metadata": `{"kind":"PodList","apiVersion":"v1","items":[
			{"metadata":{"name":"a","namespace":"ns"}},
			{"spec":{"nodeName":"n2"}}
		]}`,
		"item is not an object": `{"kind":"PodList","apiVersion":"v1","items":[
			{"metadata":{"name":"a","namespace":"ns"}},
			"not-an-object"
		]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got, ok := projectPartialMetadata([]byte(body), "v1"); ok {
				t.Errorf("projected a list with an unprojectable item instead of passing it through; got %s", got)
			}
		})
	}

	// Sanity: a fully projectable list of the same shape still projects.
	good := `{"kind":"PodList","apiVersion":"v1","items":[
		{"metadata":{"name":"a","namespace":"ns"}},
		{"metadata":{"name":"b","namespace":"ns"}}
	]}`
	if _, ok := projectPartialMetadata([]byte(good), "v1"); !ok {
		t.Error("refused a list whose items are all projectable")
	}
}

// An empty list is projectable — there is nothing to drop, and returning
// PartialObjectMetadataList with zero items is what a real apiserver does.
func TestProjectPartialMetadata_EmptyList(t *testing.T) {
	got, ok := projectPartialMetadata([]byte(`{"kind":"PodList","apiVersion":"v1","items":[]}`), "v1")
	if !ok {
		t.Fatal("refused an empty list")
	}
	var out struct {
		Kind  string           `json:"kind"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Kind != "PartialObjectMetadataList" || len(out.Items) != 0 {
		t.Errorf("kind=%s items=%d, want PartialObjectMetadataList with 0 items", out.Kind, len(out.Items))
	}
	if out.Items == nil {
		t.Error("items is null; it must be [] so clients can iterate it")
	}
}

// A kind ending in "List" is not proof of an object list. The discovery
// documents APIResourceList and APIGroupList end in "List" but carry
// `resources`/`groups`, and projecting them emptied discovery outright: /api/v1
// went from 38 resources to 0 and /apis from 93 groups to 0 for any client that
// bootstrapped with a metadata Accept header.
func TestProjectPartialMetadata_DiscoveryListsAreNotObjectLists(t *testing.T) {
	for name, body := range map[string]string{
		"APIResourceList": `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[
			{"name":"pods","namespaced":true,"kind":"Pod"},
			{"name":"nodes","namespaced":false,"kind":"Node"}
		]}`,
		"APIGroupList": `{"kind":"APIGroupList","apiVersion":"v1","groups":[
			{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}]}
		]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := projectPartialMetadata([]byte(body), "v1"); ok {
				t.Errorf("projected a discovery document, discarding its payload; got %s", got)
			}
		})
	}

	// A genuine list with an items field still projects, including an empty one —
	// the distinction is whether `items` is present, not whether it has entries.
	for name, body := range map[string]string{
		"populated": `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"a"}}]}`,
		"empty":     `{"kind":"PodList","apiVersion":"v1","items":[]}`,
	} {
		t.Run("still projects/"+name, func(t *testing.T) {
			if _, ok := projectPartialMetadata([]byte(body), "v1"); !ok {
				t.Error("refused a genuine object list")
			}
		})
	}
}

// Vary: Accept must appear once even when both negotiation wrappers stack, which
// happens when a client prefers protobuf *and* asks for a metadata projection.
func TestVaryAccept_NotDuplicatedWhenWritersStack(t *testing.T) {
	rec := httptest.NewRecorder()
	inner := NewProtobufResponseWriter(rec)
	outer := NewPartialMetadataResponseWriter(inner, "v1")

	outer.Header().Set("Content-Type", "application/json")
	outer.WriteHeader(http.StatusOK)
	_, _ = outer.Write([]byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"},"data":{"k":"v"}}`))
	// Same order as the handler's LIFO defers: projection, then protobuf.
	outer.Flush()
	inner.Flush()

	var accepts int
	for _, v := range rec.Header().Values("Vary") {
		for _, field := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(field), "Accept") {
				accepts++
			}
		}
	}
	if accepts != 1 {
		t.Errorf("Vary lists Accept %d times, want exactly 1; got %v", accepts, rec.Header().Values("Vary"))
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
