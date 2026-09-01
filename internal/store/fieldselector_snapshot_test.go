package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// This test closes the loop between the hand-maintained tables in
// fieldselector.go and upstream Kubernetes. There are three links in the chain:
//
//	upstream source  --(scripts/fieldselector_drift.py)-->  snapshot JSON
//	snapshot JSON    --(this test)-->                       fieldSelectorKinds
//
// The script keeps the snapshot honest against upstream (it needs the network,
// so it runs as its own CI job); this test keeps the Go tables honest against
// the snapshot, offline and on every `make test`. Without it, the snapshot could
// track upstream perfectly while the Go code quietly drifted from both.
//
// The conformance differential (scripts/conformance_diff.py, section G) is the
// third mechanism, comparing actual behavior against a live apiserver.

const snapshotPath = "../../scripts/fieldselector-snapshot.json"

// upstreamSnapshot mirrors the JSON scripts/fieldselector_drift.py writes.
type upstreamSnapshot struct {
	KubernetesVersion string `json:"kubernetesVersion"`
	// Accepted is file path -> Kind -> accepted field labels.
	Accepted map[string]map[string][]string `json:"accepted"`
	// Selectable is file path -> function name -> selectable field labels.
	Selectable map[string]map[string][]string `json:"selectable"`
}

// snapshotAcceptedKey identifies one entry in the snapshot's accepted section.
type snapshotAcceptedKey struct {
	file string
	kind string
}

// acceptedSources maps each upstream conversion func to the group/resource its
// contract governs in our table.
var acceptedSources = map[snapshotAcceptedKey]groupResource{
	{"pkg/apis/core/v1/conversion.go", "Pod"}:                               {"", "pods"},
	{"pkg/apis/core/v1/conversion.go", "Node"}:                              {"", "nodes"},
	{"pkg/apis/core/v1/conversion.go", "Namespace"}:                         {"", "namespaces"},
	{"pkg/apis/core/v1/conversion.go", "Secret"}:                            {"", "secrets"},
	{"pkg/apis/core/v1/conversion.go", "Service"}:                           {"", "services"},
	{"pkg/apis/core/v1/conversion.go", "ReplicationController"}:             {"", "replicationcontrollers"},
	{"pkg/apis/core/v1/conversion.go", "Event"}:                             {"", "events"},
	{"pkg/apis/events/v1/conversion.go", "Event"}:                           {"events.k8s.io", "events"},
	{"pkg/apis/batch/v1/conversion.go", "Job"}:                              {"batch", "jobs"},
	{"pkg/apis/certificates/v1/conversion.go", "CertificateSigningRequest"}: {"certificates.k8s.io", "certificatesigningrequests"},
	{"pkg/apis/certificates/v1/conversion.go", "ClusterTrustBundle"}:        {"certificates.k8s.io", "clustertrustbundles"},
	{"pkg/apis/certificates/v1/conversion.go", "PodCertificateRequest"}:     {"certificates.k8s.io", "podcertificaterequests"},
}

// snapshotSelectableKey identifies one entry in the snapshot's selectable section.
type snapshotSelectableKey struct {
	file string
	fn   string
}

// selectableSources maps each upstream ToSelectableFields to the group/resource
// it projects in our table.
var selectableSources = map[snapshotSelectableKey]groupResource{
	{"pkg/registry/core/pod/strategy.go", "ToSelectableFields"}:                             {"", "pods"},
	{"pkg/registry/core/node/strategy.go", "NodeToSelectableFields"}:                        {"", "nodes"},
	{"pkg/registry/core/namespace/strategy.go", "NamespaceToSelectableFields"}:              {"", "namespaces"},
	{"pkg/registry/core/secret/strategy.go", "SelectableFields"}:                            {"", "secrets"},
	{"pkg/registry/core/service/strategy.go", "SelectableFields"}:                           {"", "services"},
	{"pkg/registry/core/replicationcontroller/strategy.go", "ControllerToSelectableFields"}: {"", "replicationcontrollers"},
	{"pkg/registry/core/event/strategy.go", "ToSelectableFields"}:                           {"", "events"},
	{"pkg/registry/batch/job/strategy.go", "JobToSelectableFields"}:                         {"batch", "jobs"},
	{"pkg/registry/certificates/certificates/strategy.go", "SelectableFields"}:              {"certificates.k8s.io", "certificatesigningrequests"},
}

// metadataLabels are supplied upstream by generic.ObjectMetaFieldsSet rather than
// appearing in the fields.Set literal, so they are absent from the snapshot's
// selectable lists and excluded when comparing.
var metadataLabels = map[string]bool{
	"metadata.name":      true,
	"metadata.namespace": true,
}

// inertSelectableLabels are keys upstream's ToSelectableFields sets that no
// conversion func accepts, making them unreachable through the API. We omit them
// deliberately rather than implement a field no request can select on.
var inertSelectableLabels = map[snapshotSelectableKey]map[string]bool{
	// NamespaceToSelectableFields sets a bare "name" alongside metadata.name,
	// kept upstream for backward compatibility and flagged there as a bug.
	{"pkg/registry/core/namespace/strategy.go", "NamespaceToSelectableFields"}: {"name": true},
}

func loadUpstreamSnapshot(t *testing.T) upstreamSnapshot {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(snapshotPath))
	if err != nil {
		t.Fatalf("reading %s: %v (regenerate with scripts/fieldselector_drift.py --update)",
			snapshotPath, err)
	}
	var snap upstreamSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("decoding %s: %v", snapshotPath, err)
	}
	return snap
}

