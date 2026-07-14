package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// tblResp is the subset of a meta.k8s.io/v1 Table we assert on.
type tblResp struct {
	Kind              string `json:"kind"`
	ColumnDefinitions []struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Priority int    `json:"priority"`
		Format   string `json:"format"`
	} `json:"columnDefinitions"`
	Rows []struct {
		Cells  []any           `json:"cells"`
		Object json.RawMessage `json:"object"`
	} `json:"rows"`
}

func decodeTable(t *testing.T, b []byte) tblResp {
	t.Helper()
	var r tblResp
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("decode table: %v\n%s", err, b)
	}
	if r.Kind != "Table" {
		t.Fatalf("kind = %q, want Table\n%s", r.Kind, b)
	}
	return r
}

// renderOne renders path/body through renderResourceTable and returns, for the
// single resulting row, a name→cell-value map and a name→priority map.
func renderOne(t *testing.T, path, body string) (map[string]any, map[string]int) {
	t.Helper()
	return renderOneAt(t, path, body, time.Time{})
}

func renderOneAt(t *testing.T, path, body string, at time.Time) (map[string]any, map[string]int) {
	t.Helper()
	h := &handler{}
	tb, ok := h.renderResourceTable(path, []byte(body), at)
	if !ok {
		t.Fatalf("renderResourceTable(%s) returned ok=false", path)
	}
	r := decodeTable(t, tb)
	if len(r.Rows) != 1 {
		t.Fatalf("got %d rows, want 1\n%s", len(r.Rows), tb)
	}
	vals := map[string]any{}
	prio := map[string]int{}
	for i, c := range r.ColumnDefinitions {
		vals[c.Name] = r.Rows[0].Cells[i]
		prio[c.Name] = c.Priority
	}
	return vals, prio
}

func wantCell(t *testing.T, vals map[string]any, name, want string) {
	t.Helper()
	if got := fmt.Sprint(vals[name]); got != want {
		t.Errorf("cell %q = %q, want %q", name, got, want)
	}
}

const nodeObj = `{
  "metadata":{"name":"node-1","creationTimestamp":"2026-04-09T10:00:00Z",
    "labels":{"node-role.kubernetes.io/control-plane":""}},
  "spec":{"unschedulable":true},
  "status":{
    "conditions":[{"type":"Ready","status":"True"}],
    "nodeInfo":{"kubeletVersion":"v1.29.0","osImage":"Ubuntu 22.04",
      "kernelVersion":"6.1.0","containerRuntimeVersion":"containerd://1.7.0"},
    "addresses":[{"type":"InternalIP","address":"10.0.0.1"},
      {"type":"ExternalIP","address":"1.2.3.4"}]}}`

func TestNodePrinter(t *testing.T) {
	vals, prio := renderOne(t, "/api/v1/nodes", nodeObj)

	wantCell(t, vals, "Name", "node-1")
	wantCell(t, vals, "Status", "Ready,SchedulingDisabled")
	wantCell(t, vals, "Roles", "control-plane")
	wantCell(t, vals, "Version", "v1.29.0")
	// Wide columns.
	wantCell(t, vals, "Internal-IP", "10.0.0.1")
	wantCell(t, vals, "External-IP", "1.2.3.4")
	wantCell(t, vals, "OS-Image", "Ubuntu 22.04")

	// Default columns have priority 0; wide columns priority 1.
	for _, c := range []string{"Name", "Status", "Roles", "Version", "Age"} {
		if prio[c] != 0 {
			t.Errorf("column %q priority = %d, want 0 (default)", c, prio[c])
		}
	}
	for _, c := range []string{"Internal-IP", "External-IP", "OS-Image", "Kernel-Version", "Container-Runtime"} {
		if prio[c] != 1 {
			t.Errorf("column %q priority = %d, want 1 (wide)", c, prio[c])
		}
	}
}

