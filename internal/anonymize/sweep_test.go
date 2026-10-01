package anonymize

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/phenixblue/k8shark/internal/capture"
)

func trackerWith(cat Category, alias func(string) string, originals ...string) *collisionTracker {
	t := newCollisionTracker(cat, alias)
	for _, o := range originals {
		t.Alias(o)
	}
	return t
}

func emptyTracker(cat Category) *collisionTracker {
	return newCollisionTracker(cat, upper)
}

func TestBuildSweepCandidates_BasicRouting(t *testing.T) {
	ns := trackerWith(CategoryNamespace, upper, "production")
	node := emptyTracker(CategoryNode)
	pod := emptyTracker(CategoryPod)
	workload := emptyTracker(CategoryWorkload)
	ip := trackerWith(CategoryIP, upper, "10.1.2.3")
	url := trackerWith(CategoryURL, upper, "webhook-svc.default.svc")

	cs, err := buildSweepCandidates(ns, node, pod, workload, ip, url)
	if err != nil {
		t.Fatal(err)
	}

	if cs.nameGroup == nil {
		t.Fatal("want a non-nil nameGroup for the namespace/url candidates")
	}
	if cs.ipGroup == nil {
		t.Fatal("want a non-nil ipGroup for the IP candidate")
	}
	if _, ok := cs.nameGroup.byOriginal["production"]; !ok {
		t.Error("namespace candidate missing from nameGroup")
	}
	if _, ok := cs.nameGroup.byOriginal["webhook-svc.default.svc"]; !ok {
		t.Error("hostname candidate missing from nameGroup")
	}
	if _, ok := cs.ipGroup.byOriginal["10.1.2.3"]; !ok {
		t.Error("IP candidate missing from ipGroup")
	}
	if cs.ambiguousSkipped != 0 {
		t.Errorf("ambiguousSkipped = %d, want 0", cs.ambiguousSkipped)
	}
}

// A URL-category candidate that happens to be a bare IP literal (a URL's
// host position can be an IP — see Result.HostsRenamed's own doc comment)
// must route into the IP-boundary group, not the name/host group, even
// though it came from the URL tracker.
func TestBuildSweepCandidates_URLCandidateThatIsAnIPRoutesToIPGroup(t *testing.T) {
	ns := emptyTracker(CategoryNamespace)
	node := emptyTracker(CategoryNode)
	pod := emptyTracker(CategoryPod)
	workload := emptyTracker(CategoryWorkload)
	ip := emptyTracker(CategoryIP)
	url := trackerWith(CategoryURL, upper, "10.1.2.3")

	cs, err := buildSweepCandidates(ns, node, pod, workload, ip, url)
	if err != nil {
		t.Fatal(err)
	}

	if cs.nameGroup != nil {
		t.Error("want nameGroup nil — the only URL candidate is IP-shaped and should route to ipGroup")
	}
	if cs.ipGroup == nil {
		t.Fatal("want a non-nil ipGroup")
	}
	if cand, ok := cs.ipGroup.byOriginal["10.1.2.3"]; !ok || cand.Category != CategoryURL {
		t.Errorf("ipGroup candidate = %+v, ok=%v, want Category=CategoryURL", cand, ok)
	}
}

// A namespace named "prod" and a pod also named "prod" produce two
// different aliases (the category prefix guarantees this) for the same
// literal original value — genuinely ambiguous for a bare mention in free
// text, so it must be excluded from the sweep entirely rather than guessed
// at.
func TestBuildSweepCandidates_CrossCategoryAmbiguityExcluded(t *testing.T) {
	// Distinct alias functions, mirroring how the real Aliaser always
	// prefixes by category (aliasName) — using the same alias function for
	// both trackers would produce identical aliases and defeat the very
	// ambiguity this test means to exercise.
	nsAlias := func(s string) string { return "namespace-" + s }
	podAlias := func(s string) string { return "pod-" + s }
	ns := trackerWith(CategoryNamespace, nsAlias, "prod")
	node := emptyTracker(CategoryNode)
	pod := trackerWith(CategoryPod, podAlias, "prod")
	workload := emptyTracker(CategoryWorkload)
	ip := emptyTracker(CategoryIP)
	url := emptyTracker(CategoryURL)

	cs, err := buildSweepCandidates(ns, node, pod, workload, ip, url)
	if err != nil {
		t.Fatal(err)
	}

	if cs.nameGroup != nil {
		t.Fatal("want nameGroup nil — the only candidate is ambiguous and should be fully excluded")
	}
	if cs.ambiguousSkipped != 1 {
		t.Errorf("ambiguousSkipped = %d, want 1", cs.ambiguousSkipped)
	}
}

