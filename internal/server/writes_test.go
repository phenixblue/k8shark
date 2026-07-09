package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const podsPath = "/api/v1/namespaces/default/pods"

// newWritableServer builds a writable replay handler over the given store/clock.
func newWritableServer(t *testing.T, store *CaptureStore, clock *ReplayClock) *httptest.Server {
	t.Helper()
	h := newHandler(store, time.Time{}, false)
	h.clock = clock
	h.overlay = newOverlay()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func doReq(t *testing.T, method, url, ctype, body string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func bodyRV(t *testing.T, b []byte) string {
	t.Helper()
	return metaString(b, "resourceVersion")
}

func listNames(t *testing.T, b []byte) []string {
	t.Helper()
	var l struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatalf("decode list: %v\n%s", err, b)
	}
	var names []string
	for _, it := range l.Items {
		names = append(names, metaString(it, "name"))
	}
	return names
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func writableTestStore(t *testing.T, from time.Time) *CaptureStore {
	return buildTestStoreWithWatch(t,
		map[string]watchTestRecord{podsPath: {id: "s", at: from, body: podList("pod-base")}},
		nil)
}

func TestOverlay_CreateGetList(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// Create pod-new.
	code, body := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-new"))
	if code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", code, body)
	}
	if rv := bodyRV(t, body); rv == "" || rv == "0" {
		t.Errorf("created object rv = %q, want non-zero", rv)
	}
	if metaString(body, "uid") == "" || metaString(body, "creationTimestamp") == "" {
		t.Errorf("created object missing uid/creationTimestamp: %s", body)
	}

	// GET the object.
	code, got := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-new", "", "")
	if code != 200 || metaString(got, "name") != "pod-new" {
		t.Fatalf("GET pod-new: status %d name %q", code, metaString(got, "name"))
	}

	// LIST includes both the replay base and the overlay object.
	code, list := doReq(t, http.MethodGet, srv.URL+podsPath, "", "")
	if code != 200 {
		t.Fatalf("list: status %d", code)
	}
	names := listNames(t, list)
	if !contains(names, "pod-base") || !contains(names, "pod-new") {
		t.Errorf("list = %v, want both pod-base and pod-new", names)
	}
}

func TestOverlay_ReplaceAndPatch(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-x"))

	// PUT replace with a label.
	replaced := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-x","namespace":"default","labels":{"team":"a"}}}`
	code, body := doReq(t, http.MethodPut, srv.URL+podsPath+"/pod-x", "application/json", replaced)
	if code != 200 {
		t.Fatalf("put: status %d: %s", code, body)
	}

	// JSON merge patch adds another label.
	code, patched := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-x",
		"application/merge-patch+json", `{"metadata":{"labels":{"tier":"web"}}}`)
	if code != 200 {
		t.Fatalf("patch: status %d: %s", code, patched)
	}
	var obj struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(patched, &obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if obj.Metadata.Labels["team"] != "a" || obj.Metadata.Labels["tier"] != "web" {
		t.Errorf("merged labels = %v, want team=a tier=web", obj.Metadata.Labels)
	}
}

func TestOverlay_DeleteTombstone(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// Delete a replay-base object → tombstone; GET 404, LIST excludes it.
	code, _ := doReq(t, http.MethodDelete, srv.URL+podsPath+"/pod-base", "", "")
	if code != 200 {
		t.Fatalf("delete: status %d", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-base", "", ""); code != 404 {
		t.Errorf("GET after delete: status %d, want 404", code)
	}
	_, list := doReq(t, http.MethodGet, srv.URL+podsPath, "", "")
	if contains(listNames(t, list), "pod-base") {
		t.Errorf("list still contains deleted pod-base: %v", listNames(t, list))
	}
}

func TestOverlay_WinsOverReplayAndRVMonotonic(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// Overwrite the replay-base object; the list must show the overlay copy.
	_, b1 := doReq(t, http.MethodPut, srv.URL+podsPath+"/pod-base", "application/json",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-base","namespace":"default","labels":{"owned":"yes"}}}`)
	_, b2 := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-2"))
	n1, _ := strconv.Atoi(bodyRV(t, b1))
	n2, _ := strconv.Atoi(bodyRV(t, b2))
	if n1 <= 0 || n2 <= n1 {
		t.Errorf("RVs not monotonic: rv1=%d rv2=%d", n1, n2)
	}

	code, got := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-base", "", "")
	if code != 200 {
		t.Fatalf("get pod-base: %d", code)
	}
	if !strings.Contains(string(got), `"owned":"yes"`) {
		t.Errorf("overlay did not win for pod-base: %s", got)
	}
}

func TestOverlay_ResetOnLoop(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, advance := newTestClock(t, from, from.Add(10*time.Second), 1, true /*loop*/, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-tmp"))
	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-tmp", "", ""); code != 200 {
		t.Fatalf("pod-tmp should exist before loop: %d", code)
	}

	advance(15 * time.Second) // cross the window end → loop wrap (epoch advances)

	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-tmp", "", ""); code != 404 {
		t.Errorf("pod-tmp should be cleared after loop wrap, got status %d", code)
	}
}

func TestOverlay_ManualReset(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-tmp"))
	code, _ := doReq(t, http.MethodPost, srv.URL+"/_k8shark/replay/reset-overlay", "", "")
	if code != 200 {
		t.Fatalf("reset-overlay: status %d", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-tmp", "", ""); code != 404 {
		t.Errorf("pod-tmp should be gone after manual reset, got %d", code)
	}
}

func TestOverlay_WriteValidationAndGeneration(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	if code, _ := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", "{not json"); code != http.StatusBadRequest {
		t.Errorf("invalid-JSON create: status %d, want 400", code)
	}
	if code, _ := doReq(t, http.MethodPost, srv.URL+podsPath+"/pod-x", "application/json", podBody("pod-x")); code != http.StatusMethodNotAllowed {
		t.Errorf("POST to item path: status %d, want 405", code)
	}
	if code, _ := doReq(t, http.MethodPut, srv.URL+podsPath+"/ghost", "application/json", podBody("ghost")); code != http.StatusNotFound {
		t.Errorf("PUT missing object: status %d, want 404", code)
	}

	// generation: 1 on create, bumped on replace.
	_, created := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-g"))
	if g := metaInt(created, "generation"); g != 1 {
		t.Errorf("created generation = %d, want 1", g)
	}
	_, updated := doReq(t, http.MethodPut, srv.URL+podsPath+"/pod-g", "application/json",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-g","namespace":"default","labels":{"x":"y"}}}`)
	if g := metaInt(updated, "generation"); g != 2 {
		t.Errorf("replaced generation = %d, want 2", g)
	}

	if code, _ := doReq(t, http.MethodDelete, srv.URL+podsPath+"/pod-g/status", "", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE subresource: status %d, want 405", code)
	}
}

func TestOverlay_StatusSubresource(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-s"))
	if code, _ := doReq(t, http.MethodPut, srv.URL+podsPath+"/pod-s/status", "application/json",
		`{"status":{"phase":"Running"}}`); code != 200 {
		t.Fatalf("PUT status: %d", code)
	}
	code, got := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-s/status", "", "")
	if code != 200 || !strings.Contains(string(got), `"phase":"Running"`) {
		t.Errorf("GET status: %d body=%s, want status.phase Running", code, got)
	}
}

func TestOverlay_CreateConflict(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// Creating over a replay-base object → 409.
	if code, _ := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-base")); code != http.StatusConflict {
		t.Errorf("create over replay object: status %d, want 409", code)
	}
	// Create then create again → 409.
	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-dup"))
	if code, _ := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-dup")); code != http.StatusConflict {
		t.Errorf("duplicate create: status %d, want 409", code)
	}
}

func TestOverlay_UnknownSubresourceRejected(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)
	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-x"))

	if code, _ := doReq(t, http.MethodPut, srv.URL+podsPath+"/pod-x/scale", "application/json", `{"spec":{"replicas":2}}`); code != http.StatusMethodNotAllowed {
		t.Errorf("PUT unknown subresource: status %d, want 405", code)
	}
	if code, _ := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-x/scale", "application/merge-patch+json", `{"spec":{"replicas":2}}`); code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH unknown subresource: status %d, want 405", code)
	}
}

