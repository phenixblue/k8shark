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

	cs := buildSweepCandidates(ns, node, pod, workload, ip, url)

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

	cs := buildSweepCandidates(ns, node, pod, workload, ip, url)

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

	cs := buildSweepCandidates(ns, node, pod, workload, ip, url)

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

	cs := buildSweepCandidates(ns, node, pod, workload, ip, url)

	if cs.nameGroup != nil {
		t.Error("want nameGroup nil — the only candidate is below minSweepCandidateLength")
	}
}

func TestSpliceCandidates_LongestCandidatePreferred(t *testing.T) {
	cands := []sweepCandidate{
		{Category: CategoryPod, Original: "web", Alias: "pod-alias"},
		{Category: CategoryWorkload, Original: "web-1", Alias: "workload-alias"},
	}
	group := buildCandidateGroup(cands, nameBoundaryReject)

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
	group := buildCandidateGroup(cands, nameBoundaryReject)

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
	group := buildCandidateGroup(cands, nameBoundaryReject)

	out, changed, n := spliceCandidates("this is unrelated-prodcuction text", group, noExclusions, "Pod", "status.message")
	if changed || n != 0 {
		t.Errorf("changed=%v n=%d out=%q, want no match — \"prod\" here is a substring of \"production\", alnum-adjacent on both sides", changed, n, out)
	}
}

// The motivating case for ipBoundaryReject's stricter charset: a short IPv6
// literal must not match inside an unrelated, longer address.
func TestSpliceCandidates_IPBoundaryRejectsWithinLongerIPv6Address(t *testing.T) {
	cands := []sweepCandidate{{Category: CategoryIP, Original: "::1", Alias: "ALIASED"}}
	group := buildCandidateGroup(cands, ipBoundaryReject)

	out, changed, n := spliceCandidates("connecting to fe80::1 on the link", group, noExclusions, "Event", "message")
	if changed || n != 0 {
		t.Errorf("changed=%v n=%d out=%q, want no match — \"::1\" is a colon-adjacent fragment of \"fe80::1\", not the loopback address", changed, n, out)
	}
}

func TestSpliceCandidates_IPBoundaryAcceptsGenuineOccurrence(t *testing.T) {
	cands := []sweepCandidate{{Category: CategoryIP, Original: "10.1.2.3", Alias: "10.99.99.99"}}
	group := buildCandidateGroup(cands, ipBoundaryReject)

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
	group := buildCandidateGroup(cands, ipBoundaryReject)

	for _, s := range []string{"110.1.2.3 is unrelated", "10.1.2.34 is unrelated", "10.1.2.3.4 is unrelated"} {
		out, changed, n := spliceCandidates(s, group, noExclusions, "Event", "message")
		if changed || n != 0 {
			t.Errorf("input %q: changed=%v n=%d out=%q, want no match", s, changed, n, out)
		}
	}
}

func TestSpliceCandidates_ExcludeRuleSkipsMatch(t *testing.T) {
	cands := []sweepCandidate{{Category: CategoryNamespace, Original: "prod", Alias: "ALIASED"}}
	group := buildCandidateGroup(cands, nameBoundaryReject)
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
	cs := &sweepCandidateSet{nameGroup: buildCandidateGroup(cands, nameBoundaryReject)}

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