func TestBuildSweepCandidates_ShortCandidatesFiltered(t *testing.T) {
	ns := trackerWith(CategoryNamespace, upper, "ab") // below minSweepCandidateLength
	node := emptyTracker(CategoryNode)
	pod := emptyTracker(CategoryPod)
	workload := emptyTracker(CategoryWorkload)
	ip := emptyTracker(CategoryIP)
	url := emptyTracker(CategoryURL)

	cs, err := buildSweepCandidates(ns, node, pod, workload, ip, url)
	if err != nil {
		t.Fatal(err)
	}

	if cs.nameGroup != nil {
		t.Error("want nameGroup nil — the only candidate is below minSweepCandidateLength")
	}
}

func TestSpliceCandidates_LongestCandidatePreferred(t *testing.T) {
	cands := []sweepCandidate{
		{Category: CategoryPod, Original: "web", Alias: "pod-alias"},
		{Category: CategoryWorkload, Original: "web-1", Alias: "workload-alias"},
	}
	group, err := buildCandidateGroup(cands, nameBoundaryReject)
	if err != nil {
		t.Fatal(err)
	}

	out, changed, n := spliceCandidates("connecting to web-1 now", group, noExclusions, "Event", "message")
	if !changed || n != 1 {
		t.Fatalf("changed=%v n=%d, want changed=true n=1", changed, n)
	}
	if out != "connecting to workload-alias now" {
		t.Errorf("out = %q, want the full web-1 match replaced by workload-alias, not a truncated web match", out)
	}
}

func TestSpliceCandidates_NameBoundaryAllowsFQDNEmbedding(t *testing.T) {
	cands := []sweepCandidate{{Category: CategoryNamespace, Original: "prod", Alias: "namespace-quiet-otter-fox"}}
	group, err := buildCandidateGroup(cands, nameBoundaryReject)
	if err != nil {
		t.Fatal(err)
	}

	out, changed, n := spliceCandidates("svc.prod.svc.cluster.local", group, noExclusions, "Pod", "status.message")
	if !changed || n != 1 {
		t.Fatalf("changed=%v n=%d, want changed=true n=1", changed, n)
	}
	if out != "svc.namespace-quiet-otter-fox.svc.cluster.local" {
		t.Errorf("out = %q, want the namespace segment aliased inside the FQDN", out)
	}
}

func TestSpliceCandidates_NameBoundaryRejectsAlnumAdjacency(t *testing.T) {
	cands := []sweepCandidate{{Category: CategoryNamespace, Original: "prod", Alias: "ALIASED"}}
	group, err := buildCandidateGroup(cands, nameBoundaryReject)
	if err != nil {
		t.Fatal(err)
	}

	out, changed, n := spliceCandidates("this is unrelated-prodcuction text", group, noExclusions, "Pod", "status.message")
	if changed || n != 0 {
		t.Errorf("changed=%v n=%d out=%q, want no match — \"prod\" here is a substring of \"production\", alnum-adjacent on both sides", changed, n, out)
	}
}

// The motivating case for ipBoundaryReject's stricter charset: a short IPv6
// literal must not match inside an unrelated, longer address.
func TestSpliceCandidates_IPBoundaryRejectsWithinLongerIPv6Address(t *testing.T) {
	cands := []sweepCandidate{{Category: CategoryIP, Original: "::1", Alias: "ALIASED"}}
	group, err := buildCandidateGroup(cands, ipBoundaryReject)
	if err != nil {
		t.Fatal(err)
	}

	out, changed, n := spliceCandidates("connecting to fe80::1 on the link", group, noExclusions, "Event", "message")
	if changed || n != 0 {
		t.Errorf("changed=%v n=%d out=%q, want no match — \"::1\" is a colon-adjacent fragment of \"fe80::1\", not the loopback address", changed, n, out)
	}
}

func TestSpliceCandidates_IPBoundaryAcceptsGenuineOccurrence(t *testing.T) {
	cands := []sweepCandidate{{Category: CategoryIP, Original: "10.1.2.3", Alias: "10.99.99.99"}}
	group, err := buildCandidateGroup(cands, ipBoundaryReject)
	if err != nil {
		t.Fatal(err)
	}

	out, changed, n := spliceCandidates("Pulling image failed, dialing 10.1.2.3 timed out", group, noExclusions, "Event", "message")
	if !changed || n != 1 {
		t.Fatalf("changed=%v n=%d, want changed=true n=1", changed, n)
	}
	if out != "Pulling image failed, dialing 10.99.99.99 timed out" {
		t.Errorf("out = %q", out)
	}
}

