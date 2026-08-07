package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// mustFieldSelector parses a selector that is expected to be valid.
func mustFieldSelector(t *testing.T, group, resource, sel string) *FieldSelector {
	t.Helper()
	fs, err := ParseFieldSelector(group, resource, sel)
	if err != nil {
		t.Fatalf("ParseFieldSelector(%q, %q, %q): unexpected error: %v", group, resource, sel, err)
	}
	return fs
}

// obj marshals a JSON object literal for use as a single list item.
func obj(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// pod builds a minimal pod carrying the fields the pod selectors read.
func pod(t *testing.T, name, namespace, nodeName, phase string) json.RawMessage {
	t.Helper()
	return obj(t, map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{"nodeName": nodeName},
		"status":     map[string]any{"phase": phase},
	})
}

// matchNames returns the names of the items the selector matches.
func matchNames(t *testing.T, fs *FieldSelector, items []json.RawMessage) []string {
	t.Helper()
	var out []string
	for _, raw := range items {
		var o K8sObject
		if err := json.Unmarshal(raw, &o); err != nil {
			t.Fatalf("unmarshal item: %v", err)
		}
		if fs.Matches(raw, &o) {
			out = append(out, o.Metadata.Name)
		}
	}
	return out
}

// TestParseFieldSelector_RejectsUnsupportedLabel is the core of #339: a label
// the kind does not accept must be an error the caller turns into a 400, not a
// silently dropped requirement that widens the selector to match everything.
func TestParseFieldSelector_RejectsUnsupportedLabel(t *testing.T) {
	cases := []struct {
		group, resource, sel string
		wantMsg              string
	}{
		// A registered kind uses its conversion func's message.
		{"", "pods", "spec.bogus=x", "field label not supported: spec.bogus"},
		{"", "nodes", "status.phase=Running", "field label not supported: status.phase"},
		// Cluster-scoped kinds do not accept metadata.namespace: upstream's Node
		// conversion func lists only metadata.name and spec.unschedulable.
		{"", "nodes", "metadata.namespace=default", "field label not supported: metadata.namespace"},
		{"certificates.k8s.io", "certificatesigningrequests", "metadata.namespace=x",
			"field label not supported: metadata.namespace"},
		// batch/v1's Job conversion func has its own wording.
		{"batch", "jobs", "spec.parallelism=2", `field label "spec.parallelism" not supported for Job`},
		// An unregistered kind (every custom resource, and everything in the
		// apps group, which registers no conversion func) falls back to
		// runtime.DefaultMetaV1FieldSelectorConversion and its distinct message.
		{"example.com", "widgets", "spec.size=large",
			`"spec.size" is not a known field selector: only "metadata.name", "metadata.namespace"`},
		{"apps", "deployments", "status.replicas=3",
			`"status.replicas" is not a known field selector: only "metadata.name", "metadata.namespace"`},
		// events.k8s.io/v1 accepts regarding.* — the core spelling is not valid
		// against that group even though it is the canonical internal label.
		{"events.k8s.io", "events", "involvedObject.name=web",
			"field label not supported: involvedObject.name"},
		{"events.k8s.io", "events", "source=kubelet", "field label not supported: source"},
	}
	for _, tc := range cases {
		fs, err := ParseFieldSelector(tc.group, tc.resource, tc.sel)
		if err == nil {
			t.Errorf("ParseFieldSelector(%q, %q, %q) = %v, want error",
				tc.group, tc.resource, tc.sel, fs)
			continue
		}
		if err.Error() != tc.wantMsg {
			t.Errorf("ParseFieldSelector(%q, %q, %q) error = %q, want %q",
				tc.group, tc.resource, tc.sel, err.Error(), tc.wantMsg)
		}
	}
}

