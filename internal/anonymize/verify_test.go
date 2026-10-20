package anonymize

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/phenixblue/k8shark/internal/capture"
	"github.com/phenixblue/k8shark/internal/config"
)

func runVerify(t *testing.T, records []*capture.Record, opts Options) Result {
	t.Helper()
	src := buildAnonymizeTestArchive(t, records)
	dst := filepath.Join(t.TempDir(), "out.kshrk")
	opts.Salt = []byte("verify-test-salt")
	opts.Verify = true
	res, err := Archive(src, dst, opts)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	return res
}

// The safety check's whole point: after a complete sweep it must report a
// clean bill, and it must do so on the same fixture that exposed every
// residual-leak form (so a "clean" report isn't just a blind check).
func TestVerify_CleanAfterFullSweep(t *testing.T) {
	res := runVerify(t, residualLeakFixture(), Options{Categories: allCategories, FullSweep: true})
	if !res.Verified {
		t.Fatal("Verified = false, want true")
	}
	if res.ResidualOccurrences != 0 || len(res.ResidualLocations) != 0 {
		t.Errorf("residual = %d occurrences %+v, want a clean result", res.ResidualOccurrences, res.ResidualLocations)
	}
	if res.ResidualLocations == nil {
		t.Error("ResidualLocations must be a non-nil (empty) slice so -o json always emits an array")
	}
}

