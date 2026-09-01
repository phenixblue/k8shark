package anonymize

import (
	"encoding/json"
	"fmt"
	"net"

	"github.com/phenixblue/k8shark/internal/capture"
)

// rewriteIPInRecord decodes rec's body and replaces every string value that
// parses as a valid IP literal (net.ParseIP) with alias(original), anywhere
// in the tree.
//
// Unlike the node/pod/workload categories, this needs no schema-aware fast
// path at all: an IP address is self-evidently a candidate the instant it
// parses as one, with no field-path allowlist required to tell a real
// occurrence from a false positive. One full-tree walk (walkStrings)
// therefore already covers every schema-aware location the design plan
// calls out (status.podIP, status.podIPs[*].ip, status.hostIP,
// spec.clusterIP(s), LoadBalancer ingress IPs, Endpoints addresses, Node
// status.addresses[type=InternalIP|ExternalIP]) *and* stray occurrences in
// annotations, Event messages, or Table cells, in a single pass — there are
// two mechanisms in the design plan's own wording, but only one is needed in
// the implementation.
//
// Out of scope: a CIDR string (e.g. a Node's spec.podCIDR, "10.244.0.0/16")
// does not parse as a bare IP (net.ParseIP rejects the "/16" suffix), so it
// passes through unrecognized. Splitting a CIDR into its address and
// prefix-length, aliasing just the address, is real additional work the
// design plan doesn't call out for this milestone.
func rewriteIPInRecord(rec *capture.Record, alias func(string) string) (bool, error) {
	var obj interface{}
	if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
		return false, nil
	}

	changed := walkStrings(obj, func(s string) (string, bool) {
		if net.ParseIP(s) == nil {
			return "", false
		}
		return alias(s), true
	})
	if !changed {
		return false, nil
	}

	newBody, err := json.Marshal(obj)
	if err != nil {
		return false, fmt.Errorf("re-marshaling record: %w", err)
	}
	rec.ResponseBody = newBody
	return true, nil
}