// TestParseFieldSelector_AcceptsPerKindLabels guards the other direction: keys a
// real apiserver accepts must not be rejected. deletecollection used to 400 on
// pods' spec.nodeName, which upstream accepts.
func TestParseFieldSelector_AcceptsPerKindLabels(t *testing.T) {
	cases := []struct{ group, resource, sel string }{
		{"", "pods", "spec.nodeName=node-1"},
		{"", "pods", "status.phase=Running"},
		{"", "pods", "spec.restartPolicy=Always"},
		{"", "pods", "spec.schedulerName=default-scheduler"},
		{"", "pods", "spec.serviceAccountName=default"},
		{"", "pods", "spec.hostNetwork=false"},
		{"", "pods", "status.podIP=10.0.0.1"},
		{"", "pods", "status.nominatedNodeName=node-2"},
		{"", "pods", "metadata.name=web,metadata.namespace=default,spec.nodeName=node-1"},
		{"", "nodes", "spec.unschedulable=true"},
		{"", "nodes", "metadata.name=node-1"},
		{"", "namespaces", "status.phase=Active"},
		{"", "secrets", "type=kubernetes.io/service-account-token"},
		{"", "services", "spec.clusterIP=10.96.0.1"},
		{"", "services", "spec.type=ClusterIP"},
		{"", "replicationcontrollers", "status.replicas=3"},
		{"", "events", "involvedObject.name=web"},
		{"", "events", "involvedObject.kind=Pod,reason=Scheduled,type=Normal"},
		{"", "events", "source=kubelet"},
		{"", "events", "reportingComponent=kubelet"},
		{"events.k8s.io", "events", "regarding.name=web"},
		{"events.k8s.io", "events", "reportingController=kubelet"},
		{"batch", "jobs", "status.successful=1"},
		{"certificates.k8s.io", "certificatesigningrequests", "spec.signerName=kubernetes.io/kubelet-serving"},
		// The metadata keys stay valid for an unregistered kind.
		{"example.com", "widgets", "metadata.name=w1"},
		{"example.com", "widgets", "metadata.namespace=default"},
	}
	for _, tc := range cases {
		if _, err := ParseFieldSelector(tc.group, tc.resource, tc.sel); err != nil {
			t.Errorf("ParseFieldSelector(%q, %q, %q): unexpected error: %v",
				tc.group, tc.resource, tc.sel, err)
		}
	}
}

// TestFieldSelector_FiltersOnNonMetadataKeys is the issue's reproduction: a
// supported key with a non-matching value must filter, not match everything.
func TestFieldSelector_FiltersOnNonMetadataKeys(t *testing.T) {
	items := []json.RawMessage{
		pod(t, "web-1", "demo", "node-a", "Running"),
		pod(t, "web-2", "demo", "node-b", "Failed"),
	}
	cases := []struct {
		sel  string
		want []string
	}{
		{"spec.nodeName=does-not-exist", nil},
		{"spec.nodeName=node-a", []string{"web-1"}},
		{"spec.nodeName!=node-a", []string{"web-2"}},
		{"status.phase=Failed", []string{"web-2"}},
		{"status.phase=Running", []string{"web-1"}},
		{"status.phase=Pending", nil},
		// A legacy alias is rewritten to its canonical label before matching.
		{"spec.host=node-a", []string{"web-1"}},
		// Requirements combine with AND across keys.
		{"spec.nodeName=node-a,status.phase=Running", []string{"web-1"}},
		{"spec.nodeName=node-a,status.phase=Failed", nil},
		{"metadata.namespace=demo,spec.nodeName=node-b", []string{"web-2"}},
	}
	for _, tc := range cases {
		fs := mustFieldSelector(t, "", "pods", tc.sel)
		got := matchNames(t, fs, items)
		if !stringSliceEqual(got, tc.want) {
			t.Errorf("[%q] matched %v, want %v", tc.sel, got, tc.want)
		}
	}
}

// TestFieldSelector_AcceptedButNotSelectable pins the upstream drift between the
// two layers. Pods' conversion func accepts status.podIPs but
// PodToSelectableFields never sets it, so upstream accepts the request and then
// matches nothing — fields.Set.Get returns "" for a key that was never set.
func TestFieldSelector_AcceptedButNotSelectable(t *testing.T) {
	items := []json.RawMessage{
		pod(t, "web-1", "demo", "node-a", "Running"),
		pod(t, "web-2", "demo", "node-b", "Failed"),
	}

	fs := mustFieldSelector(t, "", "pods", "status.podIPs=10.0.0.1")
	if got := matchNames(t, fs, items); got != nil {
		t.Errorf("status.podIPs=10.0.0.1 matched %v, want no items", got)
	}
	// Matching the empty value matches everything, exactly as upstream does.
	fs = mustFieldSelector(t, "", "pods", "status.podIPs=")
	if got := matchNames(t, fs, items); !stringSliceEqual(got, []string{"web-1", "web-2"}) {
		t.Errorf("status.podIPs= matched %v, want both items", got)
	}
}

