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

	"k8s.io/apimachinery/pkg/runtime/schema"
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

// containerNamesImages decodes spec.containers into a name→image map.
func containerNamesImages(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var obj struct {
		Spec struct {
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("decode containers: %v\n%s", err, body)
	}
	m := map[string]string{}
	for _, c := range obj.Spec.Containers {
		m[c.Name] = c.Image
	}
	return m
}

// TestOverlay_StrategicMergePatch verifies that a strategic-merge patch of a
// built-in type merges a keyed list (containers, by name) element-wise instead
// of replacing it — the behavior a plain JSON merge patch cannot provide.
func TestOverlay_StrategicMergePatch(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	twoContainers := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-sm","namespace":"default"},` +
		`"spec":{"containers":[{"name":"app","image":"app:v1"},{"name":"sidecar","image":"sidecar:v1"}]}}`
	if code, body := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", twoContainers); code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", code, body)
	}

	// Strategic merge: update only the "app" container by its merge key.
	patch := `{"spec":{"containers":[{"name":"app","image":"app:v2"}]}}`
	code, got := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-sm", "application/strategic-merge-patch+json", patch)
	if code != 200 {
		t.Fatalf("strategic patch: status %d: %s", code, got)
	}
	ci := containerNamesImages(t, got)
	if ci["app"] != "app:v2" {
		t.Errorf("app image = %q, want app:v2", ci["app"])
	}
	if ci["sidecar"] != "sidecar:v1" {
		t.Errorf("sidecar container was dropped by strategic merge (got %v), want it preserved", ci)
	}
}

// TestOverlay_MergePatchReplacesList is the contrast to strategic merge: a plain
// JSON merge patch replaces a list wholesale rather than merging by key.
func TestOverlay_MergePatchReplacesList(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	twoContainers := `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-mp","namespace":"default"},` +
		`"spec":{"containers":[{"name":"app","image":"app:v1"},{"name":"sidecar","image":"sidecar:v1"}]}}`
	if code, body := doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", twoContainers); code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", code, body)
	}

	patch := `{"spec":{"containers":[{"name":"app","image":"app:v2"}]}}`
	code, got := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-mp", "application/merge-patch+json", patch)
	if code != 200 {
		t.Fatalf("merge patch: status %d: %s", code, got)
	}
	ci := containerNamesImages(t, got)
	if _, ok := ci["sidecar"]; ok {
		t.Errorf("merge patch should replace the containers list, but sidecar survived: %v", ci)
	}
	if ci["app"] != "app:v2" {
		t.Errorf("app image = %q, want app:v2", ci["app"])
	}
}

// TestOverlay_StrategicMergePatch_CapturedNoTypeMeta guards the case where the
// object being patched comes from a replayed LIST and so has no apiVersion/kind
// (the apiserver strips TypeMeta from list items). The GVK must still be resolved
// from the request path, or strategic merge would silently degrade to a JSON
// merge and drop the sibling container.
func TestOverlay_StrategicMergePatch_CapturedNoTypeMeta(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	// A captured list whose item carries no apiVersion/kind, as a real apiserver
	// returns list items.
	listBody := `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"1"},"items":[` +
		`{"metadata":{"name":"cap-pod","namespace":"default"},` +
		`"spec":{"containers":[{"name":"app","image":"app:v1"},{"name":"sidecar","image":"sidecar:v1"}]}}]}`
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{podsPath: {id: "s", at: from, body: listBody}}, nil)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, store, clock)

	patch := `{"spec":{"containers":[{"name":"app","image":"app:v2"}]}}`
	code, got := doReq(t, http.MethodPatch, srv.URL+podsPath+"/cap-pod", "application/strategic-merge-patch+json", patch)
	if code != 200 {
		t.Fatalf("strategic patch: status %d: %s", code, got)
	}
	ci := containerNamesImages(t, got)
	if ci["sidecar"] != "sidecar:v1" {
		t.Errorf("sidecar dropped — GVK was not resolved from the path for a TypeMeta-less object: %v", ci)
	}
	if ci["app"] != "app:v2" {
		t.Errorf("app image = %q, want app:v2", ci["app"])
	}
}

// TestKindForResource checks the resource→Kind resolver against the scheme,
// including cases a naive capitalization heuristic gets wrong (endpointslices →
// EndpointSlice, not Endpointslice), and that custom resources resolve to ok=false.
func TestKindForResource(t *testing.T) {
	cases := []struct {
		group, version, resource, wantKind string
		wantOK                             bool
	}{
		{"", "v1", "pods", "Pod", true},
		{"", "v1", "configmaps", "ConfigMap", true},
		{"apps", "v1", "deployments", "Deployment", true},
		{"discovery.k8s.io", "v1", "endpointslices", "EndpointSlice", true},
		{"networking.k8s.io", "v1", "networkpolicies", "NetworkPolicy", true},
		{"example.com", "v1", "widgets", "", false}, // custom resource: not in the scheme
	}
	for _, c := range cases {
		gvk, ok := kindForResource(schema.GroupVersion{Group: c.group, Version: c.version}, c.resource)
		if ok != c.wantOK || gvk.Kind != c.wantKind {
			t.Errorf("kindForResource(%s/%s, %s) = (%q, %v), want (%q, %v)",
				c.group, c.version, c.resource, gvk.Kind, ok, c.wantKind, c.wantOK)
		}
	}
}