// TestOverlay_ListThenWatchNoRelistLoop verifies a LIST RV bumped by an overlay
// write is still a valid WATCH resume point (no 410 relist loop).
func TestOverlay_ListThenWatchNoRelistLoop(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-x"))
	_, list := doReq(t, http.MethodGet, srv.URL+podsPath, "", "")
	listRV := metaString(list, "resourceVersion")
	if listRV == "" {
		// list-level RV is in metadata.resourceVersion; metaString reads metadata.*
		var l struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(list, &l)
		listRV = l.Metadata.ResourceVersion
	}
	resp, err := http.Get(srv.URL + podsPath + "?watch=1&resourceVersion=" + listRV + "&timeoutSeconds=1")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("WATCH from list RV %s: status %d, want 200 (no 410 relist loop)", listRV, resp.StatusCode)
	}
}

// TestOverlay_NullBodyNoPanic ensures client-supplied "null"/non-object write
// bodies are rejected with 400 rather than crashing the server (nil-map panic).
func TestOverlay_NullBodyNoPanic(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	for _, body := range []string{"null", `["a"]`, `"scalar"`, `{"metadata":null,"kind":"Pod"}`} {
		if code, _ := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", body); code != http.StatusBadRequest {
			t.Errorf("POST body %q: status %d, want 400", body, code)
		}
	}
	// A merge patch of "null" on an existing object must not panic (422, not 500).
	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-n"))
	if code, _ := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-n", "application/merge-patch+json", "null"); code != http.StatusUnprocessableEntity {
		t.Errorf("merge-patch null: status %d, want 422", code)
	}
	// An unknown/empty PATCH Content-Type is rejected with 415, not merge-patched.
	if code, _ := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-n", "text/plain", `{"x":1}`); code != http.StatusUnsupportedMediaType {
		t.Errorf("unknown patch content-type: status %d, want 415", code)
	}
	// Media types are case-insensitive.
	if code, _ := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-n", "Application/Merge-Patch+JSON", `{"metadata":{"labels":{"c":"d"}}}`); code != 200 {
		t.Errorf("mixed-case patch content-type: status %d, want 200", code)
	}
	if code, _ := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-n", "", `{"x":1}`); code != http.StatusUnsupportedMediaType {
		t.Errorf("empty patch content-type: status %d, want 415", code)
	}
}