// TestFieldSelector_AbsentFieldDefaults checks that an unset field reads the way
// ToSelectableFields would have written it: bools as "false", counts as "0",
// strings as empty. A capture that never recorded the field is the common case.
func TestFieldSelector_AbsentFieldDefaults(t *testing.T) {
	bare := obj(t, map[string]any{"metadata": map[string]any{"name": "x"}})

	cases := []struct {
		group, resource, sel string
		want                 bool
	}{
		// Pods: spec.hostNetwork is a bool, so absent reads "false".
		{"", "pods", "spec.hostNetwork=false", true},
		{"", "pods", "spec.hostNetwork=true", false},
		// Nodes: same for spec.unschedulable.
		{"", "nodes", "spec.unschedulable=false", true},
		{"", "nodes", "spec.unschedulable=true", false},
		// Counts read "0" when absent.
		{"", "replicationcontrollers", "status.replicas=0", true},
		{"", "replicationcontrollers", "status.replicas=3", false},
		{"batch", "jobs", "status.successful=0", true},
		// Strings read empty when absent, so a non-empty query cannot match.
		{"", "pods", "spec.nodeName=node-a", false},
		{"", "pods", "spec.nodeName=", true},
	}
	for _, tc := range cases {
		fs := mustFieldSelector(t, tc.group, tc.resource, tc.sel)
		if got := fs.Matches(bare, nil); got != tc.want {
			t.Errorf("[%s %s %q] Matches = %v, want %v",
				tc.group, tc.resource, tc.sel, got, tc.want)
		}
	}
}

// TestFieldSelector_ValueStringification covers the non-string wire types:
// upstream stringifies bools with FormatBool and counts with Itoa, so a JSON
// number must not come back as "3e+00".
func TestFieldSelector_ValueStringification(t *testing.T) {
	hostNet := obj(t, map[string]any{
		"metadata": map[string]any{"name": "p"},
		"spec":     map[string]any{"hostNetwork": true},
	})
	if fs := mustFieldSelector(t, "", "pods", "spec.hostNetwork=true"); !fs.Matches(hostNet, nil) {
		t.Error("spec.hostNetwork=true should match a pod with hostNetwork: true")
	}
	if fs := mustFieldSelector(t, "", "pods", "spec.hostNetwork=false"); fs.Matches(hostNet, nil) {
		t.Error("spec.hostNetwork=false should not match a pod with hostNetwork: true")
	}

	rc := obj(t, map[string]any{
		"metadata": map[string]any{"name": "rc"},
		"status":   map[string]any{"replicas": 3},
	})
	if fs := mustFieldSelector(t, "", "replicationcontrollers", "status.replicas=3"); !fs.Matches(rc, nil) {
		t.Error("status.replicas=3 should match replicas: 3")
	}

	// Job's label and field differ: status.successful reads status.succeeded.
	job := obj(t, map[string]any{
		"metadata": map[string]any{"name": "j"},
		"status":   map[string]any{"succeeded": 2},
	})
	if fs := mustFieldSelector(t, "batch", "jobs", "status.successful=2"); !fs.Matches(job, nil) {
		t.Error("status.successful=2 should match status.succeeded: 2")
	}
}

// TestFieldSelector_EventPaths covers the two Event API groups, whose wire
// formats differ while sharing the same canonical labels, plus core Events'
// "source", which upstream computes with a fallback.
func TestFieldSelector_EventPaths(t *testing.T) {
	coreEvent := obj(t, map[string]any{
		"metadata":       map[string]any{"name": "e1", "namespace": "demo"},
		"involvedObject": map[string]any{"kind": "Pod", "name": "web-1"},
		"reason":         "Scheduled",
		"type":           "Normal",
		"source":         map[string]any{"component": "default-scheduler"},
	})
	for _, sel := range []string{
		"involvedObject.kind=Pod",
		"involvedObject.name=web-1",
		"reason=Scheduled",
		"type=Normal",
		"source=default-scheduler",
	} {
		if fs := mustFieldSelector(t, "", "events", sel); !fs.Matches(coreEvent, nil) {
			t.Errorf("core event: %q should match", sel)
		}
	}

	// source falls back to reportingComponent when source.component is unset,
	// mirroring upstream's ToSelectableFields.
	fallbackEvent := obj(t, map[string]any{
		"metadata":           map[string]any{"name": "e2"},
		"reportingComponent": "kubelet",
	})
	if fs := mustFieldSelector(t, "", "events", "source=kubelet"); !fs.Matches(fallbackEvent, nil) {
		t.Error("core event: source should fall back to reportingComponent")
	}

	// events.k8s.io/v1 carries regarding.* and reportingController on the wire,
	// and its accepted labels are spelled that way too.
	newEvent := obj(t, map[string]any{
		"metadata":            map[string]any{"name": "e3", "namespace": "demo"},
		"regarding":           map[string]any{"kind": "Pod", "name": "web-1"},
		"reportingController": "kubelet",
		"reason":              "Scheduled",
	})
	for _, sel := range []string{
		"regarding.kind=Pod",
		"regarding.name=web-1",
		"reportingController=kubelet",
		"reason=Scheduled",
	} {
		if fs := mustFieldSelector(t, "events.k8s.io", "events", sel); !fs.Matches(newEvent, nil) {
			t.Errorf("events.k8s.io event: %q should match", sel)
		}
	}
	if fs := mustFieldSelector(t, "events.k8s.io", "events", "regarding.name=other"); fs.Matches(newEvent, nil) {
		t.Error("events.k8s.io event: regarding.name=other should not match")
	}
}