func TestNodePrinter_ColumnOrder(t *testing.T) {
	h := &handler{}
	tb, ok := h.renderResourceTable("/api/v1/nodes", []byte(nodeObj), time.Time{})
	if !ok {
		t.Fatal("render ok=false")
	}
	r := decodeTable(t, tb)
	var names []string
	for _, c := range r.ColumnDefinitions {
		names = append(names, c.Name)
	}
	// Upstream kubectl order: NAME STATUS ROLES AGE VERSION, then wide columns.
	want := []string{"Name", "Status", "Roles", "Age", "Version",
		"Internal-IP", "External-IP", "OS-Image", "Kernel-Version", "Container-Runtime"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("node columns = %v\nwant %v", names, want)
	}
}

// TestAgeColumn verifies the AGE cell is a relative string computed against the
// replay clock (matching a real apiserver), not a raw timestamp.
func TestAgeColumn(t *testing.T) {
	h := &handler{}
	// creationTimestamp 90 minutes before the replay clock → "90m".
	obj := `{"metadata":{"name":"n","creationTimestamp":"2026-04-09T10:00:00Z"},
      "status":{"conditions":[{"type":"Ready","status":"True"}]}}`
	now := time.Date(2026, 4, 9, 11, 30, 0, 0, time.UTC)
	tb, ok := h.renderResourceTable("/api/v1/nodes", []byte(obj), now)
	if !ok {
		t.Fatal("render ok=false")
	}
	r := decodeTable(t, tb)
	var ageType, ageVal string
	for i, c := range r.ColumnDefinitions {
		if c.Name == "Age" {
			ageType = c.Type
			ageVal = fmt.Sprint(r.Rows[0].Cells[i])
		}
	}
	if ageType != "string" {
		t.Errorf("Age column type = %q, want string (relative age is pre-computed)", ageType)
	}
	if ageVal != "90m" {
		t.Errorf("Age cell = %q, want 90m", ageVal)
	}
}

// TestAgeColumn_FractionalSeconds guards that a creationTimestamp with
// fractional seconds (RFC3339Nano) still renders a relative age — Go's
// time.Parse(time.RFC3339, …) accepts a fractional-second field on input.
func TestAgeColumn_FractionalSeconds(t *testing.T) {
	obj := `{"metadata":{"name":"n","creationTimestamp":"2026-04-09T10:00:00.123456Z"},
      "status":{"conditions":[{"type":"Ready","status":"True"}]}}`
	now := time.Date(2026, 4, 9, 10, 30, 1, 0, time.UTC)
	vals, _ := renderOneAt(t, "/api/v1/nodes", obj, now)
	if got := fmt.Sprint(vals["Age"]); got != "30m" {
		t.Errorf("Age with fractional-second timestamp = %q, want 30m (not <unknown>)", got)
	}
}

const podObj = `{
  "metadata":{"name":"pod-1","namespace":"default","creationTimestamp":"2026-04-09T10:00:00Z"},
  "spec":{"nodeName":"node-1"},
  "status":{"phase":"Running","podIP":"10.244.0.5",
    "containerStatuses":[{"ready":true,"restartCount":2},{"ready":false,"restartCount":0}]}}`

func TestPodPrinter(t *testing.T) {
	vals, prio := renderOne(t, "/api/v1/namespaces/default/pods", podObj)

	wantCell(t, vals, "Name", "pod-1")
	wantCell(t, vals, "Ready", "1/2")
	wantCell(t, vals, "Status", "Running")
	wantCell(t, vals, "Restarts", "2")
	wantCell(t, vals, "IP", "10.244.0.5")
	wantCell(t, vals, "Node", "node-1")

	if prio["IP"] != 1 || prio["Node"] != 1 {
		t.Errorf("IP/Node should be wide (priority 1); got IP=%d Node=%d", prio["IP"], prio["Node"])
	}
	if prio["Ready"] != 0 || prio["Status"] != 0 {
		t.Errorf("Ready/Status should be default (priority 0)")
	}
}

func TestPodStatus_WaitingReasonWins(t *testing.T) {
	body := `{"metadata":{"name":"p"},"status":{"phase":"Running",
      "containerStatuses":[{"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`
	vals, _ := renderOne(t, "/api/v1/namespaces/default/pods", body)
	wantCell(t, vals, "Status", "CrashLoopBackOff")
}