func TestIsIPv6Address(t *testing.T) {
	cases := map[string]bool{
		"":               false,
		"None":           false, // headless Service
		"10.0.0.1":       false,
		"172.16.0.5":     false,
		"fd00::1234":     true,
		"::1":            true,
		"2001:db8::1":    true,
		"not-an-ip-addr": false,
	}
	for in, want := range cases {
		if got := isIPv6Address(in); got != want {
			t.Errorf("isIPv6Address(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestOverlay_APIDefaulting verifies that objects of Kinds real controllers
// reconcile get the same defaulting a live apiserver applies on create — a
// regression test for panics found wiring up --with-controller-manager:
// kube-controller-manager unconditionally dereferences fields like
// Deployment.spec.strategy.rollingUpdate.maxSurge and Job.spec.backoffLimit,
// assuming the apiserver already defaulted them.
func TestOverlay_APIDefaulting(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	t.Run("Deployment strategy", func(t *testing.T) {
		body := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"nginx","namespace":"default"},
			"spec":{"replicas":1,"selector":{"matchLabels":{"app":"nginx"}},
			"template":{"metadata":{"labels":{"app":"nginx"}},"spec":{"containers":[{"name":"nginx","image":"nginx"}]}}}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/apis/apps/v1/namespaces/default/deployments", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		var d struct {
			Spec struct {
				Strategy struct {
					Type          string `json:"type"`
					RollingUpdate *struct {
						MaxSurge       string `json:"maxSurge"`
						MaxUnavailable string `json:"maxUnavailable"`
					} `json:"rollingUpdate"`
				} `json:"strategy"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(out, &d); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if d.Spec.Strategy.Type != "RollingUpdate" {
			t.Errorf("strategy.type = %q, want RollingUpdate", d.Spec.Strategy.Type)
		}
		if d.Spec.Strategy.RollingUpdate == nil {
			t.Fatalf("strategy.rollingUpdate is nil — kube-controller-manager panics dereferencing .maxSurge")
		}
		if d.Spec.Strategy.RollingUpdate.MaxSurge != "25%" || d.Spec.Strategy.RollingUpdate.MaxUnavailable != "25%" {
			t.Errorf("rollingUpdate = %+v, want 25%%/25%%", d.Spec.Strategy.RollingUpdate)
		}
	})

	t.Run("Deployment/StatefulSet/ReplicaSet replicas default to 1 when omitted", func(t *testing.T) {
		// Some charts (e.g. Istio's istiod) omit `replicas` entirely, relying on
		// the apiserver's default of 1 — a nil Spec.Replicas panics
		// kube-controller-manager's NewRSNewReplicas (which dereferences it
		// unconditionally), so the Deployment never gets a ReplicaSet.
		cases := []struct {
			name, path, body string
		}{
			{"deployment", "/apis/apps/v1/namespaces/default/deployments", `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"no-replicas-d","namespace":"default"},
				"spec":{"selector":{"matchLabels":{"app":"x"}},
				"template":{"metadata":{"labels":{"app":"x"}},"spec":{"containers":[{"name":"x","image":"x"}]}}}}`},
			{"statefulset", "/apis/apps/v1/namespaces/default/statefulsets", `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"no-replicas-s","namespace":"default"},
				"spec":{"serviceName":"x","selector":{"matchLabels":{"app":"x"}},
				"template":{"metadata":{"labels":{"app":"x"}},"spec":{"containers":[{"name":"x","image":"x"}]}}}}`},
			{"replicaset", "/apis/apps/v1/namespaces/default/replicasets", `{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"no-replicas-r","namespace":"default"},
				"spec":{"selector":{"matchLabels":{"app":"x"}},
				"template":{"metadata":{"labels":{"app":"x"}},"spec":{"containers":[{"name":"x","image":"x"}]}}}}`},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				code, out := doReq(t, http.MethodPost, srv.URL+c.path, "application/json", c.body)
				if code != http.StatusCreated {
					t.Fatalf("create: status %d: %s", code, out)
				}
				var d struct {
					Spec struct {
						Replicas *int32 `json:"replicas"`
					} `json:"spec"`
				}
				if err := json.Unmarshal(out, &d); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if d.Spec.Replicas == nil || *d.Spec.Replicas != 1 {
					t.Errorf("replicas = %v, want 1", d.Spec.Replicas)
				}
			})
		}
	})

	t.Run("StatefulSet/DaemonSet revisionHistoryLimit defaults to 10 when omitted", func(t *testing.T) {
		// Unlike Deployment's cleanup (which nil-checks via HasRevisionHistoryLimit
		// before dereferencing), the StatefulSet and DaemonSet controllers'
		// history-truncation code (truncateHistory / cleanupHistory) dereference
		// *set.Spec.RevisionHistoryLimit unconditionally — a nil value crashes
		// the whole controller-manager process (client-go's crash handler logs
		// it, then repanics), not just that one object's reconcile. Found by
		// actually running the upstream conformance suite against
		// --with-controller-manager.
		cases := []struct {
			name, path, body string
		}{
			{"statefulset", "/apis/apps/v1/namespaces/default/statefulsets", `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"no-rhl-s","namespace":"default"},
				"spec":{"serviceName":"x","selector":{"matchLabels":{"app":"x"}},
				"template":{"metadata":{"labels":{"app":"x"}},"spec":{"containers":[{"name":"x","image":"x"}]}}}}`},
			{"daemonset", "/apis/apps/v1/namespaces/default/daemonsets", `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"no-rhl-d","namespace":"default"},
				"spec":{"selector":{"matchLabels":{"app":"x"}},
				"template":{"metadata":{"labels":{"app":"x"}},"spec":{"containers":[{"name":"x","image":"x"}]}}}}`},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				code, out := doReq(t, http.MethodPost, srv.URL+c.path, "application/json", c.body)
				if code != http.StatusCreated {
					t.Fatalf("create: status %d: %s", code, out)
				}
				var d struct {
					Spec struct {
						RevisionHistoryLimit *int32 `json:"revisionHistoryLimit"`
					} `json:"spec"`
				}
				if err := json.Unmarshal(out, &d); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if d.Spec.RevisionHistoryLimit == nil || *d.Spec.RevisionHistoryLimit != 10 {
					t.Errorf("revisionHistoryLimit = %v, want 10 — a nil value crashes the whole controller-manager process", d.Spec.RevisionHistoryLimit)
				}
			})
		}
	})

	t.Run("Job backoffLimit and friends", func(t *testing.T) {
		body := `{"apiVersion":"batch/v1","kind":"Job","metadata":{"name":"hello","namespace":"default"},
			"spec":{"template":{"spec":{"containers":[{"name":"hello","image":"busybox"}],"restartPolicy":"Never"}}}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/apis/batch/v1/namespaces/default/jobs", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		var j struct {
			Spec struct {
				Parallelism    *int32  `json:"parallelism"`
				BackoffLimit   *int32  `json:"backoffLimit"`
				CompletionMode *string `json:"completionMode"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(out, &j); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if j.Spec.BackoffLimit == nil || *j.Spec.BackoffLimit != 6 {
			t.Errorf("backoffLimit = %v, want 6 — job controller panics dereferencing a nil backoffLimit", j.Spec.BackoffLimit)
		}
		if j.Spec.Parallelism == nil || *j.Spec.Parallelism != 1 {
			t.Errorf("parallelism = %v, want 1", j.Spec.Parallelism)
		}
		if j.Spec.CompletionMode == nil || *j.Spec.CompletionMode != "NonIndexed" {
			t.Errorf("completionMode = %v, want NonIndexed", j.Spec.CompletionMode)
		}
	})

	t.Run("DaemonSet updateStrategy", func(t *testing.T) {
		body := `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"fluentd","namespace":"default"},
			"spec":{"selector":{"matchLabels":{"app":"fluentd"}},
			"template":{"metadata":{"labels":{"app":"fluentd"}},"spec":{"containers":[{"name":"fluentd","image":"fluentd"}]}}}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/apis/apps/v1/namespaces/default/daemonsets", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		var d struct {
			Spec struct {
				UpdateStrategy struct {
					Type          string `json:"type"`
					RollingUpdate *struct {
						MaxUnavailable string `json:"maxUnavailable"`
					} `json:"rollingUpdate"`
				} `json:"updateStrategy"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(out, &d); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if d.Spec.UpdateStrategy.Type != "RollingUpdate" {
			t.Errorf("updateStrategy.type = %q, want RollingUpdate", d.Spec.UpdateStrategy.Type)
		}
		if d.Spec.UpdateStrategy.RollingUpdate == nil || d.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable != "25%" {
			t.Errorf("updateStrategy.rollingUpdate = %+v, want non-nil with maxUnavailable 25%%", d.Spec.UpdateStrategy.RollingUpdate)
		}
	})

	t.Run("StatefulSet updateStrategy and podManagementPolicy", func(t *testing.T) {
		body := `{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"name":"web","namespace":"default"},
			"spec":{"serviceName":"web","selector":{"matchLabels":{"app":"web"}},
			"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"web","image":"nginx"}]}}}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/apis/apps/v1/namespaces/default/statefulsets", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		var s struct {
			Spec struct {
				PodManagementPolicy string `json:"podManagementPolicy"`
				UpdateStrategy      struct {
					Type          string `json:"type"`
					RollingUpdate *struct {
						Partition *int32 `json:"partition"`
					} `json:"rollingUpdate"`
				} `json:"updateStrategy"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(out, &s); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if s.Spec.PodManagementPolicy != "OrderedReady" {
			t.Errorf("podManagementPolicy = %q, want OrderedReady", s.Spec.PodManagementPolicy)
		}
		if s.Spec.UpdateStrategy.Type != "RollingUpdate" {
			t.Errorf("updateStrategy.type = %q, want RollingUpdate", s.Spec.UpdateStrategy.Type)
		}
		if s.Spec.UpdateStrategy.RollingUpdate == nil || s.Spec.UpdateStrategy.RollingUpdate.Partition == nil {
			t.Fatalf("updateStrategy.rollingUpdate.partition is nil — statefulset controller panics dereferencing it")
		}
		if *s.Spec.UpdateStrategy.RollingUpdate.Partition != 0 {
			t.Errorf("partition = %d, want 0", *s.Spec.UpdateStrategy.RollingUpdate.Partition)
		}
	})

	t.Run("CronJob concurrencyPolicy and history limits", func(t *testing.T) {
		body := `{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"hello-cron","namespace":"default"},
			"spec":{"schedule":"* * * * *","jobTemplate":{"spec":{"template":{"spec":{
			"containers":[{"name":"hello","image":"busybox"}],"restartPolicy":"OnFailure"}}}}}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/apis/batch/v1/namespaces/default/cronjobs", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		var c struct {
			Spec struct {
				ConcurrencyPolicy          string `json:"concurrencyPolicy"`
				Suspend                    *bool  `json:"suspend"`
				SuccessfulJobsHistoryLimit *int32 `json:"successfulJobsHistoryLimit"`
				FailedJobsHistoryLimit     *int32 `json:"failedJobsHistoryLimit"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(out, &c); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if c.Spec.ConcurrencyPolicy != "Allow" {
			t.Errorf("concurrencyPolicy = %q, want Allow", c.Spec.ConcurrencyPolicy)
		}
		if c.Spec.Suspend == nil || *c.Spec.Suspend != false {
			t.Errorf("suspend = %v, want false", c.Spec.Suspend)
		}
		if c.Spec.SuccessfulJobsHistoryLimit == nil || *c.Spec.SuccessfulJobsHistoryLimit != 3 {
			t.Errorf("successfulJobsHistoryLimit = %v, want 3", c.Spec.SuccessfulJobsHistoryLimit)
		}
		if c.Spec.FailedJobsHistoryLimit == nil || *c.Spec.FailedJobsHistoryLimit != 1 {
			t.Errorf("failedJobsHistoryLimit = %v, want 1", c.Spec.FailedJobsHistoryLimit)
		}
	})

	t.Run("Service IPFamilies", func(t *testing.T) {
		body := `{"apiVersion":"v1","kind":"Service","metadata":{"name":"web","namespace":"default"},
			"spec":{"selector":{"app":"nginx"},"ports":[{"port":80}]}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces/default/services", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		var s struct {
			Spec struct {
				Type       string   `json:"type"`
				IPFamilies []string `json:"ipFamilies"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(out, &s); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if s.Spec.Type != "ClusterIP" {
			t.Errorf("type = %q, want ClusterIP", s.Spec.Type)
		}
		if len(s.Spec.IPFamilies) == 0 {
			t.Fatalf("ipFamilies is empty — the endpoint controller panics indexing ipFamilies[0]")
		}
		if s.Spec.IPFamilies[0] != "IPv4" {
			t.Errorf("ipFamilies = %v, want [IPv4] as the fallback when nothing indicates IPv6", s.Spec.IPFamilies)
		}
	})

	t.Run("Service IPFamilies infers IPv6 from an explicit ClusterIP", func(t *testing.T) {
		// Defaulting to IPv4 unconditionally would pair an IPv6 ClusterIP with
		// an IPv4 family — an inconsistent Service that sends the endpoint
		// controller looking for the wrong address family.
		body := `{"apiVersion":"v1","kind":"Service","metadata":{"name":"web6","namespace":"default"},
			"spec":{"clusterIP":"fd00::1234","selector":{"app":"nginx"},"ports":[{"port":80}]}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces/default/services", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		var s struct {
			Spec struct {
				IPFamilies []string `json:"ipFamilies"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(out, &s); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(s.Spec.IPFamilies) == 0 || s.Spec.IPFamilies[0] != "IPv6" {
			t.Errorf("ipFamilies = %v, want [IPv6] inferred from clusterIP fd00::1234", s.Spec.IPFamilies)
		}
	})

	t.Run("dual-stack Service keeps IPv4 primary despite an IPv6 secondary in clusterIPs", func(t *testing.T) {
		// Scanning every clusterIPs entry (instead of just the primary address)
		// would pick IPv6 here and produce ipFamilies[0] inconsistent with the
		// primary clusterIP, sending the endpoint controller down the wrong
		// address family.
		body := `{"apiVersion":"v1","kind":"Service","metadata":{"name":"web-dual","namespace":"default"},
			"spec":{"clusterIP":"10.0.0.5","clusterIPs":["10.0.0.5","fd00::5678"],"selector":{"app":"nginx"},"ports":[{"port":80}]}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces/default/services", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		var s struct {
			Spec struct {
				IPFamilies []string `json:"ipFamilies"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(out, &s); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(s.Spec.IPFamilies) == 0 || s.Spec.IPFamilies[0] != "IPv4" {
			t.Errorf("ipFamilies = %v, want [IPv4] matching the primary clusterIP 10.0.0.5", s.Spec.IPFamilies)
		}
	})

	t.Run("LoadBalancer Service gets a synthesized external IP", func(t *testing.T) {
		// No cloud-controller-manager runs against the overlay (see
		// cmd/controllermanager.go), so nothing else would ever populate
		// status.loadBalancer.ingress — `helm install --wait` (and any client
		// polling for a LoadBalancer Service to become ready) hangs without this.
		body := `{"apiVersion":"v1","kind":"Service","metadata":{"name":"gw","namespace":"default"},
			"spec":{"type":"LoadBalancer","selector":{"app":"gw"},"ports":[{"port":80}]}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces/default/services", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		var s struct {
			Status struct {
				LoadBalancer struct {
					Ingress []struct {
						IP string `json:"ip"`
					} `json:"ingress"`
				} `json:"loadBalancer"`
			} `json:"status"`
		}
		if err := json.Unmarshal(out, &s); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(s.Status.LoadBalancer.Ingress) == 0 || s.Status.LoadBalancer.Ingress[0].IP == "" {
			t.Fatalf("loadBalancer.ingress is empty — helm install --wait hangs forever on this Service")
		}
		gotIP := s.Status.LoadBalancer.Ingress[0].IP

		// A ClusterIP Service (the common case) must not get a bogus LoadBalancer
		// ingress synthesized.
		cbody := `{"apiVersion":"v1","kind":"Service","metadata":{"name":"web-clusterip","namespace":"default"},
			"spec":{"selector":{"app":"nginx"},"ports":[{"port":80}]}}`
		_, cout := doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces/default/services", "application/json", cbody)
		if strings.Contains(string(cout), "loadBalancer") {
			t.Errorf("ClusterIP Service got a loadBalancer status: %s", cout)
		}

		// A subsequent write to the same Service (e.g. a label patch) must keep
		// the same address rather than reassigning a new one.
		_, patched := doReq(t, http.MethodPatch, srv.URL+"/api/v1/namespaces/default/services/gw",
			"application/merge-patch+json", `{"metadata":{"labels":{"x":"y"}}}`)
		var s2 struct {
			Status struct {
				LoadBalancer struct {
					Ingress []struct {
						IP string `json:"ip"`
					} `json:"ingress"`
				} `json:"loadBalancer"`
			} `json:"status"`
		}
		if err := json.Unmarshal(patched, &s2); err != nil {
			t.Fatalf("decode patched: %v\n%s", err, patched)
		}
		if len(s2.Status.LoadBalancer.Ingress) == 0 || s2.Status.LoadBalancer.Ingress[0].IP != gotIP {
			t.Errorf("loadBalancer ingress changed across writes: got %v, want stable %q", s2.Status.LoadBalancer.Ingress, gotIP)
		}
	})

	t.Run("Service gets a synthesized ClusterIP", func(t *testing.T) {
		// kstatus's readiness check (which Helm v4's `--wait` uses) requires
		// spec.clusterIP to be non-empty for a LoadBalancer Service — NOT
		// status.loadBalancer.ingress — so a missing ClusterIP hangs
		// `helm install --wait` forever even with an external address assigned.
		body := `{"apiVersion":"v1","kind":"Service","metadata":{"name":"gw-cip","namespace":"default"},
			"spec":{"type":"LoadBalancer","selector":{"app":"gw"},"ports":[{"port":80}]}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces/default/services", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		var s struct {
			Spec struct {
				ClusterIP  string   `json:"clusterIP"`
				ClusterIPs []string `json:"clusterIPs"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(out, &s); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if s.Spec.ClusterIP == "" {
			t.Fatalf("clusterIP is empty — kstatus reports this LoadBalancer Service InProgress forever")
		}
		if len(s.Spec.ClusterIPs) == 0 || s.Spec.ClusterIPs[0] != s.Spec.ClusterIP {
			t.Errorf("clusterIPs = %v, want [%q]", s.Spec.ClusterIPs, s.Spec.ClusterIP)
		}
		gotIP := s.Spec.ClusterIP

		// A headless Service (explicit clusterIP: None) is left alone.
		hbody := `{"apiVersion":"v1","kind":"Service","metadata":{"name":"headless","namespace":"default"},
			"spec":{"clusterIP":"None","selector":{"app":"x"},"ports":[{"port":80}]}}`
		_, hout := doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces/default/services", "application/json", hbody)
		if got := metaString(hout, "name"); got != "headless" {
			t.Fatalf("create headless: %s", hout)
		}
		var h struct {
			Spec struct {
				ClusterIP string `json:"clusterIP"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(hout, &h); err != nil {
			t.Fatalf("decode headless: %v", err)
		}
		if h.Spec.ClusterIP != "None" {
			t.Errorf("headless Service clusterIP = %q, want None left untouched", h.Spec.ClusterIP)
		}

		// A subsequent write keeps the same ClusterIP rather than reassigning one.
		_, patched := doReq(t, http.MethodPatch, srv.URL+"/api/v1/namespaces/default/services/gw-cip",
			"application/merge-patch+json", `{"metadata":{"labels":{"x":"y"}}}`)
		if !strings.Contains(string(patched), `"clusterIP":"`+gotIP+`"`) {
			t.Errorf("clusterIP changed across writes: %s, want stable %q", patched, gotIP)
		}
	})

	t.Run("unknown resource passes through unchanged", func(t *testing.T) {
		body := `{"apiVersion":"example.com/v1","kind":"Widget","metadata":{"name":"w1","namespace":"default"},"spec":{}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/apis/example.com/v1/namespaces/default/widgets", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		if metaString(out, "name") != "w1" {
			t.Errorf("name = %q, want w1 (custom resource create should be unaffected)", metaString(out, "name"))
		}
	})

	t.Run("defaulting preserves fields unknown to the vendored types", func(t *testing.T) {
		// defaultObject round-trips through a typed struct to compute what
		// defaulting changed, but must apply that as a merge patch onto the
		// original body rather than returning the re-marshaled typed struct
		// directly — otherwise a field the vendored k8s.io/api types don't
		// know about (e.g. a newer API field a live cluster's capture might
		// have, or here a made-up one standing in for that case) would be
		// silently dropped even though the client explicitly sent it.
		body := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"nginx-unknown","namespace":"default"},
			"spec":{"replicas":1,"selector":{"matchLabels":{"app":"nginx"}},
			"template":{"metadata":{"labels":{"app":"nginx"}},"spec":{"containers":[{"name":"nginx","image":"nginx"}]}},
			"unknownFutureField":"should-survive"}}`
		code, out := doReq(t, http.MethodPost, srv.URL+"/apis/apps/v1/namespaces/default/deployments", "application/json", body)
		if code != http.StatusCreated {
			t.Fatalf("create: status %d: %s", code, out)
		}
		var d struct {
			Spec struct {
				Strategy struct {
					Type string `json:"type"`
				} `json:"strategy"`
				UnknownFutureField string `json:"unknownFutureField"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(out, &d); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if d.Spec.UnknownFutureField != "should-survive" {
			t.Errorf("unknownFutureField = %q, want it preserved through defaulting", d.Spec.UnknownFutureField)
		}
		if d.Spec.Strategy.Type != "RollingUpdate" {
			t.Errorf("strategy.type = %q, want defaulting to still apply alongside the unknown field", d.Spec.Strategy.Type)
		}
	})
}

// TestOverlay_PatchAppliesDefaulting verifies that PATCHing a captured object
// that predates this project's defaulting (e.g. an older/incomplete capture,
// or a hand-authored fixture) — not just POST/PUT — backfills the same
// defaults, since the patched result is just as capable of reaching a real
// controller as a freshly created object.
func TestOverlay_PatchAppliesDefaulting(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)

	listPath := "/apis/apps/v1/namespaces/default/deployments"
	deployPath := listPath + "/nginx"
	deployItem := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"nginx","namespace":"default"},
		"spec":{"replicas":1,"selector":{"matchLabels":{"app":"nginx"}},"strategy":{},
		"template":{"metadata":{"labels":{"app":"nginx"}},"spec":{"containers":[{"name":"nginx","image":"nginx"}]}}}}`
	capturedList := `{"apiVersion":"apps/v1","kind":"DeploymentList","metadata":{},"items":[` + deployItem + `]}`
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{listPath: {id: "d", at: from, body: capturedList}}, nil)
	srv := newWritableServer(t, store, clock)

	// Confirm the captured object really does carry an empty strategy (i.e.
	// this predates defaulting, not already fixed by something else).
	code, before := doReq(t, http.MethodGet, srv.URL+deployPath, "", "")
	if code != 200 {
		t.Fatalf("GET before patch: status %d: %s", code, before)
	}

	patch := `{"spec":{"replicas":2}}`
	code, after := doReq(t, http.MethodPatch, srv.URL+deployPath, "application/merge-patch+json", patch)
	if code != 200 {
		t.Fatalf("PATCH: status %d: %s", code, after)
	}
	var d struct {
		Spec struct {
			Replicas int `json:"replicas"`
			Strategy struct {
				Type          string `json:"type"`
				RollingUpdate *struct {
					MaxSurge string `json:"maxSurge"`
				} `json:"rollingUpdate"`
			} `json:"strategy"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(after, &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Spec.Replicas != 2 {
		t.Errorf("replicas = %d, want 2 (the patch itself)", d.Spec.Replicas)
	}
	if d.Spec.Strategy.Type != "RollingUpdate" || d.Spec.Strategy.RollingUpdate == nil {
		t.Errorf("strategy = %+v, want defaulted to RollingUpdate/25%% — PATCH must default just like POST/PUT", d.Spec.Strategy)
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

// TestOverlay_StatusPUTSetsAnnotation reproduces the exact write pattern the
// real deployment controller uses to stamp `deployment.kubernetes.io/revision`
// onto a Deployment: client-go's UpdateStatus PUTs the *entire* object
// (metadata included) to .../status. Discovered by actually running the
// upstream conformance suite against --with-controller-manager: every
// Deployment reconciled that way never got its revision annotation, because
// an earlier version of the overlay's status-subresource handling protected
// all of metadata, not just .spec.
func TestOverlay_StatusPUTSetsAnnotation(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	deployPath := "/apis/apps/v1/namespaces/default/deployments"
	depBody := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"dep-s"},
		"spec":{"replicas":1,"selector":{"matchLabels":{"app":"x"}},
		"template":{"metadata":{"labels":{"app":"x"}},"spec":{"containers":[{"name":"x","image":"x"}]}}}}`
	code, created := doReq(t, http.MethodPost, srv.URL+deployPath, "application/json", depBody)
	if code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", code, created)
	}

	// Simulate UpdateStatus: PUT the whole object (with a new annotation and a
	// spec change that must NOT take effect) to .../status.
	var full map[string]any
	if err := json.Unmarshal(created, &full); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	meta := full["metadata"].(map[string]any)
	meta["annotations"] = map[string]any{"deployment.kubernetes.io/revision": "1"}
	full["spec"].(map[string]any)["replicas"] = float64(99) // must be dropped
	full["status"] = map[string]any{"observedGeneration": float64(1), "replicas": float64(1)}
	putBody, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	code, got := doReq(t, http.MethodPut, srv.URL+deployPath+"/dep-s/status", "application/json", string(putBody))
	if code != 200 {
		t.Fatalf("PUT status: %d: %s", code, got)
	}
	if !strings.Contains(string(got), `"deployment.kubernetes.io/revision":"1"`) {
		t.Errorf("annotation set via status PUT did not persist: %s", got)
	}
	if strings.Contains(string(got), `"replicas":99`) {
		t.Errorf("status PUT leaked a spec change: %s", got)
	}
	if !strings.Contains(string(got), `"observedGeneration":1`) {
		t.Errorf("status change did not apply: %s", got)
	}
}

// TestOverlay_WriteResponseStampsKind verifies a write response always
// carries apiVersion/kind, even when the client's request body omitted them —
// exactly what client-go's typed Update/UpdateStatus calls do (they
// round-trip an object fetched via Get/List, whose TypeMeta the apiserver
// strips on read, a well-known client-go quirk). Reads already stamped this;
// writes didn't, so a typed client-go decoder failed the *response* to its
// own UpdateStatus call with "Object 'Kind' is missing" — found via the
// upstream conformance suite's "Deployment should run the lifecycle of a
// Deployment" spec, which does exactly this after creating a Deployment.
func TestOverlay_WriteResponseStampsKind(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	deployPath := "/apis/apps/v1/namespaces/default/deployments"
	// No apiVersion/kind at all — mirrors a client-go object fetched via
	// Get/List, whose TypeMeta the apiserver strips.
	noKindBody := `{"metadata":{"name":"d-nokind"},
		"spec":{"replicas":1,"selector":{"matchLabels":{"app":"x"}},
		"template":{"metadata":{"labels":{"app":"x"}},"spec":{"containers":[{"name":"x","image":"x"}]}}}}`
	code, created := doReq(t, http.MethodPost, srv.URL+deployPath, "application/json", noKindBody)
	if code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", code, created)
	}
	if !strings.Contains(string(created), `"apiVersion":"apps/v1"`) || !strings.Contains(string(created), `"kind":"Deployment"`) {
		t.Errorf("create response missing apiVersion/kind: %s", created)
	}

	// PUT .../status with a body that (like a real UpdateStatus call) also
	// carries no apiVersion/kind.
	putBody := `{"metadata":{"name":"d-nokind","namespace":"default"},"status":{"readyReplicas":1}}`
	code, put := doReq(t, http.MethodPut, srv.URL+deployPath+"/d-nokind/status", "application/json", putBody)
	if code != 200 {
		t.Fatalf("PUT status: status %d: %s", code, put)
	}
	if !strings.Contains(string(put), `"apiVersion":"apps/v1"`) || !strings.Contains(string(put), `"kind":"Deployment"`) {
		t.Errorf("PUT status response missing apiVersion/kind: %s", put)
	}

	// PATCH .../status likewise.
	code, patched := doReq(t, http.MethodPatch, srv.URL+deployPath+"/d-nokind/status",
		"application/merge-patch+json", `{"status":{"readyReplicas":2}}`)
	if code != 200 {
		t.Fatalf("PATCH status: status %d: %s", code, patched)
	}
	if !strings.Contains(string(patched), `"apiVersion":"apps/v1"`) || !strings.Contains(string(patched), `"kind":"Deployment"`) {
		t.Errorf("PATCH status response missing apiVersion/kind: %s", patched)
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

	// "scale" itself isn't a good example here: it's a real subresource, just
	// inapplicable to Pods (see TestOverlay_Scale_UnsupportedResourceRejected
	// for that 404 case). Use a name that isn't a subresource on anything.
	if code, _ := doReq(t, http.MethodPut, srv.URL+podsPath+"/pod-x/frobnicate", "application/json", `{"x":1}`); code != http.StatusMethodNotAllowed {
		t.Errorf("PUT unknown subresource: status %d, want 405", code)
	}
	if code, _ := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-x/frobnicate", "application/merge-patch+json", `{"x":1}`); code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH unknown subresource: status %d, want 405", code)
	}
}

// TestOverlay_Scale_UnsupportedResourceRejected verifies GET/PUT .../scale
// 404s for a resource kind with no real scale subresource (Pods), matching a
// real apiserver's response for an unregistered route — unlike Deployments/
// ReplicaSets/StatefulSets/ReplicationControllers, which do have one.
func TestOverlay_Scale_UnsupportedResourceRejected(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)
	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-x"))

	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-x/scale", "", ""); code != http.StatusNotFound {
		t.Errorf("GET pod scale: status %d, want 404", code)
	}
	if code, _ := doReq(t, http.MethodPut, srv.URL+podsPath+"/pod-x/scale", "application/json",
		`{"apiVersion":"autoscaling/v1","kind":"Scale","spec":{"replicas":2}}`); code != http.StatusNotFound {
		t.Errorf("PUT pod scale: status %d, want 404", code)
	}
}

// TestOverlay_Scale_GetPutPatch covers the full /scale lifecycle for a
// Deployment: GET reflects current spec/status, PUT sets a new replica count
// (and nothing else, even if the client tries to sneak in other spec/status
// fields), and a subsequent merge-patch and JSON-patch each update replicas
// too. Discovered via actually running the upstream conformance suite's
// "Deployment should have a working scale subresource" spec against
// --with-controller-manager, which uses exactly this GET→PUT round trip
// (client-go's ScalesGetter).
func TestOverlay_Scale_GetPutPatch(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	deployPath := "/apis/apps/v1/namespaces/default/deployments"
	body := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"scale-d"},
		"spec":{"replicas":2,"selector":{"matchLabels":{"app":"x"}},
		"template":{"metadata":{"labels":{"app":"x"}},"spec":{"containers":[{"name":"x","image":"x"}]}}}}`
	if code, out := doReq(t, http.MethodPost, srv.URL+deployPath, "application/json", body); code != http.StatusCreated {
		t.Fatalf("create: status %d: %s", code, out)
	}

	// GET reflects the created object's spec.replicas and derived selector.
	code, got := doReq(t, http.MethodGet, srv.URL+deployPath+"/scale-d/scale", "", "")
	if code != 200 {
		t.Fatalf("GET scale: status %d: %s", code, got)
	}
	var s struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Spec       struct {
			Replicas int32 `json:"replicas"`
		} `json:"spec"`
		Status struct {
			Selector string `json:"selector"`
		} `json:"status"`
	}
	if err := json.Unmarshal(got, &s); err != nil {
		t.Fatalf("decode: %v\n%s", err, got)
	}
	if s.APIVersion != "autoscaling/v1" || s.Kind != "Scale" {
		t.Errorf("apiVersion/kind = %s/%s, want autoscaling/v1/Scale", s.APIVersion, s.Kind)
	}
	if s.Spec.Replicas != 2 {
		t.Errorf("GET scale replicas = %d, want 2", s.Spec.Replicas)
	}
	if s.Status.Selector != "app=x" {
		t.Errorf("GET scale status.selector = %q, want app=x", s.Status.Selector)
	}

	// PUT a higher replica count, and try to sneak in an unrelated spec field —
	// only replicas may change on the underlying Deployment.
	putBody := `{"apiVersion":"autoscaling/v1","kind":"Scale","metadata":{"name":"scale-d","namespace":"default"},"spec":{"replicas":5}}`
	code, put := doReq(t, http.MethodPut, srv.URL+deployPath+"/scale-d/scale", "application/json", putBody)
	if code != 200 {
		t.Fatalf("PUT scale: status %d: %s", code, put)
	}
	if !strings.Contains(string(put), `"replicas":5`) {
		t.Errorf("PUT scale response = %s, want replicas 5", put)
	}
	_, dep := doReq(t, http.MethodGet, srv.URL+deployPath+"/scale-d", "", "")
	if !strings.Contains(string(dep), `"replicas":5`) {
		t.Errorf("underlying Deployment not scaled: %s", dep)
	}
	if metaInt(dep, "generation") != 2 {
		t.Errorf("scaling did not bump generation: %s", dep)
	}

	// A merge-patch to scale also updates replicas.
	code, patched := doReq(t, http.MethodPatch, srv.URL+deployPath+"/scale-d/scale",
		"application/merge-patch+json", `{"spec":{"replicas":7}}`)
	if code != 200 || !strings.Contains(string(patched), `"replicas":7`) {
		t.Fatalf("merge-patch scale: status %d: %s", code, patched)
	}

	// A JSON patch (RFC 6902) works too.
	code, jpatched := doReq(t, http.MethodPatch, srv.URL+deployPath+"/scale-d/scale",
		"application/json-patch+json", `[{"op":"replace","path":"/spec/replicas","value":3}]`)
	if code != 200 || !strings.Contains(string(jpatched), `"replicas":3`) {
		t.Fatalf("json-patch scale: status %d: %s", code, jpatched)
	}
	_, dep2 := doReq(t, http.MethodGet, srv.URL+deployPath+"/scale-d", "", "")
	if !strings.Contains(string(dep2), `"replicas":3`) {
		t.Errorf("underlying Deployment not scaled by json-patch: %s", dep2)
	}
}

// TestOverlay_Scale_ReadOnlyGet verifies GET .../scale works against a plain
// captured Deployment even without --writable (unlike scale writes, which
// need the overlay).
func TestOverlay_Scale_ReadOnlyGet(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	depList := `{"apiVersion":"apps/v1","kind":"DeploymentList","metadata":{},"items":[
		{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"default"},
		 "spec":{"replicas":4,"selector":{"matchLabels":{"app":"web"}}},
		 "status":{"replicas":4}}]}`
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{"/apis/apps/v1/namespaces/default/deployments": {id: "s", at: from, body: depList}},
		nil)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	h := newHandler(store, time.Time{}, false)
	h.clock = clock
	srv := httptest.NewServer(h)
	defer srv.Close()

	code, got := doReq(t, http.MethodGet, srv.URL+"/apis/apps/v1/namespaces/default/deployments/web/scale", "", "")
	if code != 200 {
		t.Fatalf("GET scale (read-only): status %d: %s", code, got)
	}
	if !strings.Contains(string(got), `"replicas":4`) {
		t.Errorf("read-only scale = %s, want replicas 4", got)
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

// TestOverlay_StatusPatchIsolated verifies a PATCH to .../status protects
// .spec (reset back to the current object) and does not bump generation
// (which tracks spec changes), while a spec PATCH does bump it. Metadata
// (annotations, labels) is NOT protected — matching the real apiserver's
// per-type status strategies (e.g. pkg/registry/core/pod/strategy.go's
// podStatusStrategy.PrepareForUpdate only resets Spec/DeletionTimestamp/
// OwnerReferences, not labels or annotations). This matters in practice: the
// deployment controller sets `deployment.kubernetes.io/revision` on the
// Deployment itself via UpdateStatus (a full-object PUT to .../status), not a
// spec/metadata write.
func TestOverlay_StatusPatchIsolated(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-s")) // generation 1

	// Status patch also sets a label and tries to sneak in a spec change — the
	// label must pass through (matching a real apiserver); the spec change must
	// be dropped (reset back to current).
	doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-s/status", "application/merge-patch+json",
		`{"metadata":{"labels":{"team":"a"}},"spec":{"nodeName":"sneaky"},"status":{"phase":"Running"}}`)
	_, got := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-s", "", "")
	if !strings.Contains(string(got), `"phase":"Running"`) {
		t.Errorf("status not applied: %s", got)
	}
	if !strings.Contains(string(got), `"team":"a"`) {
		t.Errorf("status patch dropped a metadata change (should pass through): %s", got)
	}
	if strings.Contains(string(got), `"nodeName":"sneaky"`) {
		t.Errorf("status patch leaked a spec change: %s", got)
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

// TestOverlay_ApplyPatchCreatesMissingObject verifies a Server-Side Apply
// PATCH (Content-Type: application/apply-patch+yaml) targeting an object that
// doesn't exist yet creates it — matching the real apiserver's
// create-or-update SSA semantics — rather than 404ing. This is the codepath
// `helm install --create-namespace`, `kubectl apply --server-side`, and
// CRD/webhook installers rely on to provision brand-new objects.
func TestOverlay_ApplyPatchCreatesMissingObject(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// Namespaced resource, mirroring a Helm chart's first apply of a new object.
	yamlBody := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: pod-ssa\n  namespace: default\n  labels:\n    applied: \"yes\"\n"
	code, got := doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-ssa", "application/apply-patch+yaml", yamlBody)
	if code != http.StatusCreated {
		t.Fatalf("apply-patch create: status %d, want 201: %s", code, got)
	}
	if !strings.Contains(string(got), `"applied":"yes"`) {
		t.Errorf("created object missing applied label: %s", got)
	}
	if rv := bodyRV(t, got); rv == "" || rv == "0" {
		t.Errorf("created object rv = %q, want non-zero", rv)
	}
	if metaString(got, "uid") == "" {
		t.Errorf("created object missing uid: %s", got)
	}
	code, get := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-ssa", "", "")
	if code != 200 || metaString(get, "name") != "pod-ssa" {
		t.Fatalf("GET pod-ssa: status %d name %q", code, metaString(get, "name"))
	}

	// Cluster-scoped resource — mirrors `helm install --create-namespace`.
	nsYAML := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ns-ssa\n"
	code, nsGot := doReq(t, http.MethodPatch, srv.URL+"/api/v1/namespaces/ns-ssa", "application/apply-patch+yaml", nsYAML)
	if code != http.StatusCreated {
		t.Fatalf("apply-patch create namespace: status %d, want 201: %s", code, nsGot)
	}
	// Namespace-create side effects (default SA, kube-root-ca.crt) still fire.
	code, sa := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/ns-ssa/serviceaccounts/default", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET default SA after apply-create: status %d, want 200\n%s", code, sa)
	}

	// A status-subresource apply cannot create its (missing) parent object.
	code, _ = doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-nope/status", "application/apply-patch+yaml",
		"apiVersion: v1\nkind: Pod\nstatus:\n  phase: Running\n")
	if code != http.StatusNotFound {
		t.Errorf("status apply-create on missing object: status %d, want 404", code)
	}

	// A body identity mismatched with the request path is still rejected on create.
	code, _ = doReq(t, http.MethodPatch, srv.URL+podsPath+"/pod-mismatch", "application/apply-patch+yaml",
		"apiVersion: v1\nkind: Pod\nmetadata:\n  name: other-name\n  namespace: default\n")
	if code != http.StatusBadRequest {
		t.Errorf("mismatched name on apply-create: status %d, want 400", code)
	}
}

// TestOverlay_ClusterScopedSingleGet covers #149: an overlay-created
// cluster-scoped object must be returned by a single-object GET (not just LIST),
// for core (namespaces, nodes) and grouped (clusterroles) resources.
func TestOverlay_ClusterScopedSingleGet(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	cases := []struct{ createPath, getPath, body, name string }{
		{"/api/v1/namespaces", "/api/v1/namespaces/ov-ns",
			`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"ov-ns"}}`, "ov-ns"},
		{"/api/v1/nodes", "/api/v1/nodes/ov-node",
			`{"apiVersion":"v1","kind":"Node","metadata":{"name":"ov-node"}}`, "ov-node"},
		{"/apis/rbac.authorization.k8s.io/v1/clusterroles", "/apis/rbac.authorization.k8s.io/v1/clusterroles/ov-cr",
			`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRole","metadata":{"name":"ov-cr"}}`, "ov-cr"},
	}
	for _, c := range cases {
		if code, b := doReq(t, http.MethodPost, srv.URL+c.createPath, "application/json", c.body); code != http.StatusCreated {
			t.Fatalf("create %s: status %d: %s", c.name, code, b)
		}
		code, got := doReq(t, http.MethodGet, srv.URL+c.getPath, "", "")
		if code != 200 {
			t.Errorf("GET %s: status %d, want 200", c.getPath, code)
			continue
		}
		if n := metaString(got, "name"); n != c.name {
			t.Errorf("GET %s: name %q, want %q", c.getPath, n, c.name)
		}
	}
}

// TestOverlay_ClusterScopedDeleteTombstone verifies deleting a captured
// cluster-scoped object (a namespace) returns 404 on GET (not the captured copy)
// and drops it from LIST.
func TestOverlay_ClusterScopedDeleteTombstone(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	nsList := `{"apiVersion":"v1","kind":"NamespaceList","metadata":{"resourceVersion":"1"},"items":[` +
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"default"}}]}`
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{"/api/v1/namespaces": {id: "ns", at: from, body: nsList}}, nil)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, store, clock)

	// Sanity: the captured namespace is visible before deletion.
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/default", "", ""); code != 200 {
		t.Fatalf("GET captured namespace before delete: status %d, want 200", code)
	}
	if code, _ := doReq(t, http.MethodDelete, srv.URL+"/api/v1/namespaces/default", "", ""); code != 200 {
		t.Fatalf("delete namespace: status %d, want 200", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/default", "", ""); code != 404 {
		t.Errorf("GET deleted namespace: status %d, want 404 (not the captured copy)", code)
	}
	_, list := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces", "", "")
	if contains(listNames(t, list), "default") {
		t.Errorf("LIST still contains deleted namespace: %v", listNames(t, list))
	}
}

// TestOverlay_NamespaceDeleteCascade covers the reported bug: deleting an
// overlay-created namespace must cascade to objects created in it — they should
// disappear from namespaced and cluster-wide (-A) lists and single GETs.
func TestOverlay_NamespaceDeleteCascade(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// Create a namespace and a deployment in it.
	doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces", "application/json",
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"joe-test"}}`)
	deployColl := "/apis/apps/v1/namespaces/joe-test/deployments"
	doReq(t, http.MethodPost, srv.URL+deployColl, "application/json",
		`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"joe-test"}}`)

	clusterDeploys := "/apis/apps/v1/deployments"
	if _, l := doReq(t, http.MethodGet, srv.URL+clusterDeploys, "", ""); !contains(listNames(t, l), "web") {
		t.Fatalf("deployment 'web' should be visible in -A list before delete: %v", listNames(t, l))
	}

	// Delete the namespace → cascade.
	if code, _ := doReq(t, http.MethodDelete, srv.URL+"/api/v1/namespaces/joe-test", "", ""); code != 200 {
		t.Fatalf("delete namespace: status %d", code)
	}

	// Deployment gone from the cluster-wide (-A) list…
	if _, l := doReq(t, http.MethodGet, srv.URL+clusterDeploys, "", ""); contains(listNames(t, l), "web") {
		t.Errorf("deployment 'web' still in -A list after namespace delete: %v", listNames(t, l))
	}
	// …the namespaced list…
	if _, l := doReq(t, http.MethodGet, srv.URL+deployColl, "", ""); len(listNames(t, l)) != 0 {
		t.Errorf("namespaced deployment list not empty after namespace delete: %v", listNames(t, l))
	}
	// …and single GET.
	if code, _ := doReq(t, http.MethodGet, srv.URL+deployColl+"/web", "", ""); code != 404 {
		t.Errorf("GET deployment after namespace delete: status %d, want 404", code)
	}
	// The namespace itself is gone.
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/joe-test", "", ""); code != 404 {
		t.Errorf("GET deleted namespace: status %d, want 404", code)
	}
	// Creating into the deleted namespace is rejected.
	if code, _ := doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces/joe-test/configmaps", "application/json",
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm","namespace":"joe-test"}}`); code != 404 {
		t.Errorf("create into deleted namespace: status %d, want 404", code)
	}
	// So is deleting an object in it — its contents are logically gone.
	if code, _ := doReq(t, http.MethodDelete, srv.URL+deployColl+"/web", "", ""); code != 404 {
		t.Errorf("delete object in deleted namespace: status %d, want 404", code)
	}
}

