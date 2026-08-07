package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/fields"
)

// Field selectors, as the apiserver actually implements them.
//
// kubectl contributes nothing here: cli-runtime's Builder.FieldSelectorParam
// trims the string, rejects a conflict with --all, and forwards it as
// ?fieldSelector=. It holds no per-kind table, so every guarantee a client
// expects — the 400 on a bad key, the per-kind key set — is owed by the server,
// which for a replay is us.
//
// The apiserver evaluates a selector in two *independent* layers, and this file
// mirrors both:
//
//  1. Validation and aliasing — Scheme.ConvertFieldLabel(gvk, label, value),
//     backed by the per-kind funcs registered with AddFieldLabelConversionFunc
//     in k8s.io/kubernetes/pkg/apis/<group>/<version>/conversion.go. The
//     generic endpoint handler runs every requirement through it via
//     fields.Selector.Transform and turns a rejection into a 400 — identically
//     for LIST, WATCH and DELETECOLLECTION. A kind with no registered func
//     falls back to runtime.DefaultMetaV1FieldSelectorConversion, which accepts
//     only metadata.name and metadata.namespace; that fallback is why a custom
//     resource rejects everything else.
//
//  2. Matching — the registry strategy's ToSelectableFields
//     (k8s.io/kubernetes/pkg/registry/.../strategy.go), which projects the
//     object into a fields.Set the selector is then matched against.
//
// The two lists are not the same, so collapsing them into one table would be
// wrong in both directions. Pods accept status.podIPs (layer 1) but
// PodToSelectableFields never sets it (layer 2), so upstream accepts
// status.podIPs=10.0.0.1 and then matches nothing, because fields.Set.Get
// returns "" for a key that was never set. Meanwhile spec.host is accepted only
// as a legacy alias, rewritten to spec.nodeName before matching. Keeping
// acceptance and selectability separate reproduces both behaviors exactly.
//
// These tables are hand-maintained because there is nothing to reflect over:
// the registrations live in k8s.io/kubernetes's internal API packages, which
// are not reachable from k8s.io/api or client-go. Two mechanisms detect drift —
// the conformance differential exercises fieldSelector queries against a live
// apiserver, and scripts/fieldselector_drift.py diffs these tables against
// upstream source. See docs/conformance.md.

// valueKind says how to stringify a resolved JSON value and, more importantly,
// what an *absent* key resolves to. ToSelectableFields always sets every
// selectable key, so an unset bool reads "false" and an unset count reads "0" —
// never "".
type valueKind uint8

const (
	valueString valueKind = iota
	valueBool
	valueInt
)

// selectablePath locates one canonical label's value in the serialized object.
type selectablePath struct {
	// path is a dotted path into the object as served over the wire, which is
	// not always the label: Job's status.successful reads status.succeeded, and
	// an events.k8s.io/v1 Event carries regarding.* where the canonical label
	// says involvedObject.*.
	path string
	// fallback supplies the value when path resolves to the empty string.
	// Mirrors core/v1 Events' "source", which upstream computes as
	// event.Source.Component falling back to event.ReportingController.
	fallback string
	kind     valueKind
}

// kindSpec transcribes one kind's two upstream lists. Deliberately verbose and
// literal so a reviewer can diff it against the upstream files named above.
type kindSpec struct {
	// namespaced mirrors the generic.ObjectMetaFieldsSet(_, namespaced)
	// argument in the kind's ToSelectableFields. Cluster-scoped kinds (Node,
	// Namespace, CertificateSigningRequest) neither accept nor select
	// metadata.namespace — `kubectl get nodes --field-selector
	// metadata.namespace=x` is a 400 upstream, not an empty result.
	namespaced bool
	// accepted are the labels the conversion func takes unchanged, beyond the
	// metadata keys implied by namespaced.
	accepted []string
	// aliases are labels the conversion func takes and rewrites, mapping the
	// client's spelling to the canonical label: legacy (pods' spec.host) or
	// version-specific (events.k8s.io/v1's regarding.*).
	aliases map[string]string
	// selectable maps a *canonical* label to where its value lives. A canonical
	// label that is accepted but missing here resolves to "" — upstream's
	// behavior for a key ToSelectableFields never sets.
	selectable map[string]selectablePath
	// rejectf overrides the 400 message for an unaccepted label. Upstream's
	// text is per-kind and inconsistent; batch/v1 Job is the odd one out.
	rejectf func(label string) string
}