func TestPodStatus_Terminating(t *testing.T) {
	body := `{"metadata":{"name":"p","deletionTimestamp":"2026-04-09T10:05:00Z"},
      "status":{"phase":"Running"}}`
	vals, _ := renderOne(t, "/api/v1/namespaces/default/pods", body)
	wantCell(t, vals, "Status", "Terminating")
}

func TestDeploymentPrinter(t *testing.T) {
	body := `{"metadata":{"name":"web","namespace":"default"},
      "spec":{"replicas":3,"selector":{"matchLabels":{"app":"web"}},
        "template":{"spec":{"containers":[{"name":"c","image":"nginx:1.25"}]}}},
      "status":{"readyReplicas":2,"updatedReplicas":3,"availableReplicas":2}}`
	vals, prio := renderOne(t, "/apis/apps/v1/namespaces/default/deployments", body)
	wantCell(t, vals, "Ready", "2/3")
	wantCell(t, vals, "Up-To-Date", "3")
	wantCell(t, vals, "Available", "2")
	wantCell(t, vals, "Containers", "c")
	wantCell(t, vals, "Images", "nginx:1.25")
	wantCell(t, vals, "Selector", "app=web")
	if prio["Containers"] != 1 || prio["Selector"] != 1 {
		t.Errorf("Containers/Selector should be wide (priority 1)")
	}
}

