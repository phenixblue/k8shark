package anonymize

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/phenixblue/k8shark/internal/capture"
)

// urlHostPattern matches a URL scheme prefix immediately followed by a
// host: one mandatory DNS label, then zero or more ".<label>" labels — so
// both a bare in-cluster service name ("https://webhook-svc:8443") and a
// fully-qualified one ("https://webhook-svc.default.svc:8443") match. The
// scheme prefix is what makes this safe as a full-tree, unscoped scan: it's
// the trigger that disambiguates a real host from any other dot-containing
// string (a version number, an image tag) which never appears right after
// "://". Deliberately does not also match a bare hostname with no scheme —
// that would reintroduce exactly the false-positive risk the scheme prefix
// exists to avoid; bare hostnames are instead handled at their own known
// schema-aware field locations (see rewriteURLInObject).
var urlHostPattern = regexp.MustCompile(`(?i)((?:https?|wss?)://)([a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*)`)

// spliceURLHosts rewrites every scheme://host occurrence in s, replacing
// only the host substring with alias(host) — the scheme, any port, path,
// and query string are left untouched, unlike the whole-value swap the
// resource-name categories use. This is what lets a hostname keep reading
// consistently whether it shows up bare (an Ingress host field) or embedded
// in a longer webhook URL string.
func spliceURLHosts(s string, alias func(string) string) (string, bool) {
	matches := urlHostPattern.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, false
	}
	var out strings.Builder
	last := 0
	for _, m := range matches {
		hostStart, hostEnd := m[4], m[5]
		out.WriteString(s[last:hostStart])
		out.WriteString(alias(s[hostStart:hostEnd]))
		last = hostEnd
	}
	out.WriteString(s[last:])
	return out.String(), true
}

// rewriteURLInObject rewrites the known bare-hostname fields (no scheme, so
// spliceURLHosts's regex wouldn't match them at all) for a single decoded
// JSON object, using kind to know which fields apply:
//
//   - Ingress: spec.rules[*].host, spec.tls[*].hosts[*]
//   - Service: spec.externalName
//
// Full-tree scheme://host occurrences (webhook URLs, cert-manager/
// external-dns annotations, status condition messages) are handled
// separately by rewriteURLInRecord's walkStrings pass over the whole body,
// not here — these two mechanisms are mutually exclusive by construction
// (one requires a scheme prefix, the other only ever looks at fields that
// never have one), so there's no risk of double-aliasing the same value.
func rewriteURLInObject(obj map[string]interface{}, kind string, alias func(string) string) bool {
	modified := false

	spec, ok := obj["spec"].(map[string]interface{})
	if !ok {
		return false
	}

	switch kind {
	case "Ingress":
		if rules, ok := spec["rules"].([]interface{}); ok {
			for _, raw := range rules {
				rule, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				if host, ok := rule["host"].(string); ok && host != "" {
					rule["host"] = alias(host)
					modified = true
				}
			}
		}
		if tlsEntries, ok := spec["tls"].([]interface{}); ok {
			for _, raw := range tlsEntries {
				entry, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				hosts, ok := entry["hosts"].([]interface{})
				if !ok {
					continue
				}
				for i, h := range hosts {
					host, ok := h.(string)
					if !ok || host == "" {
						continue
					}
					hosts[i] = alias(host)
					modified = true
				}
			}
		}
	case "Service":
		if externalName, ok := spec["externalName"].(string); ok && externalName != "" {
			spec["externalName"] = alias(externalName)
			modified = true
		}
	}

	return modified
}

// rewriteURLInRecord decodes rec's body and rewrites every URL/hostname
// occurrence it recognizes: the bare-hostname schema-aware fields
// (rewriteURLInObject, list-aware the same way rewriteNamespaceInRecord and
// rewriteResourceNameInRecord are) plus a full-tree walk for
// scheme://host substrings anywhere in the body (webhook URLs,
// cert-manager/external-dns annotations, status condition messages).
func rewriteURLInRecord(rec *capture.Record, alias func(string) string) (bool, error) {
	var obj map[string]interface{}
	if err := json.Unmarshal(rec.ResponseBody, &obj); err != nil {
		return false, nil
	}

	kind, _ := obj["kind"].(string)
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
			if rewriteURLInObject(item, ik, alias) {
				items[i] = item
				modified = true
			}
		}
	} else if rewriteURLInObject(obj, kind, alias) {
		modified = true
	}

	if walkStrings(interface{}(obj), func(s string) (string, bool) {
		return spliceURLHosts(s, alias)
	}) {
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