// TestOverlay_NamespaceDeleteCascadeCaptured verifies deleting a namespace also
// hides captured objects that live in it (lazy read filter).
func TestOverlay_NamespaceDeleteCascadeCaptured(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	nsList := `{"apiVersion":"v1","kind":"NamespaceList","metadata":{"resourceVersion":"1"},"items":[` +
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"cap-ns"}}]}`
	cmList := `{"apiVersion":"v1","kind":"ConfigMapList","metadata":{"resourceVersion":"1"},"items":[` +
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cap-cm","namespace":"cap-ns"}}]}`
	store := buildTestStoreWithWatch(t, map[string]watchTestRecord{
		"/api/v1/namespaces":                   {id: "ns", at: from, body: nsList},
		"/api/v1/namespaces/cap-ns/configmaps": {id: "cm", at: from, body: cmList},
	}, nil)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, store, clock)

	if _, l := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/cap-ns/configmaps", "", ""); !contains(listNames(t, l), "cap-cm") {
		t.Fatalf("captured configmap should be visible before delete: %v", listNames(t, l))
	}
	if code, _ := doReq(t, http.MethodDelete, srv.URL+"/api/v1/namespaces/cap-ns", "", ""); code != 200 {
		t.Fatalf("delete captured namespace: status %d", code)
	}
	if _, l := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/cap-ns/configmaps", "", ""); len(listNames(t, l)) != 0 {
		t.Errorf("captured configmap list not empty after namespace delete: %v", listNames(t, l))
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/cap-ns/configmaps/cap-cm", "", ""); code != 404 {
		t.Errorf("GET captured configmap after namespace delete: status %d, want 404", code)
	}
}

