package anonymize

// adjectives and nouns back the human-readable alias encoding in name.go
// (e.g. "node-quiet-otter" instead of "node-3f9a2b1c"). Every entry must be
// lowercase ASCII letters only — no digits, hyphens, or other characters —
// so that joining words with "-" always produces a valid DNS-1123 label;
// wordlist_test.go checks every entry against
// k8s.io/apimachinery/pkg/util/validation.IsDNS1123Label individually to
// catch a future editing mistake here (a typo'd space, an uppercase letter)
// before it ships as a silently-invalid alias.
//
// 64 adjectives x 64 nouns = 4096 two-word combinations, and 64x64x64 =
// 262144 three-word combinations (see name.go for which categories get
// which word count). Sized for realistic capture sizes, not an arbitrarily
// large namespace: a real cluster's distinct node/namespace count is
// realistically in the tens to low hundreds, not the tens of thousands.
var adjectives = []string{
	"able", "ample", "arid", "avid", "blue", "bold", "brave", "brief",
	"brisk", "broad", "calm", "clean", "clear", "close", "cool", "coral",
	"crisp", "curt", "dark", "deep", "dim", "dry", "eager", "early",
	"fair", "fast", "fine", "firm", "fond", "free", "fresh", "gentle",
	"glad", "gold", "good", "grand", "gray", "great", "green", "happy",
	"hardy", "keen", "kind", "large", "late", "level", "light", "little",
	"lively", "loyal", "lucky", "mellow", "merry", "mild", "misty", "modest",
	"neat", "nice", "noble", "plain", "quick", "quiet", "rapid", "steady",
}

var nouns = []string{
	"badger", "bear", "beetle", "bison", "boar", "bobcat", "bream", "camel",
	"canary", "cheetah", "cobra", "condor", "coyote", "crane", "crow", "deer",
	"dingo", "dolphin", "dove", "eagle", "egret", "elk", "falcon", "ferret",
	"finch", "fox", "gazelle", "gecko", "goose", "gopher", "grouse", "gull",
	"hare", "hawk", "heron", "horse", "ibex", "ibis", "jackal", "jaguar",
	"kite", "koala", "lemur", "leopard", "lion", "llama", "lynx", "magpie",
	"marten", "mink", "mole", "moose", "otter", "owl", "panther", "parrot",
	"perch", "quail", "rabbit", "raven", "robin", "seal", "shark", "wolf",
}
