// Package diagnose analyzes a captured cluster archive and produces
// severity-ranked findings — the offline equivalent of popeye/kube-score.
// It is shared by the `kshrk diagnose` CLI and the web UI.
package diagnose

import "sort"

// SchemaVersion is the version of the Report/Finding JSON shape. Bump on a
// breaking change to the documented `-o json` output so CI consumers can pin it.
const SchemaVersion = 1

// Severity levels, ordered.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// severityRank orders severities for sorting and --fail-on comparisons.
var severityRank = map[string]int{SeverityInfo: 0, SeverityWarning: 1, SeverityCritical: 2}

// SeverityAtLeast reports whether a is at least as severe as min. Unknown
// severities never satisfy the comparison, so a stray/typo'd value can't
// silently pass a filter.
func SeverityAtLeast(a, min string) bool {
	ra, okA := severityRank[a]
	rm, okMin := severityRank[min]
	if !okA || !okMin {
		return false
	}
	return ra >= rm
}

// ObjectRef identifies the object a finding is about.
type ObjectRef struct {
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	APIPath   string `json:"api_path,omitempty"`
}

// Finding is a single diagnostic result. The JSON shape is a stable contract
// (see SchemaVersion).
type Finding struct {
	RuleID     string    `json:"rule_id"`            // stable id, e.g. "pod.crashloopbackoff"
	Severity   string    `json:"severity"`           // info | warning | critical
	Category   string    `json:"category"`           // workload|scheduling|storage|node|cluster
	Title      string    `json:"title"`              // short human summary
	Object     ObjectRef `json:"object"`             // the (representative) affected object
	Evidence   string    `json:"evidence,omitempty"` // what was observed
	Suggestion string    `json:"suggestion,omitempty"`
	Count      int       `json:"count,omitempty"` // >1 when this finding groups several objects
}

// Report is the top-level diagnose output.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	CaptureID     string    `json:"capture_id,omitempty"`
	At            string    `json:"at,omitempty"`
	Summary       Summary   `json:"summary"`
	Findings      []Finding `json:"findings"`
}

// Summary counts findings by severity.
type Summary struct {
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
}

// sortFindings orders findings by severity (critical first), then category,
// then rule id, then object — deterministic for stable output and golden tests.
func sortFindings(fs []Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		if ri, rj := severityRank[fs[i].Severity], severityRank[fs[j].Severity]; ri != rj {
			return ri > rj
		}
		if fs[i].Category != fs[j].Category {
			return fs[i].Category < fs[j].Category
		}
		if fs[i].RuleID != fs[j].RuleID {
			return fs[i].RuleID < fs[j].RuleID
		}
		if fs[i].Object.Namespace != fs[j].Object.Namespace {
			return fs[i].Object.Namespace < fs[j].Object.Namespace
		}
		return fs[i].Object.Name < fs[j].Object.Name
	})
}