// TestOverlay_DeleteCollection_Mixed covers issue #176: a DELETE at a
// collection path (deletecollection) must remove every matching object,
// whether it's a captured-only, overlay-created, or overlay-over-captured
// identity, in a single call.
func TestOverlay_DeleteCollection_Mixed(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock) // captures pod-base

	// Overlay-created pod.
	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-new"))
	// Overlay-over-captured: overwrite pod-base.
	doReq(t, http.MethodPut, srv.URL+podsPath+"/pod-base", "application/json", podBody("pod-base"))

	_, before := doReq(t, http.MethodGet, srv.URL+podsPath, "", "")
	names := listNames(t, before)
	if !contains(names, "pod-base") || !contains(names, "pod-new") {
		t.Fatalf("sanity: list before delete = %v, want both pod-base and pod-new", names)
	}

	code, body := doReq(t, http.MethodDelete, srv.URL+podsPath, "", "")
	if code != 200 {
		t.Fatalf("deletecollection: status %d: %s", code, body)
	}
	var status struct {
		Kind   string `json:"kind"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &status); err != nil || status.Kind != "Status" || status.Status != "Success" {
		t.Errorf("deletecollection response = %s, want kind:Status status:Success", body)
	}

	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-base", "", ""); code != 404 {
		t.Errorf("GET pod-base after deletecollection: status %d, want 404", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-new", "", ""); code != 404 {
		t.Errorf("GET pod-new after deletecollection: status %d, want 404", code)
	}
	_, after := doReq(t, http.MethodGet, srv.URL+podsPath, "", "")
	if names := listNames(t, after); len(names) != 0 {
		t.Errorf("list after deletecollection = %v, want empty", names)
	}
}

// TestOverlay_DeleteCollection_LabelSelectorFilters verifies deletecollection
// honors labelSelector: only matching items are removed.
func TestOverlay_DeleteCollection_LabelSelectorFilters(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock) // captures pod-base

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-la","namespace":"default","labels":{"app":"x"}}}`)
	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-lb","namespace":"default","labels":{"app":"y"}}}`)

	code, _ := doReq(t, http.MethodDelete, srv.URL+podsPath+"?labelSelector=app%3Dx", "", "")
	if code != 200 {
		t.Fatalf("deletecollection with selector: status %d", code)
	}

	_, list := doReq(t, http.MethodGet, srv.URL+podsPath, "", "")
	names := listNames(t, list)
	if contains(names, "pod-la") {
		t.Errorf("pod-la (matched selector) should have been deleted: %v", names)
	}
	if !contains(names, "pod-lb") || !contains(names, "pod-base") {
		t.Errorf("non-matching items should survive: %v", names)
	}
}