// Without a sweep the same fixture is full of surviving originals; the check
// must find them, say where, and never repeat an original in doing so.
func TestVerify_FlagsLeaksWithoutSweepAndNeverEchoesOriginals(t *testing.T) {
	res := runVerify(t, residualLeakFixture(), Options{Categories: allCategories})
	if res.ResidualOccurrences == 0 || len(res.ResidualLocations) == 0 {
		t.Fatalf("residual = %d / %d locations, want leaks reported without --full-sweep", res.ResidualOccurrences, len(res.ResidualLocations))
	}
	sum := 0
	paths := map[string]bool{}
	for _, l := range res.ResidualLocations {
		sum += l.Occurrences
		paths[l.FieldPath] = true
	}
	if sum != res.ResidualOccurrences {
		t.Errorf("location occurrences sum to %d, total is %d (fixture is small enough for no truncation)", sum, res.ResidualOccurrences)
	}
	for _, want := range []string{"metadata.labels.app", "message"} {
		found := false
		for p := range paths {
			if strings.HasSuffix(p, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no location for a field ending %q; got %v", want, paths)
		}
	}

	out, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range residualLeakForms {
		// The "bare cell" form is just `"api"`, which also legitimately
		// appears as a JSON string elsewhere in the report; the other forms
		// are specific enough to prove nothing was echoed.
		if l.form == `"api"` {
			continue
		}
		if strings.Contains(string(out), l.form) {
			t.Errorf("the report contains an original (%s: %q): %s", l.what, l.form, out)
		}
	}
}

// A path an exclude rule covers is a deliberate keep, not a leak.
func TestVerify_ExcludedOccurrencesAreNotLeaks(t *testing.T) {
	records := []*capture.Record{{
		ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/configmaps/c", HTTPMethod: "GET", ResponseCode: 200,
		ResponseBody: json.RawMessage(`{"kind":"ConfigMap","apiVersion":"v1","metadata":{"name":"c","namespace":"prod"},"data":{"note":"keep prod visible here"}}`),
	}}
	base := Options{Categories: []Category{CategoryNamespace}}

	res := runVerify(t, records, base)
	if res.ResidualOccurrences != 1 {
		t.Fatalf("baseline: residual = %d, want 1 (the free-text mention)", res.ResidualOccurrences)
	}

	withRule := base
	withRule.Rules = []config.AnonymizeRule{{Category: "namespace", Kind: "ConfigMap", FieldPath: "data.note", Exclude: true}}
	res = runVerify(t, records, withRule)
	if res.ResidualOccurrences != 0 {
		t.Errorf("with an exclude rule on data.note: residual = %d, want 0", res.ResidualOccurrences)
	}
}

// A value that is a candidate under two categories can't be swept (which
// alias?), but it is still an original that survived — the check must say so.
func TestVerify_FlagsAmbiguousValuesTheSweepSkips(t *testing.T) {
	records := []*capture.Record{
		{ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/shared", HTTPMethod: "GET", ResponseCode: 200,
			ResponseBody: json.RawMessage(`{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"shared"}}`)},
		{ID: "r2", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/shared/pods/shared", HTTPMethod: "GET", ResponseCode: 200,
			ResponseBody: json.RawMessage(`{"kind":"Pod","apiVersion":"v1","metadata":{"name":"shared","namespace":"shared"},"status":{"message":"pod shared in namespace shared"}}`)},
	}
	res := runVerify(t, records, Options{Categories: []Category{CategoryNamespace, CategoryPod}, FullSweep: true})
	if res.SweepAmbiguousSkipped == 0 {
		t.Fatal("fixture should have an ambiguous value")
	}
	if res.ResidualOccurrences == 0 {
		t.Error("the unsweepable ambiguous value survives in status.message; the check must report it")
	}
}

// Image registries aren't swept as free text, but one surviving where the
// image category's own matcher doesn't look is still a leak.
func TestVerify_CoversImageRegistries(t *testing.T) {
	records := []*capture.Record{{
		ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/ns/pods/p", HTTPMethod: "GET", ResponseCode: 200,
		ResponseBody: json.RawMessage(`{"kind":"Pod","apiVersion":"v1","metadata":{"name":"p","namespace":"ns"},
			"spec":{"containers":[{"name":"a","image":"registry.internal.corp/team/app:v1"}]},
			"metadata2":{"mirror":"registry.internal.corp/team/app:v1"}}`),
	}}
	res := runVerify(t, records, Options{Categories: []Category{CategoryImage}})
	if res.ResidualOccurrences == 0 {
		t.Error("an image registry left outside a recognized image field must be reported")
	}
}

// The archive's own server_address is checked, and its host is a known
// original for the rest of the archive (so the sweep catches it elsewhere).
func TestVerify_ServerAddressHostIsAKnownOriginal(t *testing.T) {
	records := []*capture.Record{{
		ID: "r1", CapturedAt: fixedNow, APIPath: "/api", HTTPMethod: "GET", ResponseCode: 200,
		ResponseBody: json.RawMessage(`{"kind":"APIVersions","note":"reachable at 127.0.0.1:6443 from the host"}`),
	}}
	// buildAnonymizeTestArchive's metadata has server_address https://127.0.0.1:6443.
	swept := runVerify(t, records, Options{Categories: []Category{CategoryURL}, FullSweep: true})
	if swept.ResidualOccurrences != 0 {
		t.Errorf("with a sweep: residual = %d %+v, want 0", swept.ResidualOccurrences, swept.ResidualLocations)
	}
	unswept := runVerify(t, records, Options{Categories: []Category{CategoryURL}})
	if unswept.ResidualOccurrences == 0 {
		t.Error("without a sweep, the server host surviving in a record's text must be reported")
	}
}

// Verification happens on the in-flight record, so it works when the output
// is encrypted to a recipient this process can't decrypt.
func TestVerify_WorksWithEncryptedOutput(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	res := runVerify(t, residualLeakFixture(), Options{Categories: allCategories, FullSweep: true, Recipients: []age.Recipient{id.Recipient()}})
	if !res.Verified || res.ResidualOccurrences != 0 {
		t.Errorf("Verified=%v residual=%d, want true/0", res.Verified, res.ResidualOccurrences)
	}
}

func TestVerify_OffByDefaultForLibraryCallers(t *testing.T) {
	src := buildAnonymizeTestArchive(t, residualLeakFixture())
	res, err := Archive(src, filepath.Join(t.TempDir(), "o.kshrk"), Options{Categories: allCategories, Salt: []byte("s")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified || res.ResidualOccurrences != 0 || len(res.ResidualLocations) != 0 {
		t.Errorf("Verified=%v residual=%d, want an unverified zero result when Options.Verify is false", res.Verified, res.ResidualOccurrences)
	}
}

// A key that still embeds an original must not put that original in the
// report via its field path.
func TestVerify_FieldPathNeverContainsAnOriginal(t *testing.T) {
	records := []*capture.Record{{
		ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/prod/pods/p", HTTPMethod: "GET", ResponseCode: 200,
		ResponseBody: json.RawMessage(`{"kind":"Pod","apiVersion":"v1","metadata":{"name":"p","namespace":"prod",
			"annotations":{"owner.prod-team.example/contact":"prod-oncall"}}}`),
	}}
	res := runVerify(t, records, Options{Categories: []Category{CategoryNamespace}})
	out, _ := json.Marshal(res)
	if res.ResidualOccurrences == 0 {
		t.Fatal("fixture should leak (annotation key and value mention the namespace)")
	}
	if strings.Contains(string(out), "prod-team") || strings.Contains(string(out), "prod-oncall") || strings.Contains(string(out), `"prod"`) {
		t.Errorf("report contains an original: %s", out)
	}
}

// managedFields identifies list items by value ("k:{...}"); each pod IP or
// container name would otherwise be its own line in the report.
func TestVerify_ManagedFieldsKeysCollapseIntoOneLocation(t *testing.T) {
	records := []*capture.Record{{
		ID: "r1", CapturedAt: fixedNow, APIPath: "/api/v1/namespaces/ns/pods/p", HTTPMethod: "GET", ResponseCode: 200,
		ResponseBody: json.RawMessage(`{"kind":"Pod","apiVersion":"v1","metadata":{"name":"p","namespace":"ns","managedFields":[{"fieldsV1":{
			"f:status":{"f:podIPs":{"k:{\"ip\":\"10.244.0.20\"}":{},"k:{\"ip\":\"10.244.0.21\"}":{}}}}}]},
			"status":{"podIP":"10.244.0.20","podIPs":[{"ip":"10.244.0.20"},{"ip":"10.244.0.21"}]}}`),
	}}
	res := runVerify(t, records, Options{Categories: []Category{CategoryIP}})
	var mf []ResidualLocation
	for _, l := range res.ResidualLocations {
		if strings.Contains(l.FieldPath, "managedFields") {
			mf = append(mf, l)
		}
	}
	if len(mf) != 1 || mf[0].Occurrences != 2 || !strings.HasSuffix(mf[0].FieldPath, "f:podIPs.k:{*}") {
		t.Errorf("managedFields locations = %+v, want one collapsed k:{*} entry with 2 occurrences", mf)
	}
}