// groupResource keys the table by API group and plural resource, since that is
// what the request path yields. Upstream keys by GroupVersionKind, but the
// group/resource mapping is stable and no kind's field-label contract varies
// across the versions we serve — except Events, whose two API groups differ
// enough that they get separate entries.
type groupResource struct {
	group    string
	resource string
}

// fieldSelectorKinds transcribes upstream's per-kind field-label contracts.
// Every entry is sourced from a pair of upstream files: the conversion func for
// `accepted`/`aliases`, the registry strategy for `selectable`. A resource
// absent here gets metadataOnlyRules, matching upstream's fallback for any kind
// with no registered conversion func — including every custom resource and, as
// it happens, everything in the apps group (apps/v1 registers none, so a
// Deployment or ReplicaSet really does reject status.replicas).
var fieldSelectorKinds = map[groupResource]kindSpec{
	// pkg/apis/core/v1/conversion.go + pkg/registry/core/pod/strategy.go
	{"", "pods"}: {
		namespaced: true,
		accepted: []string{
			"spec.nodeName", "spec.restartPolicy", "spec.schedulerName",
			"spec.serviceAccountName", "spec.hostNetwork", "status.phase",
			"status.podIP", "status.podIPs", "status.nominatedNodeName",
		},
		aliases: map[string]string{"spec.host": "spec.nodeName"},
		selectable: map[string]selectablePath{
			"spec.nodeName":           {path: "spec.nodeName"},
			"spec.restartPolicy":      {path: "spec.restartPolicy"},
			"spec.schedulerName":      {path: "spec.schedulerName"},
			"spec.serviceAccountName": {path: "spec.serviceAccountName"},
			// Upstream reads Spec.SecurityContext.HostNetwork on the internal
			// type; the v1 wire format carries it at spec.hostNetwork.
			"spec.hostNetwork": {path: "spec.hostNetwork", kind: valueBool},
			"status.phase":     {path: "status.phase"},
			// Upstream reads Status.PodIPs[0].IP; the serialized object carries
			// the equivalent scalar, which the apiserver keeps in sync.
			"status.podIP":             {path: "status.podIP"},
			"status.nominatedNodeName": {path: "status.nominatedNodeName"},
			// status.podIPs is intentionally absent: accepted by the conversion
			// func, never set by PodToSelectableFields.
		},
	},
	// pkg/apis/core/v1/conversion.go + pkg/registry/core/node/strategy.go
	{"", "nodes"}: {
		accepted: []string{"spec.unschedulable"},
		selectable: map[string]selectablePath{
			"spec.unschedulable": {path: "spec.unschedulable", kind: valueBool},
		},
	},
	// pkg/apis/core/v1/conversion.go + pkg/registry/core/namespace/strategy.go
	{"", "namespaces"}: {
		accepted: []string{"status.phase"},
		selectable: map[string]selectablePath{
			"status.phase": {path: "status.phase"},
			// NamespaceToSelectableFields also sets a bare "name" key, kept
			// upstream for backward compatibility. It is unreachable: the
			// conversion func does not accept "name", so no request can select
			// on it. Omitted deliberately.
		},
	},
	// pkg/apis/core/v1/conversion.go + pkg/registry/core/secret/strategy.go
	{"", "secrets"}: {
		namespaced: true,
		accepted:   []string{"type"},
		selectable: map[string]selectablePath{"type": {path: "type"}},
	},
	// pkg/apis/core/v1/conversion.go + pkg/registry/core/service/strategy.go
	{"", "services"}: {
		namespaced: true,
		accepted:   []string{"spec.clusterIP", "spec.type"},
		selectable: map[string]selectablePath{
			"spec.clusterIP": {path: "spec.clusterIP"},
			"spec.type":      {path: "spec.type"},
		},
	},
	// pkg/apis/core/v1/conversion.go
	// + pkg/registry/core/replicationcontroller/strategy.go
	{"", "replicationcontrollers"}: {
		namespaced: true,
		accepted:   []string{"status.replicas"},
		selectable: map[string]selectablePath{
			"status.replicas": {path: "status.replicas", kind: valueInt},
		},
	},
	// pkg/apis/core/v1/conversion.go + pkg/registry/core/event/strategy.go
	{"", "events"}: {
		namespaced: true,
		accepted: []string{
			"involvedObject.kind", "involvedObject.namespace", "involvedObject.name",
			"involvedObject.uid", "involvedObject.apiVersion",
			"involvedObject.resourceVersion", "involvedObject.fieldPath",
			"reason", "reportingComponent", "source", "type",
		},
		selectable: map[string]selectablePath{
			"involvedObject.kind":            {path: "involvedObject.kind"},
			"involvedObject.namespace":       {path: "involvedObject.namespace"},
			"involvedObject.name":            {path: "involvedObject.name"},
			"involvedObject.uid":             {path: "involvedObject.uid"},
			"involvedObject.apiVersion":      {path: "involvedObject.apiVersion"},
			"involvedObject.resourceVersion": {path: "involvedObject.resourceVersion"},
			"involvedObject.fieldPath":       {path: "involvedObject.fieldPath"},
			"reason":                         {path: "reason"},
			"reportingComponent":             {path: "reportingComponent"},
			"source":                         {path: "source.component", fallback: "reportingComponent"},
			"type":                           {path: "type"},
		},
	},
	// pkg/apis/events/v1/conversion.go + pkg/registry/core/event/strategy.go.
	// events.k8s.io/v1 shares the core Event storage, so the canonical labels
	// are the core ones — but the wire format uses regarding.* and
	// reportingController, and the core spellings are *not* accepted here.
	{"events.k8s.io", "events"}: {
		namespaced: true,
		accepted:   []string{"reason", "type"},
		aliases: map[string]string{
			"regarding.kind":            "involvedObject.kind",
			"regarding.namespace":       "involvedObject.namespace",
			"regarding.name":            "involvedObject.name",
			"regarding.uid":             "involvedObject.uid",
			"regarding.apiVersion":      "involvedObject.apiVersion",
			"regarding.resourceVersion": "involvedObject.resourceVersion",
			"regarding.fieldPath":       "involvedObject.fieldPath",
			"reportingController":       "reportingComponent",
		},
		selectable: map[string]selectablePath{
			"involvedObject.kind":            {path: "regarding.kind"},
			"involvedObject.namespace":       {path: "regarding.namespace"},
			"involvedObject.name":            {path: "regarding.name"},
			"involvedObject.uid":             {path: "regarding.uid"},
			"involvedObject.apiVersion":      {path: "regarding.apiVersion"},
			"involvedObject.resourceVersion": {path: "regarding.resourceVersion"},
			"involvedObject.fieldPath":       {path: "regarding.fieldPath"},
			"reason":                         {path: "reason"},
			"reportingComponent":             {path: "reportingController"},
			"type":                           {path: "type"},
		},
	},
	// pkg/apis/batch/v1/conversion.go + pkg/registry/batch/job/strategy.go
	{"batch", "jobs"}: {
		namespaced: true,
		accepted:   []string{"status.successful"},
		selectable: map[string]selectablePath{
			// The label and the field differ: upstream reads Status.Succeeded.
			"status.successful": {path: "status.succeeded", kind: valueInt},
		},
		rejectf: func(label string) string {
			return fmt.Sprintf("field label %q not supported for Job", label)
		},
	},
	// pkg/apis/certificates/v1/conversion.go
	// + pkg/registry/certificates/certificates/strategy.go
	{"certificates.k8s.io", "certificatesigningrequests"}: {
		accepted: []string{"spec.signerName"},
		selectable: map[string]selectablePath{
			"spec.signerName": {path: "spec.signerName"},
		},
	},
}

