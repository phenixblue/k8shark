// Package anonymize implements value/pattern-based anonymization of capture
// archives: an IP, hostname, image registry, or resource name is replaced
// consistently with the same alias everywhere it occurs in the archive,
// unlike kshrk redact's field-path-based replacement (internal/redact),
// which targets one exact field at a time with a fixed constant.
//
// The core primitive is Aliaser (alias.go): a pure function of
// (category, original value, salt), with no shared mutable state across
// records. That is a deliberate choice, not an implementation detail — see
// the Aliaser doc comment for why a stateful first-seen-order counter would
// be nondeterministic across runs.
package anonymize

// Category identifies which kind of value an alias is being generated for.
// It is mixed into every alias's derivation, so the same original string
// anonymized under two different categories never produces the same alias —
// not because of a lookup, but because each category renders into its own
// disjoint string space (see alias.go).
type Category string

const (
	// CategoryIP covers pod/host/cluster/load-balancer IP addresses.
	CategoryIP Category = "ip"
	// CategoryURL covers URLs and bare hostnames (ingress hosts, webhook
	// URLs, TLS SAN entries, the mock API server's own address).
	CategoryURL Category = "url"
	// CategoryImage covers container image registry hosts.
	CategoryImage Category = "image"
	// CategoryNode covers Node names.
	CategoryNode Category = "node"
	// CategoryNamespace covers Namespace names.
	CategoryNamespace Category = "namespace"
	// CategoryPod covers Pod names.
	CategoryPod Category = "pod"
	// CategoryWorkload covers Deployment/DaemonSet/StatefulSet/ReplicaSet/
	// Job/CronJob names.
	CategoryWorkload Category = "workload"
)

// AllCategories is the full set of categories anonymize is designed to
// eventually support, in a stable order — the target end-state from #137's
// design, not what's implemented today. Most are not usable yet:
// (*Aliaser).Alias panics for any category not in its own
// implementedCategories set (alias.go), which is a strict subset of this
// list until later milestones add the rest. Don't iterate AllCategories
// expecting every entry to work; iterate implementedCategories (or call
// Alias and let it panic loudly) if you need "what works right now."
var AllCategories = []Category{
	CategoryIP,
	CategoryURL,
	CategoryImage,
	CategoryNode,
	CategoryNamespace,
	CategoryPod,
	CategoryWorkload,
}