// TestParseFieldSelector_MalformedGrammar checks we use apimachinery's real
// grammar. Whitespace around the operator is *not* trimmed upstream — the label
// becomes "metadata.name " and the conversion rejects it — so k8shark's old
// hand-rolled trimming was itself a divergence.
func TestParseFieldSelector_MalformedGrammar(t *testing.T) {
	if _, err := ParseFieldSelector("", "pods", "metadata.name = nginx"); err == nil {
		t.Error(`"metadata.name = nginx" should be rejected: upstream does not trim around the operator`)
	}
	if _, err := ParseFieldSelector("", "pods", "not-a-selector"); err == nil {
		t.Error("a segment with no operator should be rejected")
	}
	// A stray comma parses to zero requirements upstream rather than erroring,
	// so the read path accepts it as "no restriction".
	fs, err := ParseFieldSelector("", "pods", ",")
	if err != nil {
		t.Fatalf("a stray comma should parse: %v", err)
	}
	if fs.Restricts() {
		t.Error("a selector with zero requirements should not restrict")
	}
}

func TestParseFieldSelector_Empty(t *testing.T) {
	fs, err := ParseFieldSelector("", "pods", "")
	if err != nil {
		t.Fatalf("empty selector: %v", err)
	}
	if fs != nil {
		t.Errorf("empty selector should parse to nil, got %v", fs)
	}
	// A nil selector matches everything, so callers can pass it through.
	if !fs.Matches(pod(t, "web", "demo", "node-a", "Running"), nil) {
		t.Error("a nil selector should match every object")
	}
	if fs.Restricts() {
		t.Error("a nil selector should not restrict")
	}
}

// TestFieldSelector_Aliases_Canonicalize checks the alias rewrite happens once,
// at parse time, so String() reports the canonical form.
func TestFieldSelector_Aliases_Canonicalize(t *testing.T) {
	fs := mustFieldSelector(t, "", "pods", "spec.host=node-a")
	if got := fs.String(); !strings.Contains(got, "spec.nodeName=node-a") {
		t.Errorf("spec.host should canonicalize to spec.nodeName, got %q", got)
	}
}

func TestNamespaceScopeSelector(t *testing.T) {
	items := []json.RawMessage{
		pod(t, "web-1", "demo", "node-a", "Running"),
		pod(t, "web-2", "other", "node-b", "Running"),
	}
	fs := NamespaceScopeSelector("demo")
	if got := matchNames(t, fs, items); !stringSliceEqual(got, []string{"web-1"}) {
		t.Errorf("namespace scope matched %v, want [web-1]", got)
	}
}

// TestFieldSelector_MatchesWithoutDecodedObject checks the raw-only path used
// where no decoded K8sObject is at hand.
func TestFieldSelector_MatchesWithoutDecodedObject(t *testing.T) {
	p := pod(t, "web-1", "demo", "node-a", "Running")
	if fs := mustFieldSelector(t, "", "pods", "metadata.name=web-1"); !fs.Matches(p, nil) {
		t.Error("metadata-only selector should match with a nil decoded object")
	}
	if fs := mustFieldSelector(t, "", "pods", "metadata.name=other"); fs.Matches(p, nil) {
		t.Error("metadata-only selector should not match a different name")
	}
}

// TestFieldSelector_UndecodableObjectIsKept keeps the existing convention: an
// item we cannot inspect is never silently hidden.
func TestFieldSelector_UndecodableObjectIsKept(t *testing.T) {
	fs := mustFieldSelector(t, "", "pods", "spec.nodeName=node-a")
	if !fs.Matches(json.RawMessage(`not json`), nil) {
		t.Error("an undecodable object should count as a match")
	}
}