// fieldLabelRules is a kindSpec compiled into lookup form.
type fieldLabelRules struct {
	accepted   map[string]string // client label -> canonical label
	selectable map[string]selectablePath
	rejectf    func(label string) string
}

// defaultRejectf is the message most upstream conversion funcs use.
func defaultRejectf(label string) string {
	return "field label not supported: " + label
}

// metadataOnlyRules mirrors runtime.DefaultMetaV1FieldSelectorConversion, the
// fallback for any kind with no registered conversion func. Note the distinct
// error text — upstream's generic path does not say "field label not
// supported".
var metadataOnlyRules = fieldLabelRules{
	accepted: map[string]string{
		"metadata.name":      "metadata.name",
		"metadata.namespace": "metadata.namespace",
	},
	selectable: map[string]selectablePath{
		"metadata.name":      {path: "metadata.name"},
		"metadata.namespace": {path: "metadata.namespace"},
	},
	rejectf: func(label string) string {
		return fmt.Sprintf("%q is not a known field selector: only %q, %q",
			label, "metadata.name", "metadata.namespace")
	},
}

var compiledFieldSelectorRules = compileFieldSelectorRules()

func compileFieldSelectorRules() map[groupResource]fieldLabelRules {
	out := make(map[groupResource]fieldLabelRules, len(fieldSelectorKinds))
	for gr, spec := range fieldSelectorKinds {
		r := fieldLabelRules{
			accepted:   make(map[string]string, len(spec.accepted)+len(spec.aliases)+2),
			selectable: make(map[string]selectablePath, len(spec.selectable)+2),
			rejectf:    spec.rejectf,
		}
		if r.rejectf == nil {
			r.rejectf = defaultRejectf
		}
		r.accepted["metadata.name"] = "metadata.name"
		r.selectable["metadata.name"] = selectablePath{path: "metadata.name"}
		if spec.namespaced {
			r.accepted["metadata.namespace"] = "metadata.namespace"
			r.selectable["metadata.namespace"] = selectablePath{path: "metadata.namespace"}
		}
		for _, label := range spec.accepted {
			r.accepted[label] = label
		}
		for from, to := range spec.aliases {
			r.accepted[from] = to
		}
		for label, p := range spec.selectable {
			r.selectable[label] = p
		}
		out[gr] = r
	}
	return out
}