// TestOverlay_StatusPatchIsolated verifies a PATCH to .../status only changes
// status (not spec/metadata) and does not bump generation, while a spec PATCH
// does bump it.
func TestOverlay_StatusPatchIsolated(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-s")) // generation 1

	// Status patch also tries to sneak in a label — the label must be ignored.
	doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-s/status", "application/merge-patch+json",
		`{"metadata":{"labels":{"hacked":"x"}},"status":{"phase":"Running"}}`)
	_, got := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-s", "", "")
	if !strings.Contains(string(got), `"phase":"Running"`) {
		t.Errorf("status not applied: %s", got)
	}
	if strings.Contains(string(got), `"hacked"`) {
		t.Errorf("status patch leaked a metadata change: %s", got)
	}
	if g := metaInt(got, "generation"); g != 1 {
		t.Errorf("status patch bumped generation to %d, want 1", g)
	}

	// A spec patch bumps generation.
	doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-s", "application/merge-patch+json", `{"spec":{"x":"y"}}`)
	_, got2 := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-s", "", "")
	if g := metaInt(got2, "generation"); g != 2 {
		t.Errorf("spec patch generation = %d, want 2", g)
	}
}

// TestOverlay_CreateNamespaceMismatch verifies a body namespace that disagrees
// with the request-path namespace is rejected.
func TestOverlay_CreateNamespaceMismatch(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	body := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-m","namespace":"other"}}`
	if code, _ := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", body); code != http.StatusBadRequest {
		t.Errorf("create namespace mismatch: status %d, want 400", code)
	}
	// PUT with a body name that disagrees with the URL is rejected.
	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-ok"))
	wrong := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"different","namespace":"default"}}`
	if code, _ := doReq(t, http.MethodPut, srv.URL+podsPath+"/pod-ok", "application/json", wrong); code != http.StatusBadRequest {
		t.Errorf("PUT name mismatch: status %d, want 400", code)
	}
}

// TestOverlay_ListSelectorFiltersOverlay verifies label selectors filter overlay
// items consistently with replayed items.
func TestOverlay_ListSelectorFiltersOverlay(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-la","namespace":"default","labels":{"app":"x"}}}`)
	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-lb","namespace":"default","labels":{"app":"y"}}}`)

	_, list := doReq(t, http.MethodGet, srv.URL+podsPath+"?labelSelector=app%3Dx", "", "")
	names := listNames(t, list)
	if !contains(names, "pod-la") {
		t.Errorf("selector app=x should include pod-la; got %v", names)
	}
	if contains(names, "pod-lb") || contains(names, "pod-base") {
		t.Errorf("selector app=x leaked non-matching items: %v", names)
	}
}