func TestSpliceCandidates_IPBoundaryRejectsPartialOctetMatch(t *testing.T) {
	cands := []sweepCandidate{{Category: CategoryIP, Original: "10.1.2.3", Alias: "ALIASED"}}
	group, err := buildCandidateGroup(cands, ipBoundaryReject)
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range []string{"110.1.2.3 is unrelated", "10.1.2.34 is unrelated", "10.1.2.3.4 is unrelated"} {
		out, changed, n := spliceCandidates(s, group, noExclusions, "Event", "message")
		if changed || n != 0 {
			t.Errorf("input %q: changed=%v n=%d out=%q, want no match", s, changed, n, out)
		}
	}
}

func TestSpliceCandidates_ExcludeRuleSkipsMatch(t *testing.T) {
	cands := []sweepCandidate{{Category: CategoryNamespace, Original: "prod", Alias: "ALIASED"}}
	group, err := buildCandidateGroup(cands, nameBoundaryReject)
	if err != nil {
		t.Fatal(err)
	}
	excluded := func(cat Category, kind, path string) bool {
		return cat == CategoryNamespace && kind == "Event" && path == "message"
	}

	out, changed, n := spliceCandidates("Namespace prod is active", group, excluded, "Event", "message")
	if changed || n != 0 {
		t.Errorf("changed=%v n=%d out=%q, want the excluded rule to leave this occurrence untouched", changed, n, out)
	}
}