func TestJobPrinter_Status(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"complete", `{"metadata":{"name":"j"},"status":{"conditions":[{"type":"Complete","status":"True"}]}}`, "Complete"},
		{"failed", `{"metadata":{"name":"j"},"status":{"conditions":[{"type":"Failed","status":"True"}]}}`, "Failed"},
		{"suspended", `{"metadata":{"name":"j"},"spec":{"suspend":true}}`, "Suspended"},
		{"running", `{"metadata":{"name":"j"},"status":{"active":1}}`, "Running"},
	}
	for _, c := range cases {
		vals, _ := renderOne(t, "/apis/batch/v1/namespaces/default/jobs", c.body)
		if got := fmt.Sprint(vals["Status"]); got != c.want {
			t.Errorf("%s: Status = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCronJobPrinter_ContainersFromJobTemplate(t *testing.T) {
	body := `{"metadata":{"name":"cj","namespace":"default"},
      "spec":{"schedule":"*/5 * * * *",
        "jobTemplate":{"spec":{"template":{"spec":{"containers":[
          {"name":"worker","image":"busybox:1.36"}]}}}}}}`
	vals, _ := renderOne(t, "/apis/batch/v1/namespaces/default/cronjobs", body)
	wantCell(t, vals, "Schedule", "*/5 * * * *")
	// -o wide columns must resolve through the nested jobTemplate.
	wantCell(t, vals, "Containers", "worker")
	wantCell(t, vals, "Images", "busybox:1.36")
}

func TestReplicationControllerPrinter_PlainSelector(t *testing.T) {
	// ReplicationController uses a plain map selector (no matchLabels).
	body := `{"metadata":{"name":"rc","namespace":"default"},
      "spec":{"replicas":2,"selector":{"app":"legacy"},
        "template":{"spec":{"containers":[{"name":"c","image":"nginx"}]}}},
      "status":{"replicas":2,"readyReplicas":2}}`
	vals, _ := renderOne(t, "/api/v1/namespaces/default/replicationcontrollers", body)
	wantCell(t, vals, "Desired", "2")
	wantCell(t, vals, "Selector", "app=legacy")
}

func TestServicePrinter_Ports(t *testing.T) {
	body := `{"metadata":{"name":"svc","namespace":"default"},
      "spec":{"type":"NodePort","clusterIP":"10.96.0.1",
        "ports":[{"port":80,"nodePort":30080,"protocol":"TCP"}]}}`
	vals, _ := renderOne(t, "/api/v1/namespaces/default/services", body)
	wantCell(t, vals, "Type", "NodePort")
	wantCell(t, vals, "Cluster-IP", "10.96.0.1")
	wantCell(t, vals, "Port(s)", "80:30080/TCP")
	wantCell(t, vals, "External-IP", "<none>")
}

func TestConfigMapPrinter_DataCount(t *testing.T) {
	body := `{"metadata":{"name":"cm","namespace":"default"},
      "data":{"a":"1","b":"2"},"binaryData":{"c":"AA=="}}`
	vals, _ := renderOne(t, "/api/v1/namespaces/default/configmaps", body)
	wantCell(t, vals, "Data", "3")
}

func TestTableStructure_NameFormatAndObject(t *testing.T) {
	h := &handler{}
	tb, ok := h.renderResourceTable("/api/v1/nodes", []byte(nodeObj), time.Time{})
	if !ok {
		t.Fatal("render returned ok=false")
	}
	r := decodeTable(t, tb)
	if r.ColumnDefinitions[0].Name != "Name" || r.ColumnDefinitions[0].Format != "name" {
		t.Errorf("first column should be Name with format=name; got %+v", r.ColumnDefinitions[0])
	}
	// Each row carries a PartialObjectMetadata object with the source metadata.
	var obj struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(r.Rows[0].Object, &obj); err != nil {
		t.Fatalf("row object: %v", err)
	}
	if obj.Kind != "PartialObjectMetadata" || obj.Metadata.Name != "node-1" {
		t.Errorf("row object = %+v, want PartialObjectMetadata for node-1", obj)
	}
}

// TestSingleObjectPathResolvesPrinter covers `kubectl get <resource> <name>`:
// a single-object path (…/configmaps/demo, …/nodes/n1) must resolve to the same
// built-in printer as the list path, not fall back to generic NAME/AGE.
func TestSingleObjectPathResolvesPrinter(t *testing.T) {
	cases := []struct {
		path string
		body string
		want string // a column that only the built-in printer produces
	}{
		{"/api/v1/namespaces/default/configmaps/demo",
			`{"metadata":{"name":"demo"},"data":{"a":"1"}}`, "Data"},
		{"/api/v1/nodes/n1", nodeObj, "Version"},
		{"/apis/apps/v1/namespaces/default/deployments/web",
			`{"metadata":{"name":"web"},"spec":{"replicas":1},"status":{"readyReplicas":1}}`, "Ready"},
	}
	for _, c := range cases {
		h := &handler{}
		tb, ok := h.renderResourceTable(c.path, []byte(c.body), time.Time{})
		if !ok {
			t.Errorf("%s: ok=false", c.path)
			continue
		}
		r := decodeTable(t, tb)
		found := false
		for _, cd := range r.ColumnDefinitions {
			if cd.Name == c.want {
				found = true
			}
		}
		if !found {
			var names []string
			for _, cd := range r.ColumnDefinitions {
				names = append(names, cd.Name)
			}
			t.Errorf("%s: missing built-in column %q; got %v", c.path, c.want, names)
		}
	}
}

func TestJSONPathCell(t *testing.T) {
	cell := jsonPathCell(".spec.replicas")
	obj := map[string]any{"spec": map[string]any{"replicas": float64(3)}}
	if got := fmt.Sprint(cell(obj)); got != "3" {
		t.Errorf("jsonpath .spec.replicas = %q, want 3", got)
	}
	// Missing key yields empty, not an error/panic.
	if got := fmt.Sprint(jsonPathCell(".status.phase")(obj)); got != "" {
		t.Errorf("missing key should render empty, got %q", got)
	}
}

// TestCRDPrinterColumns renders a custom resource via its CRD's
// additionalPrinterColumns (JSONPath), including a wide (priority 1) column.
func TestCRDPrinterColumns(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	crdList := `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinitionList","items":[
      {"spec":{"group":"example.com","names":{"plural":"widgets"},
        "versions":[{"name":"v1","additionalPrinterColumns":[
          {"name":"Replicas","type":"integer","jsonPath":".spec.replicas"},
          {"name":"Phase","type":"string","jsonPath":".status.phase","priority":1}]}]}}]}`
	store := buildTestStoreWithWatch(t, map[string]watchTestRecord{
		"/apis/apiextensions.k8s.io/v1/customresourcedefinitions": {id: "crd", at: from, body: crdList},
	}, nil)
	h := newHandler(store, time.Time{}, false)

	widgets := `{"items":[{"metadata":{"name":"w1","creationTimestamp":"2026-04-09T10:00:00Z"},
      "spec":{"replicas":3},"status":{"phase":"Ready"}}]}`
	tb, ok := h.renderResourceTable("/apis/example.com/v1/widgets", []byte(widgets), from)
	if !ok {
		t.Fatal("renderResourceTable(widgets) ok=false")
	}
	r := decodeTable(t, tb)

	var names []string
	prio := map[string]int{}
	for _, c := range r.ColumnDefinitions {
		names = append(names, c.Name)
		prio[c.Name] = c.Priority
	}
	// NAME prepended, AGE appended, CRD columns in between.
	if names[0] != "Name" || names[len(names)-1] != "Age" {
		t.Errorf("columns = %v, want Name…Age framing", names)
	}
	if prio["Phase"] != 1 {
		t.Errorf("Phase should be a wide column (priority 1); got %d", prio["Phase"])
	}
	vals := map[string]any{}
	for i, c := range r.ColumnDefinitions {
		vals[c.Name] = r.Rows[0].Cells[i]
	}
	wantCell(t, vals, "Replicas", "3")
	wantCell(t, vals, "Phase", "Ready")
}

// TestCapturedColumns_AgeTypeIsString verifies the captured-columns fallback
// emits the Age column as type string (its cell is a relative-age string), even
// when the captured columnDefinitions declared it as "date".
func TestCapturedColumns_AgeTypeIsString(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	// A kind with no built-in printer and no CRD → falls to captured columns.
	listPath := "/apis/policy/v1/namespaces/default/poddisruptionbudgets"
	capturedTable := `{"kind":"Table","apiVersion":"meta.k8s.io/v1","columnDefinitions":[
      {"name":"Name","type":"string"},{"name":"Min Available","type":"string"},
      {"name":"Age","type":"date"}],"rows":[]}`
	store := buildTestStoreWithWatch(t, map[string]watchTestRecord{
		listPath + "?as=Table": {id: "pdb", at: from, body: capturedTable},
	}, nil)
	h := newHandler(store, time.Time{}, false)

	pdbs := `{"items":[{"metadata":{"name":"pdb1","creationTimestamp":"2026-04-09T09:00:00Z"}}]}`
	now := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	tb, ok := h.renderResourceTable(listPath, []byte(pdbs), now)
	if !ok {
		t.Fatal("render ok=false")
	}
	r := decodeTable(t, tb)
	var ageType, ageVal string
	for i, c := range r.ColumnDefinitions {
		if c.Name == "Age" {
			ageType = c.Type
			ageVal = fmt.Sprint(r.Rows[0].Cells[i])
		}
	}
	if ageType != "string" {
		t.Errorf("captured Age column type = %q, want string", ageType)
	}
	if ageVal != "60m" {
		t.Errorf("captured Age cell = %q, want 60m (relative)", ageVal)
	}
}

// TestGenericFallback_RelativeAge verifies the last-resort generic table (no
// built-in/CRD/captured columns) still emits AGE as a relative string of type
// string, consistent with the computed tiers — not a raw timestamp.
func TestGenericFallback_RelativeAge(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	store := buildTestStoreWithWatch(t, map[string]watchTestRecord{
		podsPath: {id: "s", at: from, body: podList("p")},
	}, nil)
	h := newHandler(store, time.Time{}, false)

	// limitranges has no built-in printer, no CRD, and no captured Table here.
	body := `{"items":[{"metadata":{"name":"lr","namespace":"default","creationTimestamp":"2026-04-09T09:30:00Z"}}]}`
	now := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	tb, ok := h.renderResourceTable("/api/v1/namespaces/default/limitranges", []byte(body), now)
	if !ok {
		t.Fatal("render ok=false")
	}
	r := decodeTable(t, tb)
	var names []string
	var ageType, ageVal string
	for i, c := range r.ColumnDefinitions {
		names = append(names, c.Name)
		if c.Name == "Age" {
			ageType, ageVal = c.Type, fmt.Sprint(r.Rows[0].Cells[i])
		}
	}
	if fmt.Sprint(names) != fmt.Sprint([]string{"Name", "Namespace", "Age"}) {
		t.Errorf("generic columns = %v, want [Name Namespace Age]", names)
	}
	if ageType != "string" || ageVal != "30m" {
		t.Errorf("generic Age = (%q,%q), want (string,30m)", ageType, ageVal)
	}
}

// TestRenderTable_ZeroAtUsesCaptureEnd verifies that in plain `open` mode (at ==
// zero, meaning "latest"), computed-table AGE is relative to the capture's end
// time rather than collapsing to "0s".
func TestRenderTable_ZeroAtUsesCaptureEnd(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	store := buildTestStoreWithWatch(t, map[string]watchTestRecord{
		podsPath: {id: "s", at: from, body: podList("p")},
	}, nil)
	store.Metadata.CapturedUntil = time.Date(2026, 4, 9, 11, 0, 0, 0, time.UTC)
	h := newHandler(store, time.Time{}, false) // open mode, no --at → zero at

	body := `{"items":[{"metadata":{"name":"n1","creationTimestamp":"2026-04-09T10:00:00Z"},
      "status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`
	tb, ok := h.renderResourceTable("/api/v1/nodes", []byte(body), h.at)
	if !ok {
		t.Fatal("render ok=false")
	}
	r := decodeTable(t, tb)
	var ageVal string
	for i, c := range r.ColumnDefinitions {
		if c.Name == "Age" {
			ageVal = fmt.Sprint(r.Rows[0].Cells[i])
		}
	}
	if ageVal != "60m" {
		t.Errorf("open-mode zero-at Age = %q, want 60m (relative to CapturedUntil, not 0s)", ageVal)
	}
}

// getTable issues a kubectl-style Table request and returns the decoded Table.
func getTable(t *testing.T, url string) tblResp {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("table request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("table request status %d: %s", resp.StatusCode, b)
	}
	return decodeTable(t, b)
}

// TestOverlayTable_PodColumns verifies an overlay-created Pod renders the full
// built-in Pod columns (incl. wide priority) via the writable path — not the
// generic NAME/AGE fallback.
func TestOverlayTable_PodColumns(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+podsPath, "application/json", podBody("pod-ov"))

	r := getTable(t, srv.URL+podsPath)
	prio := map[string]int{}
	for _, c := range r.ColumnDefinitions {
		prio[c.Name] = c.Priority
	}
	for _, c := range []string{"Name", "Ready", "Status", "Restarts", "Age"} {
		if _, ok := prio[c]; !ok {
			t.Errorf("Table missing default column %q; got %v", c, prio)
		}
	}
	if prio["IP"] != 1 || prio["Node"] != 1 {
		t.Errorf("wide columns IP/Node missing or wrong priority: %v", prio)
	}
	// The overlay-created object must appear.
	found := false
	for _, row := range r.Rows {
		if len(row.Cells) > 0 && fmt.Sprint(row.Cells[0]) == "pod-ov" {
			found = true
		}
	}
	if !found {
		t.Errorf("overlay pod pod-ov not present in Table rows")
	}
}

// TestOverlayTable_NeverCapturedKind renders a kind absent from the capture
// (nodes) through the built-in printer after an overlay create.
func TestOverlayTable_NeverCapturedKind(t *testing.T) {
	from := time.Date(2026, 4, 9, 10, 0, 0, 0, time.UTC)
	clock, _ := newTestClock(t, from, from.Add(time.Minute), 1, false, false)
	srv := newWritableServer(t, writableTestStore(t, from), clock)

	doReq(t, http.MethodPost, srv.URL+"/api/v1/nodes", "application/json",
		`{"apiVersion":"v1","kind":"Node","metadata":{"name":"n-ov"},
          "status":{"conditions":[{"type":"Ready","status":"True"}],
            "nodeInfo":{"kubeletVersion":"v1.29.0"}}}`)

	r := getTable(t, srv.URL+"/api/v1/nodes")
	names := map[string]bool{}
	for _, c := range r.ColumnDefinitions {
		names[c.Name] = true
	}
	for _, c := range []string{"Status", "Roles", "Version"} {
		if !names[c] {
			t.Errorf("nodes Table missing built-in column %q; got %v", c, names)
		}
	}
}