// TestOverlay_DeleteCollection_SetBasedSelectorFilters verifies well-formed
// set-based ("in"/"notin") labelSelectors still work for deletecollection —
// filterItemsStrict's strict parsing must reject malformed set syntax without
// breaking legitimate use.
func TestOverlay_DeleteCollection_SetBasedSelectorFilters(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock) // captures pod-base

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-la","namespace":"default","labels":{"app":"x"}}}`)
	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json",
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-lb","namespace":"default","labels":{"app":"y"}}}`)

	code, _ := doReq(t, http.MethodDelete, srv.URL+podsPath+"?labelSelector=app+in+%28x%29", "", "") // app in (x)
	if code != 200 {
		t.Fatalf("deletecollection with set-based selector: status %d", code)
	}

	_, list := doReq(t, http.MethodGet, srv.URL+podsPath, "", "")
	names := listNames(t, list)
	if contains(names, "pod-la") {
		t.Errorf("pod-la (matched \"app in (x)\") should have been deleted: %v", names)
	}
	if !contains(names, "pod-lb") || !contains(names, "pod-base") {
		t.Errorf("non-matching items should survive: %v", names)
	}
}

// TestOverlay_DeleteCollection_EmptyIsStillSuccess verifies a deletecollection
// that matches nothing still returns 200 Success, not a 404 — matching the
// real apiserver's behavior for an empty collection.
func TestOverlay_DeleteCollection_EmptyIsStillSuccess(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	// A resource with nothing captured or overlaid at all.
	code, body := doReq(t, http.MethodDelete, srv.URL+"/api/v1/namespaces/default/secrets", "", "")
	if code != 200 {
		t.Fatalf("deletecollection on empty resource: status %d: %s", code, body)
	}

	// A selector that excludes every existing item.
	code, body = doReq(t, http.MethodDelete, srv.URL+podsPath+"?labelSelector=nope%3Dnever", "", "")
	if code != 200 {
		t.Fatalf("deletecollection with non-matching selector: status %d: %s", code, body)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-base", "", ""); code != 200 {
		t.Errorf("pod-base should survive a non-matching selector delete: status %d", code)
	}
}