func TestSweepRecord_ListUsesEachItemsOwnKindForExclusion(t *testing.T) {
	cands := []sweepCandidate{{Category: CategoryNamespace, Original: "prod", Alias: "ALIASED"}}
	group, err := buildCandidateGroup(cands, nameBoundaryReject)
	if err != nil {
		t.Fatal(err)
	}
	cs := &sweepCandidateSet{nameGroup: group}

	body := `{"kind":"EventList","items":[
		{"kind":"Event","message":"Namespace prod is active"},
		{"kind":"Event","message":"another mention of prod here"}
	]}`
	rec := &capture.Record{ResponseBody: json.RawMessage(body)}

	changed, occurrences, err := sweepRecord(rec, cs, noExclusions)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || occurrences != 2 {
		t.Fatalf("changed=%v occurrences=%d, want changed=true occurrences=2", changed, occurrences)
	}

	var out struct {
		Items []struct {
			Message string `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
		t.Fatal(err)
	}
	for i, item := range out.Items {
		if item.Message == "" {
			t.Fatalf("item %d: empty message", i)
		}
		if want := "ALIASED"; !strings.Contains(item.Message, want) {
			t.Errorf("item %d message = %q, want it to contain %q", i, item.Message, want)
		}
	}
}

// Reproduces a real gap found in manual end-to-end testing: a Table/
// TableSchema response's rows[*] describe a real Kind (Node here), but the
// response's own top-level "kind" is always "Table" and each row's embedded
// "object" is always typed PartialObjectMetadata (see
// tableRowKindFromAPIPath's own doc comment) — neither carries the real
// Kind the way a List item's own "kind" field does. Before sweepRecord
// resolved the real Kind from the record's APIPath, every occurrence inside
// a Table response was checked against excluded() as kind="Table", so a
// rule scoped to the real Kind (exactly docs/config.md's own documented
// example, `category: node, kind: Node, fieldPath: metadata.name`) silently
// never matched there — even though the identical rule correctly protects
// that exact field on a plain GET/List response of that Kind.
func TestSweepRecord_TableRowsUseResolvedKindForExclusion(t *testing.T) {
	nodeTracker := trackerWith(CategoryNode, upper, "worker-1")
	cs, err := buildSweepCandidates(
		emptyTracker(CategoryNamespace), nodeTracker, emptyTracker(CategoryPod),
		emptyTracker(CategoryWorkload), emptyTracker(CategoryIP), emptyTracker(CategoryURL),
	)
	if err != nil {
		t.Fatal(err)
	}

	newTableRecord := func() *capture.Record {
		body := `{"kind":"Table","apiVersion":"meta.k8s.io/v1",
			"columnDefinitions":[{"name":"Name","description":"worker-1 is a fine example name"}],
			"rows":[{"cells":["worker-1"],"object":{"kind":"PartialObjectMetadata","apiVersion":"meta.k8s.io/v1",
				"metadata":{"name":"worker-1"}}}]}`
		return &capture.Record{APIPath: "/api/v1/nodes?as=Table", ResponseBody: json.RawMessage(body)}
	}
	parseTable := func(t *testing.T, rec *capture.Record) (cellName, objectName, colDesc string) {
		t.Helper()
		var out struct {
			ColumnDefinitions []struct {
				Description string `json:"description"`
			} `json:"columnDefinitions"`
			Rows []struct {
				Cells  []string `json:"cells"`
				Object struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
				} `json:"object"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(rec.ResponseBody, &out); err != nil {
			t.Fatal(err)
		}
		return out.Rows[0].Cells[0], out.Rows[0].Object.Metadata.Name, out.ColumnDefinitions[0].Description
	}

	t.Run("no exclude rule: both object.metadata.name and cells are swept", func(t *testing.T) {
		rec := newTableRecord()
		changed, occurrences, err := sweepRecord(rec, cs, noExclusions)
		if err != nil {
			t.Fatal(err)
		}
		if !changed || occurrences == 0 {
			t.Fatalf("changed=%v occurrences=%d, want changed=true occurrences>0", changed, occurrences)
		}
		cell, object, _ := parseTable(t, rec)
		if cell != "worker-1-ALIASED" || object != "worker-1-ALIASED" {
			t.Errorf("cell=%q object=%q, want both aliased", cell, object)
		}
	})

	t.Run("exclude rule scoped to the real Kind protects object.metadata.name, not cells", func(t *testing.T) {
		rec := newTableRecord()
		excluded := func(cat Category, kind, path string) bool {
			return cat == CategoryNode && kind == "Node" && path == "metadata.name"
		}
		if _, _, err := sweepRecord(rec, cs, excluded); err != nil {
			t.Fatal(err)
		}
		cell, object, _ := parseTable(t, rec)
		if object != "worker-1" {
			t.Errorf("object.metadata.name = %q, want the real value left untouched by the Kind-scoped exclude rule", object)
		}
		if cell != "worker-1-ALIASED" {
			t.Errorf("cells[0] = %q, want it still aliased — the exclude rule only covers fieldPath metadata.name, not cells[*]", cell)
		}
	})

	t.Run("an additional cells[*] exclude rule protects the printed cell too", func(t *testing.T) {
		rec := newTableRecord()
		excluded := func(cat Category, kind, path string) bool {
			return cat == CategoryNode && kind == "Node" && (path == "metadata.name" || path == "cells[*]")
		}
		if _, _, err := sweepRecord(rec, cs, excluded); err != nil {
			t.Fatal(err)
		}
		cell, object, _ := parseTable(t, rec)
		if object != "worker-1" || cell != "worker-1" {
			t.Errorf("cell=%q object=%q, want both left untouched", cell, object)
		}
	})

	t.Run("columnDefinitions and other Table-wrapper content are still swept under kind=Table", func(t *testing.T) {
		rec := newTableRecord()
		excluded := func(cat Category, kind, path string) bool {
			// A rule scoped to kind=Table (the wrapper's own kind, as
			// opposed to kind=Node) must still apply to columnDefinitions
			// — unaffected by the rows[*]-specific Kind resolution above.
			return cat == CategoryNode && kind == "Table" && path == "columnDefinitions[*].description"
		}
		if _, _, err := sweepRecord(rec, cs, excluded); err != nil {
			t.Fatal(err)
		}
		_, _, colDesc := parseTable(t, rec)
		if colDesc != "worker-1 is a fine example name" {
			t.Errorf("columnDefinitions[0].description = %q, want it left untouched by the Table-kind-scoped exclude rule", colDesc)
		}
	})

	t.Run("a resource type outside resourceTypeKind's set falls back to kind=Table for its rows too", func(t *testing.T) {
		rec := newTableRecord()
		rec.APIPath = "/apis/portworx.io/v1/storagenodes?as=Table" // not in resourceTypeKind
		var excludedCalls []string
		excluded := func(cat Category, kind, path string) bool {
			excludedCalls = append(excludedCalls, kind+"/"+path)
			return false
		}
		if _, _, err := sweepRecord(rec, cs, excluded); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range excludedCalls {
			if c == "Table/metadata.name" {
				found = true
			}
		}
		if !found {
			t.Errorf("excluded() calls = %v, want a Table/metadata.name call for the unmapped resource type's row", excludedCalls)
		}
	})
}

func TestSweepRecord_NilCandidatesIsNoOp(t *testing.T) {
	rec := &capture.Record{ResponseBody: json.RawMessage(`{"kind":"Event","message":"Namespace prod is active"}`)}
	orig := string(rec.ResponseBody)

	changed, occurrences, err := sweepRecord(rec, nil, noExclusions)
	if err != nil {
		t.Fatal(err)
	}
	if changed || occurrences != 0 {
		t.Fatalf("changed=%v occurrences=%d, want changed=false occurrences=0", changed, occurrences)
	}
	if string(rec.ResponseBody) != orig {
		t.Error("body must be byte-identical when nothing was swept")
	}
}