// TestApplySelectors_NonMetadataFieldFilter exercises the list path end to end,
// which is what `kubectl get pods --field-selector spec.nodeName=…` hits.
func TestApplySelectors_NonMetadataFieldFilter(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "PodList",
		"metadata":   map[string]any{},
		"items": []json.RawMessage{
			pod(t, "web-1", "demo", "node-a", "Running"),
			pod(t, "web-2", "demo", "node-b", "Running"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := ApplySelectors(body, "", mustFieldSelector(t, "", "pods", "spec.nodeName=node-a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(t, out); !stringSliceEqual(got, []string{"web-1"}) {
		t.Errorf("got %v, want [web-1]", got)
	}

	out, err = ApplySelectors(body, "", mustFieldSelector(t, "", "pods", "spec.nodeName=does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(t, out); len(got) != 0 {
		t.Errorf("got %v, want no items", got)
	}
}

// TestListIdentities covers the basis for filtering a Table whose rows carry
// only metadata: ok must be false when there is no list to intersect with, so
// the caller falls back instead of dropping every row.
func TestListIdentities(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "PodList",
		"items": []json.RawMessage{
			pod(t, "web-1", "demo", "node-a", "Running"),
			pod(t, "web-2", "other", "node-b", "Running"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids, ok := ListIdentities(body)
	if !ok {
		t.Fatal("ListIdentities on a list body: ok = false")
	}
	if !ids[ObjectIdentity{"demo", "web-1"}] || !ids[ObjectIdentity{"other", "web-2"}] {
		t.Errorf("identities = %v, want both pods", ids)
	}
	if len(ids) != 2 {
		t.Errorf("got %d identities, want 2", len(ids))
	}

	if _, ok := ListIdentities([]byte(`{"kind":"Pod"}`)); ok {
		t.Error("a non-list body should report ok = false")
	}
	if _, ok := ListIdentities([]byte(`not json`)); ok {
		t.Error("an undecodable body should report ok = false")
	}
}

// TestFilterTableRowsToIdentities checks rows are kept by namespace/name, which
// is all a PartialObjectMetadata row exposes.
func TestFilterTableRowsToIdentities(t *testing.T) {
	table, err := json.Marshal(map[string]any{
		"apiVersion": "meta.k8s.io/v1",
		"kind":       "Table",
		"rows": []map[string]any{
			{"cells": []any{"web-1"}, "object": map[string]any{
				"kind":     "PartialObjectMetadata",
				"metadata": map[string]any{"name": "web-1", "namespace": "demo"},
			}},
			{"cells": []any{"web-2"}, "object": map[string]any{
				"kind":     "PartialObjectMetadata",
				"metadata": map[string]any{"name": "web-2", "namespace": "demo"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out, err := FilterTableRowsToIdentities(table, map[ObjectIdentity]bool{
		{"demo", "web-1"}: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Rows []struct {
			Object struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"object"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 || got.Rows[0].Object.Metadata.Name != "web-1" {
		t.Errorf("kept %d rows (first %q), want just web-1",
			len(got.Rows), got.Rows[0].Object.Metadata.Name)
	}

	// A body that isn't a Table comes back untouched.
	plain := []byte(`{"kind":"PodList"}`)
	out, err = FilterTableRowsToIdentities(plain, nil)
	if err != nil || string(out) != string(plain) {
		t.Errorf("non-Table body = %s, %v; want it unchanged", out, err)
	}
}

// TestListIdentities_UndecodableItemIsKept guards the convention the rest of
// this file follows: an item we cannot fully decode is never silently hidden.
// K8sObject declares Labels as map[string]string, so an object with a
// non-string label value fails to unmarshal against it — and since FilterItems
// keeps such an item, dropping its identity here would make the Table path lose
// a row the JSON list path kept.
func TestListIdentities_UndecodableItemIsKept(t *testing.T) {
	odd := json.RawMessage(
		`{"metadata":{"name":"odd","namespace":"demo","labels":{"a":1}},"spec":{"nodeName":"node-a"}}`)

	// Precondition: this really is undecodable as a K8sObject, so the test
	// stays meaningful if that struct changes.
	var probe K8sObject
	if err := json.Unmarshal(odd, &probe); err == nil {
		t.Skip("K8sObject now decodes non-string labels; pick another malformed shape")
	}

	body, err := json.Marshal(map[string]any{"items": []json.RawMessage{odd}})
	if err != nil {
		t.Fatal(err)
	}
	ids, ok := ListIdentities(body)
	if !ok {
		t.Fatal("ListIdentities ok = false")
	}
	if !ids[ObjectIdentity{"demo", "odd"}] {
		t.Errorf("identity dropped for an item FilterItems would keep: %v", ids)
	}

	// And the two paths agree: the row survives Table filtering too.
	table, err := json.Marshal(map[string]any{
		"kind": "Table",
		"rows": []map[string]any{{"cells": []any{"odd"}, "object": json.RawMessage(odd)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := FilterTableRowsToIdentities(table, ids)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Rows []json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 1 {
		t.Errorf("kept %d rows, want 1 — the JSON list path keeps this item", len(got.Rows))
	}
}

// TestListIdentities_UndecodableIdentityFailsClosed covers what identityOnly
// still cannot decode. Skipping such an item would silently drop the Table row
// for an object the JSON list path keeps, so the whole identity set is reported
// unusable and the caller falls back instead of filtering partially.
func TestListIdentities_UndecodableIdentityFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		item string
	}{
		{"numeric name", `{"metadata":{"name":5,"namespace":"demo"}}`},
		{"metadata not an object", `{"metadata":"nope"}`},
		{"item not an object", `"just-a-string"`},
		{"namespace not a string", `{"metadata":{"name":"ok","namespace":[]}}`},
	}
	for _, tc := range cases {
		raw := json.RawMessage(tc.item)
		// Precondition: FilterItems keeps this item, which is what makes
		// dropping its row a divergence rather than a wash.
		kept := FilterItems([]json.RawMessage{raw}, "",
			mustFieldSelector(t, "", "pods", "spec.nodeName=node-a"))
		if len(kept) != 1 {
			t.Errorf("[%s] FilterItems kept %d items, want 1 (undecodable items are never hidden)",
				tc.name, len(kept))
		}

		body, err := json.Marshal(map[string]any{"items": []json.RawMessage{raw}})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := ListIdentities(body); ok {
			t.Errorf("[%s] ListIdentities reported ok; want it to fail closed so the "+
				"caller falls back rather than dropping the row", tc.name)
		}
	}

	// A well-formed list alongside is still usable — failing closed must not
	// mean failing always.
	body, err := json.Marshal(map[string]any{
		"items": []json.RawMessage{pod(t, "web-1", "demo", "node-a", "Running")},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids, ok := ListIdentities(body)
	if !ok || !ids[ObjectIdentity{"demo", "web-1"}] {
		t.Errorf("well-formed list should still yield identities: ok=%v ids=%v", ok, ids)
	}
}

// TestFieldSelector_OutOfRangeNumber checks a count that cannot fit in an int64.
// Converting an out-of-range float64 to int64 is undefined in Go, so without a
// range check a wildly large value stringifies to something arbitrary — and
// archive content is not trusted to hold only plausible counts.
func TestFieldSelector_OutOfRangeNumber(t *testing.T) {
	huge := obj(t, map[string]any{
		"metadata": map[string]any{"name": "rc"},
		"status":   map[string]any{"replicas": 1e30},
	})
	// Assert the exact rendering rather than "not some wrong value": the result
	// of the undefined conversion is platform-specific (amd64 gives MinInt64,
	// arm64 saturates to MaxInt64), so a negative assertion would pass
	// vacuously on one of them.
	fs := mustFieldSelector(t, "", "replicationcontrollers",
		"status.replicas=1000000000000000000000000000000")
	if !fs.Matches(huge, nil) {
		t.Error("a replicas value of 1e30 should render as its full decimal expansion")
	}
	// Sanity: an in-range count still formats as an integer, not 3e+00.
	normal := obj(t, map[string]any{
		"metadata": map[string]any{"name": "rc"},
		"status":   map[string]any{"replicas": 3},
	})
	if fs := mustFieldSelector(t, "", "replicationcontrollers", "status.replicas=3"); !fs.Matches(normal, nil) {
		t.Error("status.replicas=3 should match replicas: 3")
	}
}

// TestFieldSelector_DecodeMode pins which selectors can be answered from an
// already-decoded K8sObject and which need the whole object. Getting this wrong
// is silent in both directions — a raw-only path read from the struct resolves
// to "" and matches nothing, and a struct-resolvable path sent through the raw
// decode just costs time — so assert the classification directly.
func TestFieldSelector_DecodeMode(t *testing.T) {
	cases := []struct {
		group, resource, sel string
		want                 decodeMode
	}{
		// metadata, spec and status all live on the struct.
		{"", "pods", "metadata.name=web", decodeFromObject},
		{"", "pods", "spec.nodeName=node-a", decodeFromObject},
		{"", "pods", "status.phase=Running", decodeFromObject},
		{"", "pods", "spec.nodeName=node-a,status.phase=Running", decodeFromObject},
		{"", "nodes", "spec.unschedulable=true", decodeFromObject},
		{"", "namespaces", "status.phase=Active", decodeFromObject},
		{"", "services", "spec.type=ClusterIP", decodeFromObject},
		{"", "replicationcontrollers", "status.replicas=3", decodeFromObject},
		{"batch", "jobs", "status.successful=1", decodeFromObject},
		{"certificates.k8s.io", "certificatesigningrequests", "spec.signerName=x", decodeFromObject},
		// Accepted but not selectable: resolves to "" from any source, so it
		// needs no raw decode.
		{"", "pods", "status.podIPs=10.0.0.1", decodeFromObject},
		// Secrets keep type at the top level, outside spec/status.
		{"", "secrets", "type=Opaque", decodeFromRaw},
		// Every Event field the selectors read is top-level.
		{"", "events", "involvedObject.kind=Pod", decodeFromRaw},
		{"", "events", "reason=Scheduled", decodeFromRaw},
		{"", "events", "type=Normal", decodeFromRaw},
		{"", "events", "source=kubelet", decodeFromRaw},
		{"events.k8s.io", "events", "regarding.kind=Pod", decodeFromRaw},
		{"events.k8s.io", "events", "reportingController=kubelet", decodeFromRaw},
		// One raw-only requirement forces the whole selector to raw.
		{"", "events", "metadata.name=e1,reason=Scheduled", decodeFromRaw},
	}
	for _, tc := range cases {
		fs := mustFieldSelector(t, tc.group, tc.resource, tc.sel)
		if fs.mode != tc.want {
			t.Errorf("[%s %s %q] mode = %v, want %v",
				tc.group, tc.resource, tc.sel, fs.mode, tc.want)
		}
	}
}

// TestFieldSelector_StructPathIgnoresRaw proves the fast path really is taken:
// with a decoded object supplied and the raw bytes deliberately unusable, a
// spec/status selector must still filter correctly. If the mode ever regresses
// to decodeFromRaw, the unmarshal fails and Matches returns true — an
// over-match, which is the bug this whole change set is about.
func TestFieldSelector_StructPathIgnoresRaw(t *testing.T) {
	var onNodeA, onNodeB K8sObject
	if err := json.Unmarshal(pod(t, "web-1", "demo", "node-a", "Running"), &onNodeA); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(pod(t, "web-2", "demo", "node-b", "Failed"), &onNodeB); err != nil {
		t.Fatal(err)
	}
	unusable := json.RawMessage(`not json at all`)

	for _, tc := range []struct {
		sel        string
		wantA      bool
		wantB      bool
		wantReason string
	}{
		{"spec.nodeName=node-a", true, false, "spec path"},
		{"status.phase=Failed", false, true, "status path"},
		{"metadata.name=web-1", true, false, "metadata path"},
	} {
		fs := mustFieldSelector(t, "", "pods", tc.sel)
		if got := fs.Matches(unusable, &onNodeA); got != tc.wantA {
			t.Errorf("[%s] web-1 (%s) = %v, want %v", tc.sel, tc.wantReason, got, tc.wantA)
		}
		if got := fs.Matches(unusable, &onNodeB); got != tc.wantB {
			t.Errorf("[%s] web-2 (%s) = %v, want %v", tc.sel, tc.wantReason, got, tc.wantB)
		}
	}
}

// TestFieldSelector_RawPathWithDecodedObject is the mirror image: a selector on a
// field outside metadata/spec/status must read the raw object even when a decoded
// K8sObject is available, since the struct simply has nowhere to hold it.
func TestFieldSelector_RawPathWithDecodedObject(t *testing.T) {
	event := obj(t, map[string]any{
		"metadata":       map[string]any{"name": "e1", "namespace": "demo"},
		"involvedObject": map[string]any{"kind": "Pod", "name": "web-1"},
		"reason":         "Scheduled",
		"type":           "Normal",
		"source":         map[string]any{"component": "default-scheduler"},
	})
	var decoded K8sObject
	if err := json.Unmarshal(event, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, sel := range []string{
		"involvedObject.kind=Pod", "reason=Scheduled", "type=Normal", "source=default-scheduler",
	} {
		fs := mustFieldSelector(t, "", "events", sel)
		if !fs.Matches(event, &decoded) {
			t.Errorf("[%s] should match even with a decoded object supplied", sel)
		}
	}
	// And a non-matching value still filters.
	fs := mustFieldSelector(t, "", "events", "reason=Killing")
	if fs.Matches(event, &decoded) {
		t.Error("reason=Killing should not match a Scheduled event")
	}

	// Secrets keep type at the top level too.
	secret := obj(t, map[string]any{
		"metadata": map[string]any{"name": "s1", "namespace": "demo"},
		"type":     "kubernetes.io/service-account-token",
	})
	var decodedSecret K8sObject
	if err := json.Unmarshal(secret, &decodedSecret); err != nil {
		t.Fatal(err)
	}
	fs = mustFieldSelector(t, "", "secrets", "type=kubernetes.io/service-account-token")
	if !fs.Matches(secret, &decodedSecret) {
		t.Error("secret type should match with a decoded object supplied")
	}
}

// TestFieldSelector_NeedsFullObject drives the Table fallback decision.
func TestFieldSelector_NeedsFullObject(t *testing.T) {
	cases := []struct {
		sel  string
		want bool
	}{
		{"metadata.name=web-1", false},
		{"metadata.namespace=demo", false},
		{"metadata.name=web-1,metadata.namespace=demo", false},
		{"spec.nodeName=node-a", true},
		{"status.phase=Running", true},
		{"metadata.name=web-1,spec.nodeName=node-a", true},
		// Accepted but not selectable still needs the full object: it is not a
		// metadata key, and resolving it to "" is what makes it match nothing.
		{"status.podIPs=10.0.0.1", true},
	}
	for _, tc := range cases {
		fs := mustFieldSelector(t, "", "pods", tc.sel)
		if got := fs.NeedsFullObject(); got != tc.want {
			t.Errorf("[%s] NeedsFullObject = %v, want %v", tc.sel, got, tc.want)
		}
	}
	if (*FieldSelector)(nil).NeedsFullObject() {
		t.Error("a nil selector should not need the full object")
	}
}

// TestFilterItemsStrict_LabelSelectorOnly covers deletecollection with a
// labelSelector and no fieldSelector, which passes a nil *FieldSelector through
// to Restricts and Matches. Both are nil-safe by design; this keeps them so.
func TestFilterItemsStrict_LabelSelectorOnly(t *testing.T) {
	withLabels := func(name, app string) json.RawMessage {
		return obj(t, map[string]any{
			"metadata": map[string]any{
				"name": name, "namespace": "demo",
				"labels": map[string]any{"app": app},
			},
			"spec": map[string]any{"nodeName": "node-a"},
		})
	}
	items := []json.RawMessage{withLabels("web-1", "web"), withLabels("db-1", "db")}

	msg, filtered := FilterItemsStrict(items, "app=web", "", nil)
	if msg != "" {
		t.Fatalf("label-only deletecollection rejected: %q", msg)
	}
	if len(filtered) != 1 {
		t.Fatalf("filtered %d items, want 1", len(filtered))
	}
	var o K8sObject
	if err := json.Unmarshal(filtered[0], &o); err != nil {
		t.Fatal(err)
	}
	if o.Metadata.Name != "web-1" {
		t.Errorf("kept %q, want web-1", o.Metadata.Name)
	}

	// Neither selector supplied: items pass through untouched.
	msg, filtered = FilterItemsStrict(items, "", "", nil)
	if msg != "" || len(filtered) != 2 {
		t.Errorf("no selectors: msg=%q, %d items; want no message and 2 items", msg, len(filtered))
	}
}

// TestFieldSelector_NilReceiverIsSafe pins the contract the call sites rely on:
// a nil *FieldSelector means "no field selector", and every method tolerates it
// rather than panicking. Go permits calling a pointer-receiver method on a nil
// pointer; these methods check the receiver before dereferencing it.
func TestFieldSelector_NilReceiverIsSafe(t *testing.T) {
	var fs *FieldSelector
	if !fs.Matches(pod(t, "web", "demo", "node-a", "Running"), nil) {
		t.Error("nil.Matches should match everything")
	}
	if fs.Restricts() {
		t.Error("nil.Restricts should be false")
	}
	if fs.NeedsFullObject() {
		t.Error("nil.NeedsFullObject should be false")
	}
	if fs.String() != "" {
		t.Errorf("nil.String = %q, want empty", fs.String())
	}
}

// TestFilterItemsStrict_FieldSelector covers deletecollection's path: the
// per-kind contract now applies (so pods' spec.nodeName is accepted, where it
// used to be rejected), while a selector that restricts nothing is still
// refused.
func TestFilterItemsStrict_FieldSelector(t *testing.T) {
	items := []json.RawMessage{
		pod(t, "web-1", "demo", "node-a", "Running"),
		pod(t, "web-2", "demo", "node-b", "Running"),
	}

	fs := mustFieldSelector(t, "", "pods", "spec.nodeName=node-a")
	msg, filtered := FilterItemsStrict(items, "", "spec.nodeName=node-a", fs)
	if msg != "" {
		t.Fatalf("spec.nodeName should be accepted for pods, got %q", msg)
	}
	if got := len(filtered); got != 1 {
		t.Errorf("filtered %d items, want 1", got)
	}

	// Zero requirements must not degrade to "delete everything".
	commaOnly := mustFieldSelector(t, "", "pods", ",")
	msg, filtered = FilterItemsStrict(items, "", ",", commaOnly)
	if msg == "" {
		t.Error("a selector that restricts nothing should be refused on the write path")
	}
	if filtered != nil {
		t.Error("a refused selector should return no items")
	}
}