// TestOverlay_DeleteCollection_ClusterScoped mirrors
// TestOverlay_ClusterScopedDeleteTombstone but for a collection delete: all
// captured items at a cluster-scoped path are removed in one call.
func TestOverlay_DeleteCollection_ClusterScoped(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	nsList := `{"apiVersion":"v1","kind":"NamespaceList","metadata":{"resourceVersion":"1"},"items":[` +
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"default"}},` +
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system"}}]}`
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{"/api/v1/namespaces": {id: "ns", at: from, body: nsList}}, nil)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, store, clock)

	if code, _ := doReq(t, http.MethodDelete, srv.URL+"/api/v1/namespaces", "", ""); code != 200 {
		t.Fatalf("cluster-scoped deletecollection: status %d", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/default", "", ""); code != 404 {
		t.Errorf("GET default namespace after deletecollection: status %d, want 404", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/kube-system", "", ""); code != 404 {
		t.Errorf("GET kube-system namespace after deletecollection: status %d, want 404", code)
	}
	_, list := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces", "", "")
	if names := listNames(t, list); len(names) != 0 {
		t.Errorf("namespace list after deletecollection = %v, want empty", names)
	}
}

// TestOverlay_DeleteCollection_NamespaceCascade verifies a namespace
// deletecollection cascades into each deleted namespace's contents, exactly
// like a single namespace delete does (TestOverlay_NamespaceDeleteCascade).
func TestOverlay_DeleteCollection_NamespaceCascade(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces", "application/json",
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"joe-test"}}`)
	deployColl := "/apis/apps/v1/namespaces/joe-test/deployments"
	doReq(t, http.MethodPost, srv.URL+deployColl, "application/json",
		`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"joe-test"}}`)

	if code, _ := doReq(t, http.MethodDelete, srv.URL+"/api/v1/namespaces", "", ""); code != 200 {
		t.Fatalf("namespace deletecollection: status %d", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/joe-test", "", ""); code != 404 {
		t.Errorf("GET joe-test namespace after deletecollection: status %d, want 404", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+deployColl+"/web", "", ""); code != 404 {
		t.Errorf("deployment in cascade-deleted namespace: status %d, want 404", code)
	}
}

// TestOverlay_DeleteCollection_ClusterWideSpansNamespaces covers a
// deletecollection issued at a cluster-wide path (no namespace segment)
// against a namespaced resource — items span multiple actual namespaces, so
// each must be deleted (and its RV floor computed) using its own
// metadata.namespace rather than the request's empty namespace.
func TestOverlay_DeleteCollection_ClusterWideSpansNamespaces(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	podsAcrossNS := `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"1"},"items":[` +
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-a","namespace":"ns-a"}},` +
		`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-b","namespace":"ns-b"}}]}`
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{"/api/v1/pods": {id: "p", at: from, body: podsAcrossNS}}, nil)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, store, clock)

	if code, _ := doReq(t, http.MethodDelete, srv.URL+"/api/v1/pods", "", ""); code != 200 {
		t.Fatalf("cluster-wide deletecollection: status %d", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/ns-a/pods/pod-a", "", ""); code != 404 {
		t.Errorf("GET pod-a after cluster-wide deletecollection: status %d, want 404", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/ns-b/pods/pod-b", "", ""); code != 404 {
		t.Errorf("GET pod-b after cluster-wide deletecollection: status %d, want 404", code)
	}
}

// TestOverlay_DeleteCollection_ClusterPathFallback verifies a namespaced
// deletecollection still finds and deletes captured items when only the
// cluster-scoped list path was captured (no per-namespace list snapshot),
// mirroring the same fallback GET/LIST already has (handler.go's
// serveResource cluster-scoped fallback).
func TestOverlay_DeleteCollection_ClusterPathFallback(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{"/api/v1/pods": {id: "p", at: from, body: podList("pod-x")}}, nil)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, store, clock)

	// Sanity: the namespaced list only sees pod-x via the cluster-path fallback.
	if _, l := doReq(t, http.MethodGet, srv.URL+podsPath, "", ""); !contains(listNames(t, l), "pod-x") {
		t.Fatalf("sanity: pod-x should be visible via the namespaced list's cluster-path fallback")
	}

	if code, _ := doReq(t, http.MethodDelete, srv.URL+podsPath, "", ""); code != 200 {
		t.Fatalf("namespaced deletecollection via cluster-path fallback: status %d", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-x", "", ""); code != 404 {
		t.Errorf("GET pod-x after deletecollection: status %d, want 404", code)
	}
}