// TestOverlay_TableReflectsWrite verifies a Table LIST (kubectl's default) shows
// overlay-created objects.
func TestOverlay_TableReflectsWrite(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-t"))

	req, _ := http.NewRequest(http.MethodGet, srv.URL+podsPath, nil)
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("table list: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"kind":"Table"`) {
		t.Fatalf("expected a Table response, got: %s", b)
	}
	if !strings.Contains(string(b), "pod-t") {
		t.Errorf("Table LIST did not reflect overlay write pod-t: %s", b)
	}
}

// TestOverlay_CrossScopeRVIsolation verifies a write to one resource does not
// inflate another resource's list resourceVersion (RVs are per path).
func TestOverlay_CrossScopeRVIsolation(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// A write to configmaps bumps the global overlay counter.
	doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces/default/configmaps", "application/json",
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"default"}}`)

	// The pods LIST RV must be unaffected by the configmap write.
	_, list := doReq(t, http.MethodGet, srv.URL+podsPath, "", "")
	if rv := metaString(list, "resourceVersion"); rv != "1" {
		t.Errorf("pods list RV = %q, want \"1\" (not inflated by the configmap write)", rv)
	}
}

// TestOverlay_ApplyPatchYAML verifies apply-patch+yaml bodies (YAML) are parsed
// and merged (interim SSA behavior).
func TestOverlay_ApplyPatchYAML(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-a"))
	yamlBody := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: pod-a\n  namespace: default\n  labels:\n    applied: \"yes\"\n"
	code, got := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-a", "application/apply-patch+yaml", yamlBody)
	if code != 200 {
		t.Fatalf("apply-patch yaml: status %d: %s", code, got)
	}
	if !strings.Contains(string(got), `"applied":"yes"`) {
		t.Errorf("apply-patch did not merge YAML body: %s", got)
	}
}

func TestOverlay_ReadOnlyRejectsWrites(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	// Read-only replay handler (no overlay).
	h := newHandler(writableTestStore(t, from), time.Time{}, false)
	h.clock = clock
	srv := httptest.NewServer(h)
	defer srv.Close()

	code, _ := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("nope"))
	if code != http.StatusMethodNotAllowed {
		t.Errorf("read-only POST: status %d, want 405", code)
	}
}

func TestOverlay_DefaultServiceAccountOnNamespaceCreate(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// Creating a namespace synthesizes its `default` ServiceAccount (a real
	// cluster's controller would); the overlay has none.
	nsBody := `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"ns-x"}}`
	if code, _ := doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces", "application/json", nsBody); code != http.StatusCreated {
		t.Fatalf("create namespace: status %d, want 201", code)
	}

	saPath := "/api/v1/namespaces/ns-x/serviceaccounts"
	code, got := doReq(t, http.MethodGet, srv.URL+saPath+"/default", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET default SA: status %d, want 200\n%s", code, got)
	}
	if n := metaString(got, "name"); n != "default" {
		t.Errorf("SA name = %q, want default", n)
	}
	if rv := bodyRV(t, got); rv == "" || rv == "0" {
		t.Errorf("SA resourceVersion = %q, want non-zero", rv)
	}
	if _, list := doReq(t, http.MethodGet, srv.URL+saPath, "", ""); !contains(listNames(t, list), "default") {
		t.Errorf("SA list missing default: %v", listNames(t, list))
	}
}
