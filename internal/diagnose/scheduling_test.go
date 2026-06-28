package diagnose

import "testing"

// Two distinct workloads with the same generic unschedulable message must NOT
// be merged into one finding (regression for the owner-less grouping key).
func TestScheduling_DistinctOwnersNotMerged(t *testing.T) {
	pods := `{"kind":"PodList","apiVersion":"v1","items":[
	  {"metadata":{"name":"a-1","ownerReferences":[{"kind":"ReplicaSet","name":"a-rs"}]},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/3 nodes available: insufficient cpu"}]}},
	  {"metadata":{"name":"b-1","ownerReferences":[{"kind":"ReplicaSet","name":"b-rs"}]},"status":{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/3 nodes available: insufficient cpu"}]}}
	]}`
	store := buildDiagStore(t, map[string]string{"/api/v1/namespaces/x/pods": pods})
	rep := Run(store, Options{Category: "scheduling"})
	if len(rep.Findings) != 2 {
		t.Fatalf("expected 2 separate unschedulable findings, got %d", len(rep.Findings))
	}
}

func TestSeverityAtLeast_UnknownRejected(t *testing.T) {
	if SeverityAtLeast("info", "bogus") {
		t.Error("unknown min severity should not be satisfied")
	}
	if SeverityAtLeast("bogus", "info") {
		t.Error("unknown finding severity should not satisfy a real min")
	}
}