// TestOverlay_DeleteCollection_AggregateAcrossNamespacesFallback verifies a
// cluster-wide deletecollection (no namespace segment) finds items even when
// only per-namespace list paths were captured — mirroring serveResource's
// AggregateAcrossNamespaces read-path fallback (handler.go) — rather than
// silently no-oping on captured objects it can't see directly.
func TestOverlay_DeleteCollection_AggregateAcrossNamespacesFallback(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	store := buildTestStoreWithWatch(t, map[string]watchTestRecord{
		"/api/v1/namespaces/ns-a/pods": {id: "a", at: from, body: `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"1"},"items":[` +
			`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-a","namespace":"ns-a"}}]}`},
		"/api/v1/namespaces/ns-b/pods": {id: "b", at: from, body: `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"1"},"items":[` +
			`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-b","namespace":"ns-b"}}]}`},
	}, nil)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, store, clock)

	// Sanity: the cluster-wide GET only sees both pods via aggregation.
	if _, l := doReq(t, http.MethodGet, srv.URL+"/api/v1/pods", "", ""); !contains(listNames(t, l), "pod-a") || !contains(listNames(t, l), "pod-b") {
		t.Fatalf("sanity: both pods should be visible via cluster-wide aggregation: %v", listNames(t, l))
	}

	if code, _ := doReq(t, http.MethodDelete, srv.URL+"/api/v1/pods", "", ""); code != 200 {
		t.Fatalf("cluster-wide deletecollection via aggregation fallback: status %d", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/ns-a/pods/pod-a", "", ""); code != 404 {
		t.Errorf("GET pod-a after deletecollection: status %d, want 404", code)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/ns-b/pods/pod-b", "", ""); code != 404 {
		t.Errorf("GET pod-b after deletecollection: status %d, want 404", code)
	}
}