// rulesFor returns the field-label contract for a group/resource, falling back
// to the metadata-only rules for anything not in the table.
func rulesFor(group, resource string) fieldLabelRules {
	if r, ok := compiledFieldSelectorRules[groupResource{group, resource}]; ok {
		return r
	}
	return metadataOnlyRules
}

// FieldSelector is a parsed, per-kind-validated fieldSelector. A nil
// *FieldSelector matches everything, so callers can pass one through unchecked.
type FieldSelector struct {
	sel   fields.Selector
	rules fieldLabelRules
	// metadataOnly is true when every requirement reads metadata.name or
	// metadata.namespace, which lets Matches answer from the already-decoded
	// K8sObject instead of unmarshaling the object a second time.
	metadataOnly bool
}

// ParseFieldSelector parses and validates a client-supplied fieldSelector for
// the given API group and plural resource, mirroring the apiserver: the real
// selector grammar, then every requirement through the kind's field-label
// conversion. A returned error is what the apiserver reports as a 400 — callers
// on the read, watch and delete paths must all surface it as one, since
// upstream rejects an unsupported label identically on all three.
//
// Returns (nil, nil) for an empty selector, meaning no restriction.
func ParseFieldSelector(group, resource, selector string) (*FieldSelector, error) {
	if selector == "" {
		return nil, nil
	}
	sel, err := fields.ParseSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("invalid fieldSelector: %w", err)
	}
	rules := rulesFor(group, resource)
	// Transform is upstream's own mechanism (see listOpts in
	// apiserver/pkg/endpoints/handlers/get.go): it rewrites aliases and
	// propagates a rejection. Never return ("", "", nil) from this func —
	// apimachinery reads an empty field+value as "replace this requirement with
	// Everything", which would silently widen the selector to match-all, the
	// exact bug this file fixes.
	transformed, err := sel.Transform(func(label, value string) (string, string, error) {
		canonical, ok := rules.accepted[label]
		if !ok {
			return "", "", errors.New(rules.rejectf(label))
		}
		return canonical, value, nil
	})
	if err != nil {
		return nil, err
	}
	fs := &FieldSelector{sel: transformed, rules: rules, metadataOnly: true}
	for _, req := range transformed.Requirements() {
		p, ok := rules.selectable[req.Field]
		if !ok || (p.path != "metadata.name" && p.path != "metadata.namespace") {
			fs.metadataOnly = false
			break
		}
	}
	return fs, nil
}

// NamespaceScopeSelector restricts an aggregated cluster-wide list body to one
// namespace. It skips per-kind validation on purpose: it is synthesized
// internally to narrow a list the server itself widened, not parsed from a
// client request, and metadata.namespace is not an accepted label for every
// kind even where narrowing by it is meaningful.
func NamespaceScopeSelector(namespace string) *FieldSelector {
	return &FieldSelector{
		sel:          fields.OneTermEqualSelector("metadata.namespace", namespace),
		rules:        metadataOnlyRules,
		metadataOnly: true,
	}
}