// TestFieldSelectorTables_AcceptedMatchesUpstream asserts every label upstream's
// conversion funcs accept is accepted here, and that we accept nothing extra.
func TestFieldSelectorTables_AcceptedMatchesUpstream(t *testing.T) {
	snap := loadUpstreamSnapshot(t)

	for key, gr := range acceptedSources {
		kinds, ok := snap.Accepted[key.file]
		if !ok {
			t.Errorf("snapshot has no accepted entry for %s", key.file)
			continue
		}
		want, ok := kinds[key.kind]
		if !ok {
			t.Errorf("snapshot %s has no accepted labels for kind %s", key.file, key.kind)
			continue
		}
		rules, ok := compiledFieldSelectorRules[gr]
		if !ok {
			t.Errorf("no field-selector rules for %+v (upstream %s/%s accepts %v)",
				gr, key.file, key.kind, want)
			continue
		}
		got := make([]string, 0, len(rules.accepted))
		for label := range rules.accepted {
			got = append(got, label)
		}
		sort.Strings(got)
		sort.Strings(want)
		if diff := labelSetDiff(want, got); diff != "" {
			t.Errorf("accepted labels for %+v disagree with upstream %s/%s:\n%s",
				gr, key.file, key.kind, diff)
		}
	}
}

// TestFieldSelectorTables_SelectableMatchesUpstream asserts our selectable set
// matches ToSelectableFields, so an accepted-but-not-selectable label stays
// accepted-but-not-selectable (pods' status.podIPs) and nothing we claim to
// filter on is absent upstream.
func TestFieldSelectorTables_SelectableMatchesUpstream(t *testing.T) {
	snap := loadUpstreamSnapshot(t)

	for key, gr := range selectableSources {
		fns, ok := snap.Selectable[key.file]
		if !ok {
			t.Errorf("snapshot has no selectable entry for %s", key.file)
			continue
		}
		upstreamLabels, ok := fns[key.fn]
		if !ok {
			t.Errorf("snapshot %s has no selectable labels for %s", key.file, key.fn)
			continue
		}
		rules, ok := compiledFieldSelectorRules[gr]
		if !ok {
			t.Errorf("no field-selector rules for %+v", gr)
			continue
		}

		want := make([]string, 0, len(upstreamLabels))
		for _, label := range upstreamLabels {
			if inertSelectableLabels[key][label] {
				continue
			}
			want = append(want, label)
		}
		got := make([]string, 0, len(rules.selectable))
		for label := range rules.selectable {
			// Metadata labels come from ObjectMetaFieldsSet upstream, so they are
			// not in the snapshot's list.
			if metadataLabels[label] {
				continue
			}
			got = append(got, label)
		}
		sort.Strings(want)
		sort.Strings(got)
		if diff := labelSetDiff(want, got); diff != "" {
			t.Errorf("selectable labels for %+v disagree with upstream %s/%s:\n%s",
				gr, key.file, key.fn, diff)
		}
	}
}

// TestFieldSelectorTables_EveryKindIsCovered guards against adding an entry to
// fieldSelectorKinds without wiring it into the snapshot comparison, which would
// leave it unchecked against upstream forever.
func TestFieldSelectorTables_EveryKindIsCovered(t *testing.T) {
	covered := map[groupResource]bool{}
	for _, gr := range acceptedSources {
		covered[gr] = true
	}
	for gr := range fieldSelectorKinds {
		if !covered[gr] {
			t.Errorf("fieldSelectorKinds has %+v with no acceptedSources entry: add one so "+
				"the upstream snapshot comparison covers it", gr)
		}
	}
	for _, gr := range selectableSources {
		if _, ok := fieldSelectorKinds[gr]; !ok {
			t.Errorf("selectableSources references %+v, which is not in fieldSelectorKinds", gr)
		}
	}
}

// TestFieldSelectorTables_SelectablePathsResolve checks every selectable path is
// non-empty and, for a canonical label, that it is actually reachable — a typo
// in a path would otherwise read as "field absent" and silently match nothing.
func TestFieldSelectorTables_SelectablePathsResolve(t *testing.T) {
	for gr, spec := range fieldSelectorKinds {
		for label, p := range spec.selectable {
			if p.path == "" {
				t.Errorf("%+v: selectable %q has an empty path", gr, label)
			}
		}
		// Every canonical label a selectable entry defines must be reachable:
		// either accepted directly, an alias target, or a metadata key.
		rules := compiledFieldSelectorRules[gr]
		for label := range spec.selectable {
			if metadataLabels[label] {
				continue
			}
			reachable := false
			for _, canonical := range rules.accepted {
				if canonical == label {
					reachable = true
					break
				}
			}
			if !reachable {
				t.Errorf("%+v: selectable %q is not reachable — no accepted label maps to it",
					gr, label)
			}
		}
	}
}

// labelSetDiff returns a human-readable description of how two sorted label
// sets differ, or "" when they match.
func labelSetDiff(want, got []string) string {
	inWant := map[string]bool{}
	for _, w := range want {
		inWant[w] = true
	}
	inGot := map[string]bool{}
	for _, g := range got {
		inGot[g] = true
	}
	var missing, extra []string
	for _, w := range want {
		if !inGot[w] {
			missing = append(missing, w)
		}
	}
	for _, g := range got {
		if !inWant[g] {
			extra = append(extra, g)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return ""
	}
	out := ""
	if len(missing) > 0 {
		out += "  missing (upstream has, we don't): " + join(missing) + "\n"
	}
	if len(extra) > 0 {
		out += "  extra (we have, upstream doesn't): " + join(extra) + "\n"
	}
	return out
}

func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}
