package anonymize

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

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
// status.addresses[type=InternalIP|ExternalIP]) with no allowlist needed —
// there are two mechanisms in the design plan's own wording, but only one is
// needed in the implementation.
//
// This is a whole-value match only, not substring splicing: a field whose
// entire value is exactly an IP literal is caught wherever it occurs
// (including an annotation or a Table cell that holds nothing else), but an
// IP embedded inside a longer free-text string — the common shape for a
// real Event message ("Pulling image ... from 10.1.2.3") — is not, since
// walkStrings only ever replaces a whole leaf value net.ParseIP accepts as
// one. Deliberately out of scope, same reasoning as the CIDR gap below:
// recognizing an IP as a substring of arbitrary text needs its own
// detector, not just net.ParseIP on the whole string. ipmatch_test.go's
// stray-occurrence case documents this boundary directly.
//
// Out of scope: a CIDR string (e.g. a Node's spec.podCIDR, "10.244.0.0/16")
// does not parse as a bare IP (net.ParseIP rejects the "/16" suffix), so it
// passes through unrecognized. Splitting a CIDR into its address and
// prefix-length, aliasing just the address, is real additional work the
// design plan doesn't call out for this milestone.
//
// List-aware, the same way the schema-aware categories are (namespace.go,
// resourcename.go): an exclude rule can be scoped to a Kind, and IP
// occurrences inside a List response's items[] belong to each item's own
// Kind, not the List's — walking the whole record body in one flat pass
// (as this function did before rule support existed) would have no correct
// Kind to check exclusion against for a List response at all.
func rewriteIPInRecord(rec *capture.Record, excluded excludedFunc, alias func(string) string) (bool, error) {
	var obj map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
		return false, nil
	}

	kind, _ := obj["kind"].(string)
	visit := func(node interface{}, itemKind string) bool {
		return walkStrings(node, "", func(path, s string) (string, bool) {
			if net.ParseIP(s) == nil {
				return "", false
			}
			if excluded(CategoryIP, itemKind, path) {
				return "", false
			}
			return alias(s), true
		})
	}

	modified := false
	if strings.HasSuffix(kind, "List") {
		items, _ := obj["items"].([]interface{})
		itemKind := strings.TrimSuffix(kind, "List")
		for i, itemRaw := range items {
			item, ok := itemRaw.(map[string]interface{})
			if !ok {
				continue
			}
			ik := itemKind
			if k, ok := item["kind"].(string); ok && k != "" {
				ik = k
			}
			if visit(item, ik) {
				items[i] = item
				modified = true
			}
		}
	} else if visit(obj, kind) {
		modified = true
	}

	if !modified {
		return false, nil
	}

	newBody, err := json.Marshal(obj)
	if err != nil {
		return false, fmt.Errorf("re-marshaling record: %w", err)
	}
	rec.ResponseBody = newBody
	return true, nil
}