// TestOverlay_DeleteCollection_InvalidSelectorRejected verifies deletecollection
// rejects a malformed or unsupported labelSelector/fieldSelector with 400
// (filterItemsStrict, backed by k8s.io/apimachinery's labels.Parse and
// fields.ParseSelector) rather than the read path's best-effort leniency
// (filterItems/applySelectors) — which for a mutating deletecollection would
// mean "delete more than the caller asked for," not just "display more than
// intended."
func TestOverlay_DeleteCollection_InvalidSelectorRejected(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{"unsupported fieldSelector key", "?fieldSelector=spec.nodeName%3Dnode-1"},
		// A bare "!" isn't a valid requirement (! must be followed by a key).
		{"bare negation, no key", "?labelSelector=%21"},
		// labels.Parse rejects a stray comma outright.
		{"empty labelSelector segment", "?labelSelector=%2C"},
		// A key must be a valid qualified name; ")" isn't one.
		{"invalid labelSelector key syntax", "?labelSelector=%21%29"},
		// An unclosed "notin (" is a syntax error in the real grammar.
		{"unclosed notin paren", "?labelSelector=app+notin+%28"},
		// fields.ParseSelector is lenient about a stray comma (parses to an
		// Empty selector rather than erroring) — caught by filterItemsStrict's
		// explicit Empty() check instead.
		{"empty fieldSelector segment", "?fieldSelector=%2C"},
		// Whitespace-only selectors parse successfully to an Empty selector in
		// both labels.Parse and fields.ParseSelector — also caught by the
		// Empty() check, since the caller supplied a non-empty selector string
		// that nonetheless restricts nothing.
		{"whitespace-only labelSelector", "?labelSelector=+"},
		{"whitespace-only fieldSelector", "?fieldSelector=+"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
			srv := newWritableServer(t, writableTestStore(t, from), clock) // captures pod-base

			code, _ := doReq(t, http.MethodDelete, srv.URL+podsPath+c.query, "", "")
			if code != http.StatusBadRequest {
				t.Errorf("deletecollection with %s: status %d, want 400", c.name, code)
			}
			if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-base", "", ""); code != 200 {
				t.Errorf("pod-base should survive a rejected selector: status %d", code)
			}
		})
	}
}

// TestOverlay_DeleteCollection_NonListCaptureBodyIsBestEffort verifies
// deletecollection doesn't hard-fail when the captured body at the list path
// isn't list-shaped (e.g. a Table-format snapshot) — it's treated as zero
// captured items (best-effort, matching CaptureStore.ReconstructAt's own
// tolerance of non-list bodies), and overlay-owned items still delete cleanly.
func TestOverlay_DeleteCollection_NonListCaptureBodyIsBestEffort(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	tableBody := `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[],"rows":[]}`
	store := buildTestStoreWithWatch(t,
		map[string]watchTestRecord{podsPath: {id: "t", at: from, body: tableBody}}, nil)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, store, clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-overlay"))

	code, body := doReq(t, http.MethodDelete, srv.URL+podsPath, "", "")
	if code != 200 {
		t.Fatalf("deletecollection over a non-list captured body: status %d: %s", code, body)
	}
	if code, _ := doReq(t, http.MethodGet, srv.URL+podsPath+"/pod-overlay", "", ""); code != 404 {
		t.Errorf("GET pod-overlay after deletecollection: status %d, want 404", code)
	}
}

// TestOverlay_DeleteCollection_NonListClusterBodyDoesNotAggregate is the
// cluster-wide counterpart: a cluster-wide deletecollection (no namespace
// segment) whose own list path was captured as a non-list (e.g. Table) 200
// response must not fall back to aggregating per-namespace captures — a
// GET/LIST on the same cluster-wide path wouldn't (serveResource only
// aggregates on an actual 404), so pods captured only under per-namespace
// paths must survive.
func TestOverlay_DeleteCollection_NonListClusterBodyDoesNotAggregate(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	tableBody := `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[],"rows":[]}`
	store := buildTestStoreWithWatch(t, map[string]watchTestRecord{
		"/api/v1/pods": {id: "t", at: from, body: tableBody},
		"/api/v1/namespaces/ns-a/pods": {id: "a", at: from, body: `{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"1"},"items":[` +
			`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"pod-a","namespace":"ns-a"}}]}`},
	}, nil)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, store, clock)

	// Sanity: pod-a is independently visible via its own namespaced path.
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/ns-a/pods/pod-a", "", ""); code != 200 {
		t.Fatalf("sanity: pod-a should exist before delete, status %d", code)
	}

	if code, _ := doReq(t, http.MethodDelete, srv.URL+"/api/v1/pods", "", ""); code != 200 {
		t.Fatalf("cluster-wide deletecollection over a non-list captured body: status %d", code)
	}
	// pod-a lives only under its own namespaced capture, invisible to a plain
	// GET on the cluster-wide path (which just returns the Table body as-is).
	// The cluster-wide deletecollection must see the same (empty) item set, not
	// aggregate across every namespace and delete it too.
	if code, _ := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/ns-a/pods/pod-a", "", ""); code != 200 {
		t.Errorf("pod-a should survive the cluster-wide deletecollection, status %d", code)
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

	// The kube-root-ca.crt ConfigMap is synthesized too.
	code, cm := doReq(t, http.MethodGet, srv.URL+"/api/v1/namespaces/ns-x/configmaps/kube-root-ca.crt", "", "")
	if code != http.StatusOK {
		t.Fatalf("GET kube-root-ca.crt: status %d, want 200\n%s", code, cm)
	}
	if n := metaString(cm, "name"); n != "kube-root-ca.crt" {
		t.Errorf("CM name = %q, want kube-root-ca.crt", n)
	}
}

// A WatchList informer (sendInitialEvents=true) must see overlay-created objects
// in the initial burst and receive the k8s.io/initial-events-end BOOKMARK, or it
// never completes its initial sync. (issues #152/#153)
func TestOverlay_WatchListInitialEvents(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	nsBody := `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"wl"}}`
	if code, _ := doReq(t, http.MethodPost, srv.URL+"/api/v1/namespaces", "application/json", nsBody); code != http.StatusCreated {
		t.Fatalf("create namespace: status %d, want 201", code)
	}

	url := srv.URL + "/api/v1/namespaces/wl/serviceaccounts?watch=true&sendInitialEvents=true&timeoutSeconds=2"
	_, tryNext, cancel := openWatchStream(t, url)
	defer cancel()

	var sawSA, sawInitialEndBookmark bool
	for {
		e, ok := tryNext(3 * time.Second)
		if !ok {
			break
		}
		if e.Type == "ADDED" && e.Object.Metadata.Name == "default" {
			sawSA = true
		}
		if e.Type == "BOOKMARK" && e.Object.Metadata.Annotations["k8s.io/initial-events-end"] == "true" {
			sawInitialEndBookmark = true
			break
		}
	}
	if !sawSA {
		t.Error("WatchList initial burst did not include the overlay-synthesized default SA")
	}
	if !sawInitialEndBookmark {
		t.Error("WatchList did not emit a BOOKMARK with the k8s.io/initial-events-end annotation")
	}
}

// TestEnsureSchedulableNode_NodeSnapshotAfterWindowStart reproduces the bug where
// a capture that contains a node still got a synthetic kwok-node-0 because the
// node's first snapshot lands after the window start, so a check at `from` sees
// "no nodes". Eager synthesis must evaluate node presence across the window.
func TestEnsureSchedulableNode_NodeSnapshotAfterWindowStart(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	nodeAt := from.Add(30 * time.Second) // first /api/v1/nodes snapshot after `from`
	to := from.Add(60 * time.Second)
	store := buildTestStoreWithWatch(t, map[string]watchTestRecord{
		"/api/v1/nodes": {id: "n", at: nodeAt,
			body: `{"apiVersion":"v1","kind":"NodeList","items":[{"metadata":{"name":"node-real"}}]}`},
	}, nil)
	clock, _ := newTestClock(t, from, to, 1, false, true) // paused at from
	h := newHandler(store, time.Time{}, false)
	h.clock = clock
	h.overlay = newOverlay()
	h.schedulePods = true

	// Precondition: at the window start the node snapshot isn't visible yet.
	if got := h.knownNodeNamesAt(from); len(got) != 0 {
		t.Fatalf("precondition: expected no nodes at window start, got %v", got)
	}

	h.ensureSchedulableNode()

	names := h.knownNodeNamesAt(to)
	if contains(names, defaultSyntheticNode) {
		t.Errorf("synthesized %s for a capture that already has a node: %v", defaultSyntheticNode, names)
	}
	if !contains(names, "node-real") {
		t.Errorf("expected captured node-real at window end, got %v", names)
	}
}

// TestEnsureSchedulableNode_NodelessCaptureSynthesizes verifies the intended KWOK
// behavior is preserved: a capture with no nodes still gets kwok-node-0.
func TestEnsureSchedulableNode_NodelessCaptureSynthesizes(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	to := from.Add(60 * time.Second)
	store := buildTestStoreWithWatch(t, map[string]watchTestRecord{
		podsPath: {id: "p", at: from, body: podList("p")},
	}, nil)
	clock, _ := newTestClock(t, from, to, 1, false, true)
	h := newHandler(store, time.Time{}, false)
	h.clock = clock
	h.overlay = newOverlay()
	h.schedulePods = true

	h.ensureSchedulableNode()

	if !contains(h.knownNodeNamesAt(to), defaultSyntheticNode) {
		t.Errorf("nodeless capture should synthesize %s", defaultSyntheticNode)
	}
}