// Restricts reports whether the selector actually narrows anything. A non-empty
// selector string can parse to zero requirements (fields.ParseSelector accepts
// a stray comma), which the write path treats as an error rather than as
// "matches everything".
func (fs *FieldSelector) Restricts() bool {
	return fs != nil && !fs.sel.Empty()
}

// NeedsFullObject reports whether evaluating the selector requires more than an
// object's metadata. A Table projection carries only PartialObjectMetadata, so a
// caller filtering Table rows must fall back to identity matching against the
// full objects when this is true.
func (fs *FieldSelector) NeedsFullObject() bool {
	return fs != nil && !fs.metadataOnly
}

// String returns the canonical selector text, after alias rewriting.
func (fs *FieldSelector) String() string {
	if fs == nil {
		return ""
	}
	return fs.sel.String()
}

// Matches reports whether one object satisfies the selector. raw is the object
// as served; obj is the same object already decoded, used for the common
// metadata-only case to avoid a second unmarshal. An object that cannot be
// decoded counts as a match, so a malformed item is never silently hidden
// (matching FilterItems' convention).
func (fs *FieldSelector) Matches(raw json.RawMessage, obj *K8sObject) bool {
	if fs == nil {
		return true
	}
	if fs.metadataOnly && obj != nil {
		return fs.sel.Matches(fieldsAdapter{rules: fs.rules, obj: obj})
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return true
	}
	return fs.sel.Matches(fieldsAdapter{rules: fs.rules, m: m})
}

// fieldsAdapter exposes an object's selectable fields as fields.Fields, so
// apimachinery's own selector can evaluate it. Exactly one of m and obj is set:
// obj is the metadata-only fast path.
type fieldsAdapter struct {
	rules fieldLabelRules
	m     map[string]any
	obj   *K8sObject
}

// Has reports whether the label is one ToSelectableFields would have set.
func (f fieldsAdapter) Has(label string) bool {
	_, ok := f.rules.selectable[label]
	return ok
}

// Get returns the label's value, or "" for a label the kind's
// ToSelectableFields never sets — reproducing fields.Set.Get for a missing key,
// which is why upstream's accepted-but-not-selectable labels match nothing.
func (f fieldsAdapter) Get(label string) string {
	p, ok := f.rules.selectable[label]
	if !ok {
		return ""
	}
	if f.obj != nil {
		switch p.path {
		case "metadata.name":
			return f.obj.Metadata.Name
		case "metadata.namespace":
			return f.obj.Metadata.Namespace
		}
		return ""
	}
	v := f.valueAt(p.path, p.kind)
	if v == "" && p.fallback != "" {
		v = f.valueAt(p.fallback, p.kind)
	}
	return v
}

func (f fieldsAdapter) valueAt(path string, kind valueKind) string {
	v, ok := lookupPath(f.m, path)
	if !ok {
		return zeroFieldValue(kind)
	}
	return stringifyFieldValue(v, kind)
}

// lookupPath walks a dotted path through a decoded JSON object. None of
// upstream's selectable fields index into an array, so no array syntax is
// supported.
func lookupPath(m map[string]any, path string) (any, bool) {
	cur := m
	parts := strings.Split(path, ".")
	for i, part := range parts {
		v, ok := cur[part]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return v, true
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return nil, false
}

// zeroFieldValue is what an absent key reads as. ToSelectableFields always sets
// every selectable key, so an unset bool is "false" and an unset count is "0";
// only strings read as empty.
func zeroFieldValue(kind valueKind) string {
	switch kind {
	case valueBool:
		return "false"
	case valueInt:
		return "0"
	default:
		return ""
	}
}

func stringifyFieldValue(v any, kind valueKind) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// Counts arrive as float64 from encoding/json; upstream stringifies them
		// with strconv.Itoa, so keep integral values integral.
		if !math.IsInf(t, 0) && !math.IsNaN(t) && t == math.Trunc(t) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case nil:
		return zeroFieldValue(kind)
	default:
		// A map or array is never a selectable value upstream.
		return ""
	}
}
