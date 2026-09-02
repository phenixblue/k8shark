package anonymize

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/phenixblue/k8shark/internal/capture"
)

// urlHostPattern matches a URL scheme prefix, an optional "userinfo@" (RFC
// 3986's "user[:password]@" form, e.g. "https://user:pass@host/path") which
// is skipped over rather than captured, and then the actual host: either a
// bracketed IPv6 literal (RFC 3986 requires the brackets in a URL's host
// position, e.g. "https://[fd00::1]:6443") or a DNS name — one mandatory
// label, then zero or more ".<label>" labels. So a bare in-cluster service
// name ("https://webhook-svc:8443"), a fully-qualified one
// ("https://webhook-svc.default.svc:8443"), an IPv6 cluster address, and a
// URL carrying credentials all match, with the captured host group always
// landing on the real host, not the username. The userinfo group excludes
// "/" so it can only ever match up to the next "@" *before* the path even
// starts — a literal "@" that shows up later, inside the path, can't be
// mistaken for a userinfo separator this way. The scheme prefix is what
// makes the whole pattern safe as a full-tree, unscoped scan: it's the
// trigger that disambiguates a real host from any other dot- or
// colon-containing string (a version number, an image tag) which never
// appears right after "://". Deliberately does not also match a bare
// hostname with no scheme — that would reintroduce exactly the
// false-positive risk the scheme prefix exists to avoid; bare hostnames are
// instead handled at their own known schema-aware field locations (see
// rewriteURLInObject).
var urlHostPattern = regexp.MustCompile(`(?i)((?:https?|wss?)://)(?:[^/@]*@)?(\[[0-9a-fA-F:]+\]|[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*)`)

// spliceURLHosts rewrites every scheme://host occurrence in s, replacing
// only the host substring with alias(host) — the scheme, any port, path,
// and query string are left untouched, unlike the whole-value swap the
// resource-name categories use. This is what lets a hostname keep reading
// consistently whether it shows up bare (an Ingress host field) or embedded
// in a longer webhook URL string.
//
// A matched IPv6 literal's surrounding brackets are stripped before
// aliasing and not reinstated: the replacement is always a DNS hostname
// (alias.go's aliasName), never an IP literal, and only an IP literal needs
// (or is allowed) brackets in a URL's host position — reinserting them
// around a hostname would itself be invalid.
func spliceURLHosts(s string, alias func(string) string) (string, bool) {
	matches := urlHostPattern.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s, false
	}
	var out strings.Builder
	last := 0
	for _, m := range matches {
		hostStart, hostEnd := m[4], m[5]
		host := strings.TrimSuffix(strings.TrimPrefix(s[hostStart:hostEnd], "["), "]")
		out.WriteString(s[last:hostStart])
		out.WriteString(alias(host))
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
