package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/util/jsonpath"
)

// Table rendering
// ---------------
// kubectl's `get`/`-o wide` output is driven by server-side Table responses
// (columnDefinitions + per-object cells). The mock serves the captured Table
// verbatim when it fully covers a request (most faithful), but that can't cover
// objects created in the writable overlay or kinds/objects absent from the
// capture. renderResourceTable computes a Table for an arbitrary set of objects:
//
//	1. a built-in per-kind printer (core/native kinds), else
//	2. a CRD printer from the captured CRD's additionalPrinterColumns (JSONPath), else
//	3. the captured columnDefinitions for the kind + metadata-only cells, else
//	4. the generic buildTable (NAME/(NAMESPACE)/AGE).
//
// `-o wide` needs no special handling: every column carries a `priority`
// (0 = default, 1 = wide) and kubectl hides priority>0 unless `-o wide`.

// tableCol is one column: its schema plus a cell computer over a decoded object.
type tableCol struct {
	name     string
	typ      string // "string" | "integer" | "date"
	priority int    // 0 = shown by default, 1 = wide-only
	desc     string
	cell     func(obj map[string]any) any
	// age marks the CreationTimestamp column: its cell is the relative age
	// ("5m49s", "2d") computed at render time against the replay clock, matching
	// how a real apiserver renders the AGE column (a pre-computed string).
	age bool
}

// ── decoded-object accessors ────────────────────────────────────────────────

func mget(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func mstr(m map[string]any, path ...string) string {
	s, _ := mget(m, path...).(string)
	return s
}

func mint(m map[string]any, path ...string) int64 {
	switch n := mget(m, path...).(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func marr(m map[string]any, path ...string) []any {
	a, _ := mget(m, path...).([]any)
	return a
}

func orNone(s string) any {
	if s == "" {
		return "<none>"
	}
	return s
}

// ── common cells ────────────────────────────────────────────────────────────

func nameCell(o map[string]any) any { return mstr(o, "metadata", "name") }

// nameColumn / ageColumn are the framing columns shared by every built-in
// printer. AGE is a string column carrying the relative age (matching a real
// apiserver), computed at render time — see the `age` field on tableCol.
func nameColumn() tableCol {
	return tableCol{name: "Name", typ: "string", desc: "Name", cell: nameCell}
}
func ageColumn() tableCol {
	return tableCol{name: "Age", typ: "string", desc: "CreationTimestamp", age: true}
}

// humanAge renders a creationTimestamp as kubectl's relative age string,
// relative to now (the replay clock). Zero/invalid inputs render "<unknown>".
func humanAge(created string, now time.Time) any {
	if created == "" {
		return "<unknown>"
	}
	t, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return "<unknown>"
	}
	if now.IsZero() {
		now = t
	}
	return duration.HumanDuration(now.Sub(t))
}

func col(name, typ, desc string, cell func(map[string]any) any) tableCol {
	return tableCol{name: name, typ: typ, desc: desc, cell: cell}
}
func wcol(name, typ, desc string, cell func(map[string]any) any) tableCol {
	return tableCol{name: name, typ: typ, priority: 1, desc: desc, cell: cell}
}

// ── built-in printers, keyed by "group/resource" ────────────────────────────

func printerKey(group, resource string) string { return group + "/" + resource }

var builtinPrinters = map[string][]tableCol{}

func registerPrinter(group, resource string, cols ...tableCol) {
	// NAME is always the first column; callers supply the rest in upstream
	// kubectl order, including ageColumn() at the right position (usually after
	// the default columns and before wide columns — but e.g. nodes place it
	// before VERSION).
	full := make([]tableCol, 0, len(cols)+1)
	full = append(full, nameColumn())
	full = append(full, cols...)
	builtinPrinters[printerKey(group, resource)] = full
}

func init() {
	// Nodes
	registerPrinter("", "nodes",
		col("Status", "string", "Node status", nodeStatus),
		col("Roles", "string", "Node roles", nodeRoles),
		ageColumn(),
		col("Version", "string", "Kubelet version", func(o map[string]any) any { return mstr(o, "status", "nodeInfo", "kubeletVersion") }),
		wcol("Internal-IP", "string", "Internal IP", nodeAddr("InternalIP")),
		wcol("External-IP", "string", "External IP", nodeAddr("ExternalIP")),
		wcol("OS-Image", "string", "OS image", func(o map[string]any) any { return mstr(o, "status", "nodeInfo", "osImage") }),
		wcol("Kernel-Version", "string", "Kernel version", func(o map[string]any) any { return mstr(o, "status", "nodeInfo", "kernelVersion") }),
		wcol("Container-Runtime", "string", "Container runtime version", func(o map[string]any) any { return mstr(o, "status", "nodeInfo", "containerRuntimeVersion") }),
	)
	// Pods
	registerPrinter("", "pods",
		col("Ready", "string", "Ready containers", podReady),
		col("Status", "string", "Pod status", podStatus),
		col("Restarts", "integer", "Container restarts", podRestarts),
		ageColumn(),
		wcol("IP", "string", "Pod IP", func(o map[string]any) any { return orNone(mstr(o, "status", "podIP")) }),
		wcol("Node", "string", "Node", func(o map[string]any) any { return orNone(mstr(o, "spec", "nodeName")) }),
		wcol("Nominated Node", "string", "Nominated node", func(o map[string]any) any { return orNone(mstr(o, "status", "nominatedNodeName")) }),
		wcol("Readiness Gates", "string", "Readiness gates", func(o map[string]any) any {
			if n := len(marr(o, "spec", "readinessGates")); n > 0 {
				return strconv.Itoa(n)
			}
			return "<none>"
		}),
	)
	// Deployments / ReplicaSets / StatefulSets / DaemonSets / ReplicationControllers
	registerPrinter("apps", "deployments",
		col("Ready", "string", "Ready/desired replicas", func(o map[string]any) any {
			return fmt.Sprintf("%d/%d", mint(o, "status", "readyReplicas"), mint(o, "spec", "replicas"))
		}),
		col("Up-To-Date", "integer", "Updated replicas", func(o map[string]any) any { return mint(o, "status", "updatedReplicas") }),
		col("Available", "integer", "Available replicas", func(o map[string]any) any { return mint(o, "status", "availableReplicas") }),
		ageColumn(),
		wcol("Containers", "string", "Container names", containersCol),
		wcol("Images", "string", "Container images", imagesCol),
		wcol("Selector", "string", "Selector", selectorCol),
	)
	replicaCols := []tableCol{
		col("Desired", "integer", "Desired replicas", func(o map[string]any) any { return mint(o, "spec", "replicas") }),
		col("Current", "integer", "Current replicas", func(o map[string]any) any { return mint(o, "status", "replicas") }),
		col("Ready", "integer", "Ready replicas", func(o map[string]any) any { return mint(o, "status", "readyReplicas") }),
		ageColumn(),
		wcol("Containers", "string", "Container names", containersCol),
		wcol("Images", "string", "Container images", imagesCol),
		wcol("Selector", "string", "Selector", selectorCol),
	}
	registerPrinter("apps", "replicasets", replicaCols...)
	registerPrinter("", "replicationcontrollers", replicaCols...)
	registerPrinter("apps", "statefulsets",
		col("Ready", "string", "Ready/desired replicas", func(o map[string]any) any {
			return fmt.Sprintf("%d/%d", mint(o, "status", "readyReplicas"), mint(o, "spec", "replicas"))
		}),
		ageColumn(),
		wcol("Containers", "string", "Container names", containersCol),
		wcol("Images", "string", "Container images", imagesCol),
	)
	registerPrinter("apps", "daemonsets",
		col("Desired", "integer", "Desired", func(o map[string]any) any { return mint(o, "status", "desiredNumberScheduled") }),
		col("Current", "integer", "Current", func(o map[string]any) any { return mint(o, "status", "currentNumberScheduled") }),
		col("Ready", "integer", "Ready", func(o map[string]any) any { return mint(o, "status", "numberReady") }),
		col("Up-To-Date", "integer", "Updated", func(o map[string]any) any { return mint(o, "status", "updatedNumberScheduled") }),
		col("Available", "integer", "Available", func(o map[string]any) any { return mint(o, "status", "numberAvailable") }),
		col("Node Selector", "string", "Node selector", func(o map[string]any) any {
			return orNone(mapString(mget(o, "spec", "template", "spec", "nodeSelector")))
		}),
		ageColumn(),
		wcol("Containers", "string", "Container names", containersCol),
		wcol("Images", "string", "Container images", imagesCol),
		wcol("Selector", "string", "Selector", selectorCol),
	)
	// Jobs / CronJobs
	registerPrinter("batch", "jobs",
		col("Status", "string", "Job status", jobStatus),
		col("Completions", "string", "Completions", func(o map[string]any) any {
			comp := mget(o, "spec", "completions")
			c := "1"
			if comp != nil {
				c = strconv.FormatInt(mint(o, "spec", "completions"), 10)
			}
			return fmt.Sprintf("%d/%s", mint(o, "status", "succeeded"), c)
		}),
		ageColumn(),
		wcol("Containers", "string", "Container names", containersCol),
		wcol("Images", "string", "Container images", imagesCol),
		wcol("Selector", "string", "Selector", selectorCol),
	)
	registerPrinter("batch", "cronjobs",
		col("Schedule", "string", "Schedule", func(o map[string]any) any { return mstr(o, "spec", "schedule") }),
		col("Suspend", "string", "Suspend", func(o map[string]any) any {
			if b, _ := mget(o, "spec", "suspend").(bool); b {
				return "True"
			}
			return "False"
		}),
		col("Active", "integer", "Active jobs", func(o map[string]any) any { return int64(len(marr(o, "status", "active"))) }),
		col("Last Schedule", "date", "Last schedule time", func(o map[string]any) any { return orNone(mstr(o, "status", "lastScheduleTime")) }),
		ageColumn(),
		wcol("Containers", "string", "Container names", containersCol),
		wcol("Images", "string", "Container images", imagesCol),
	)
	// Services / Endpoints / Ingress
	registerPrinter("", "services",
		col("Type", "string", "Service type", func(o map[string]any) any { return orNone(mstr(o, "spec", "type")) }),
		col("Cluster-IP", "string", "Cluster IP", func(o map[string]any) any { return orNone(mstr(o, "spec", "clusterIP")) }),
		col("External-IP", "string", "External IP", serviceExternalIP),
		col("Port(s)", "string", "Ports", servicePorts),
		ageColumn(),
		wcol("Selector", "string", "Selector", func(o map[string]any) any { return orNone(mapString(mget(o, "spec", "selector"))) }),
	)
	registerPrinter("", "endpoints",
		col("Endpoints", "string", "Endpoints", endpointsCol),
		ageColumn(),
	)
	registerPrinter("networking.k8s.io", "ingresses",
		col("Class", "string", "Ingress class", func(o map[string]any) any { return orNone(mstr(o, "spec", "ingressClassName")) }),
		col("Hosts", "string", "Hosts", ingressHosts),
		col("Address", "string", "Address", ingressAddress),
		col("Ports", "string", "Ports", func(o map[string]any) any {
			if len(marr(o, "spec", "tls")) > 0 {
				return "80, 443"
			}
			return "80"
		}),
		ageColumn(),
	)
	// Config / storage / identity
	registerPrinter("", "configmaps",
		col("Data", "integer", "Data entries", func(o map[string]any) any {
			return int64(len(asMap(mget(o, "data"))) + len(asMap(mget(o, "binaryData"))))
		}),
		ageColumn(),
	)
	registerPrinter("", "secrets",
		col("Type", "string", "Secret type", func(o map[string]any) any { return mstr(o, "type") }),
		col("Data", "integer", "Data entries", func(o map[string]any) any { return int64(len(asMap(mget(o, "data")))) }),
		ageColumn(),
	)
	registerPrinter("", "persistentvolumeclaims",
		col("Status", "string", "Phase", func(o map[string]any) any { return mstr(o, "status", "phase") }),
		col("Volume", "string", "Bound volume", func(o map[string]any) any { return mstr(o, "spec", "volumeName") }),
		col("Capacity", "string", "Capacity", func(o map[string]any) any { return mstr(o, "status", "capacity", "storage") }),
		col("Access Modes", "string", "Access modes", accessModesCol),
		col("Storageclass", "string", "Storage class", func(o map[string]any) any { return mstr(o, "spec", "storageClassName") }),
		ageColumn(),
	)
	registerPrinter("", "persistentvolumes",
		col("Capacity", "string", "Capacity", func(o map[string]any) any { return mstr(o, "spec", "capacity", "storage") }),
		col("Access Modes", "string", "Access modes", accessModesCol),
		col("Reclaim Policy", "string", "Reclaim policy", func(o map[string]any) any { return mstr(o, "spec", "persistentVolumeReclaimPolicy") }),
		col("Status", "string", "Phase", func(o map[string]any) any { return mstr(o, "status", "phase") }),
		col("Claim", "string", "Bound claim", func(o map[string]any) any {
			ns, n := mstr(o, "spec", "claimRef", "namespace"), mstr(o, "spec", "claimRef", "name")
			if n == "" {
				return ""
			}
			if ns != "" {
				return ns + "/" + n
			}
			return n
		}),
		col("Storageclass", "string", "Storage class", func(o map[string]any) any { return mstr(o, "spec", "storageClassName") }),
		ageColumn(),
	)
	registerPrinter("", "namespaces",
		col("Status", "string", "Phase", func(o map[string]any) any { return mstr(o, "status", "phase") }),
		ageColumn(),
	)
	registerPrinter("", "serviceaccounts",
		col("Secrets", "integer", "Secrets", func(o map[string]any) any { return int64(len(marr(o, "secrets"))) }),
		ageColumn(),
	)
	// Events (both core and events.k8s.io)
	eventCols := []tableCol{
		col("Type", "string", "Event type", func(o map[string]any) any { return mstr(o, "type") }),
		col("Reason", "string", "Reason", func(o map[string]any) any { return firstNonEmpty(mstr(o, "reason")) }),
		col("Object", "string", "Involved object", eventObject),
		col("Message", "string", "Message", func(o map[string]any) any { return firstNonEmpty(mstr(o, "message"), mstr(o, "note")) }),
		ageColumn(),
	}
	registerPrinter("", "events", eventCols...)
	registerPrinter("events.k8s.io", "events", eventCols...)
}

// ── cell helpers ────────────────────────────────────────────────────────────

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func nodeStatus(o map[string]any) any {
	status := "Unknown"
	for _, c := range marr(o, "status", "conditions") {
		cm := asMap(c)
		if mstr(cm, "type") == "Ready" {
			if mstr(cm, "status") == "True" {
				status = "Ready"
			} else {
				status = "NotReady"
			}
		}
	}
	if b, _ := mget(o, "spec", "unschedulable").(bool); b {
		status += ",SchedulingDisabled"
	}
	return status
}

func nodeRoles(o map[string]any) any {
	var roles []string
	for k := range asMap(mget(o, "metadata", "labels")) {
		if r := strings.TrimPrefix(k, "node-role.kubernetes.io/"); r != k {
			if r == "" {
				continue
			}
			roles = append(roles, r)
		}
	}
	sort.Strings(roles)
	if len(roles) == 0 {
		return "<none>"
	}
	return strings.Join(roles, ",")
}

func nodeAddr(typ string) func(map[string]any) any {
	return func(o map[string]any) any {
		for _, a := range marr(o, "status", "addresses") {
			am := asMap(a)
			if mstr(am, "type") == typ {
				return mstr(am, "address")
			}
		}
		return "<none>"
	}
}

func podReady(o map[string]any) any {
	cs := marr(o, "status", "containerStatuses")
	ready := 0
	for _, c := range cs {
		if b, _ := asMap(c)["ready"].(bool); b {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(cs))
}

func podRestarts(o map[string]any) any {
	var n int64
	for _, c := range marr(o, "status", "containerStatuses") {
		n += mint(asMap(c), "restartCount")
	}
	return n
}

// podStatus approximates kubectl's computed pod status: a container waiting/
// terminated reason wins over phase; a deletionTimestamp shows Terminating.
// (Full kubectl logic — init containers, signals — is simplified.)
func podStatus(o map[string]any) any {
	if mstr(o, "metadata", "deletionTimestamp") != "" {
		return "Terminating"
	}
	reason := mstr(o, "status", "phase")
	if r := mstr(o, "status", "reason"); r != "" {
		reason = r
	}
	for _, c := range marr(o, "status", "containerStatuses") {
		st := asMap(asMap(c)["state"])
		if w := asMap(st["waiting"]); w != nil && mstr(w, "reason") != "" {
			reason = mstr(w, "reason")
			break
		}
		if t := asMap(st["terminated"]); t != nil && mstr(t, "reason") != "" {
			reason = mstr(t, "reason")
			break
		}
	}
	return reason
}

func jobStatus(o map[string]any) any {
	for _, c := range marr(o, "status", "conditions") {
		cm := asMap(c)
		if mstr(cm, "status") == "True" {
			switch mstr(cm, "type") {
			case "Complete":
				return "Complete"
			case "Failed":
				return "Failed"
			}
		}
	}
	if mint(o, "status", "active") > 0 || len(marr(o, "status", "active")) > 0 {
		return "Running"
	}
	return "Running"
}

func containersCol(o map[string]any) any {
	var names []string
	for _, c := range marr(o, "spec", "template", "spec", "containers") {
		names = append(names, mstr(asMap(c), "name"))
	}
	return strings.Join(names, ",")
}

func imagesCol(o map[string]any) any {
	var imgs []string
	for _, c := range marr(o, "spec", "template", "spec", "containers") {
		imgs = append(imgs, mstr(asMap(c), "image"))
	}
	return strings.Join(imgs, ",")
}

func selectorCol(o map[string]any) any {
	return orNone(mapString(mget(o, "spec", "selector", "matchLabels")))
}

// mapString formats a string map as "k=v,k2=v2" (sorted). Non-map / empty → "".
func mapString(v any) string {
	m := asMap(v)
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if s, ok := m[k].(string); ok {
			parts = append(parts, k+"="+s)
		}
	}
	return strings.Join(parts, ",")
}

func serviceExternalIP(o map[string]any) any {
	switch mstr(o, "spec", "type") {
	case "LoadBalancer":
		var ips []string
		for _, ing := range marr(o, "status", "loadBalancer", "ingress") {
			im := asMap(ing)
			if ip := mstr(im, "ip"); ip != "" {
				ips = append(ips, ip)
			} else if h := mstr(im, "hostname"); h != "" {
				ips = append(ips, h)
			}
		}
		if len(ips) > 0 {
			return strings.Join(ips, ",")
		}
		return "<pending>"
	case "ExternalName":
		return orNone(mstr(o, "spec", "externalName"))
	}
	var ext []string
	for _, ip := range marr(o, "spec", "externalIPs") {
		if s, ok := ip.(string); ok {
			ext = append(ext, s)
		}
	}
	if len(ext) > 0 {
		return strings.Join(ext, ",")
	}
	return "<none>"
}

func servicePorts(o map[string]any) any {
	var parts []string
	for _, p := range marr(o, "spec", "ports") {
		pm := asMap(p)
		port := mint(pm, "port")
		proto := mstr(pm, "protocol")
		if proto == "" {
			proto = "TCP"
		}
		if np := mint(pm, "nodePort"); np != 0 {
			parts = append(parts, fmt.Sprintf("%d:%d/%s", port, np, proto))
		} else {
			parts = append(parts, fmt.Sprintf("%d/%s", port, proto))
		}
	}
	if len(parts) == 0 {
		return "<none>"
	}
	return strings.Join(parts, ",")
}

func accessModesCol(o map[string]any) any {
	short := map[string]string{"ReadWriteOnce": "RWO", "ReadOnlyMany": "ROX", "ReadWriteMany": "RWX", "ReadWriteOncePod": "RWOP"}
	var modes []string
	src := marr(o, "spec", "accessModes")
	if len(src) == 0 {
		src = marr(o, "status", "accessModes")
	}
	for _, m := range src {
		s, _ := m.(string)
		if sh, ok := short[s]; ok {
			modes = append(modes, sh)
		} else if s != "" {
			modes = append(modes, s)
		}
	}
	return strings.Join(modes, ",")
}

func endpointsCol(o map[string]any) any {
	var eps []string
	for _, s := range marr(o, "subsets") {
		sm := asMap(s)
		for _, a := range marr(sm, "addresses") {
			ip := mstr(asMap(a), "ip")
			for _, p := range marr(sm, "ports") {
				eps = append(eps, fmt.Sprintf("%s:%d", ip, mint(asMap(p), "port")))
				if len(eps) >= 3 {
					return strings.Join(eps, ",") + " + more..."
				}
			}
		}
	}
	if len(eps) == 0 {
		return "<none>"
	}
	return strings.Join(eps, ",")
}

func ingressHosts(o map[string]any) any {
	var hosts []string
	for _, r := range marr(o, "spec", "rules") {
		if h := mstr(asMap(r), "host"); h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return "*"
	}
	return strings.Join(hosts, ",")
}

func ingressAddress(o map[string]any) any {
	var addrs []string
	for _, ing := range marr(o, "status", "loadBalancer", "ingress") {
		im := asMap(ing)
		if ip := mstr(im, "ip"); ip != "" {
			addrs = append(addrs, ip)
		} else if h := mstr(im, "hostname"); h != "" {
			addrs = append(addrs, h)
		}
	}
	return strings.Join(addrs, ",")
}

func eventObject(o map[string]any) any {
	// core Event uses involvedObject; events.k8s.io uses regarding.
	io := asMap(mget(o, "involvedObject"))
	if io == nil {
		io = asMap(mget(o, "regarding"))
	}
	kind, name := mstr(io, "kind"), mstr(io, "name")
	if kind == "" && name == "" {
		return ""
	}
	return strings.ToLower(kind) + "/" + name
}

// ── rendering ───────────────────────────────────────────────────────────────

// renderTableFromColumns builds a meta.k8s.io/v1 Table from columns + decoded
// objects (with their raw bytes for the row.object PartialObjectMetadata). now
// (the replay clock) is used to compute relative AGE cells.
func renderTableFromColumns(cols []tableCol, objs []map[string]any, now time.Time) []byte {
	colDefs := make([]map[string]any, len(cols))
	for i, c := range cols {
		cd := map[string]any{"name": c.name, "type": c.typ, "description": c.desc}
		if c.name == "Name" {
			cd["format"] = "name"
		}
		if c.priority != 0 {
			cd["priority"] = c.priority
		}
		colDefs[i] = cd
	}
	rows := make([]map[string]any, 0, len(objs))
	for _, o := range objs {
		cells := make([]any, len(cols))
		for i, c := range cols {
			switch {
			case c.age:
				cells[i] = humanAge(mstr(o, "metadata", "creationTimestamp"), now)
			case c.cell != nil:
				cells[i] = c.cell(o)
			}
		}
		rows = append(rows, map[string]any{
			"cells": cells,
			"object": map[string]any{
				"kind": "PartialObjectMetadata", "apiVersion": "meta.k8s.io/v1",
				"metadata": mget(o, "metadata"),
			},
		})
	}
	out, _ := json.Marshal(map[string]any{
		"kind": "Table", "apiVersion": "meta.k8s.io/v1",
		"metadata":          map[string]any{"resourceVersion": "0"},
		"columnDefinitions": colDefs,
		"rows":              rows,
	})
	return out
}

// renderResourceTable renders a Table for the given list/single-object JSON body
// using (in order) a built-in printer, the captured CRD's additionalPrinterColumns,
// captured columnDefinitions, or the generic buildTable. Returns ok=false when the
// body isn't decodable (caller falls back).
func (h *handler) renderResourceTable(path string, body []byte, at time.Time) ([]byte, bool) {
	trimmed := strings.TrimSuffix(path, "/")
	group, version, resource, _ := parseAPIPath(trimmed)
	if resource == "" {
		// parseAPIPath only resolves list paths; a single-object GET
		// (.../<resource>/<name>) resolves after dropping the trailing name.
		if i := strings.LastIndex(trimmed, "/"); i > 0 {
			group, version, resource, _ = parseAPIPath(trimmed[:i])
		}
	}
	if resource == "" {
		return nil, false
	}

	// Decode items (list) or a single object.
	var env struct {
		Items []json.RawMessage `json:"items"`
	}
	_ = json.Unmarshal(body, &env)
	raws := env.Items
	if raws == nil {
		raws = []json.RawMessage{json.RawMessage(body)}
	}
	objs := make([]map[string]any, 0, len(raws))
	for _, r := range raws {
		var m map[string]any
		if err := json.Unmarshal(r, &m); err != nil || m == nil {
			continue
		}
		objs = append(objs, m)
	}

	// 1. Built-in printer.
	if cols, ok := builtinPrinters[printerKey(group, resource)]; ok {
		return renderTableFromColumns(cols, objs, at), true
	}
	// 2. CRD additionalPrinterColumns (JSONPath).
	if cols := h.crdPrinterColumns(group, version, resource, at); cols != nil {
		return renderTableFromColumns(cols, objs, at), true
	}
	// 3. Captured columnDefinitions for the kind + metadata-only cells.
	if cols := h.capturedColumns(path, at); cols != nil {
		return renderTableFromColumns(cols, objs, at), true
	}
	// 4. Generic.
	if tb, err := buildTable(body); err == nil {
		return tb, true
	}
	return nil, false
}

// capturedColumns reuses the columnDefinitions from a captured Table for the
// kind, filling only metadata cells (Name/Namespace/Age) — blanks otherwise.
func (h *handler) capturedColumns(path string, at time.Time) []tableCol {
	tb, code, _ := h.store.Latest(strings.TrimSuffix(path, "/")+tableIndexKeySuffix, at)
	if code != 200 {
		return nil
	}
	var t struct {
		ColumnDefinitions []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Priority int    `json:"priority"`
			Desc     string `json:"description"`
		} `json:"columnDefinitions"`
	}
	if err := json.Unmarshal(tb, &t); err != nil || len(t.ColumnDefinitions) == 0 {
		return nil
	}
	cols := make([]tableCol, 0, len(t.ColumnDefinitions))
	for _, cd := range t.ColumnDefinitions {
		c := tableCol{name: cd.Name, typ: cd.Type, priority: cd.Priority, desc: cd.Desc}
		switch strings.ToLower(cd.Name) {
		case "name":
			c.cell = nameCell
		case "namespace":
			c.cell = func(o map[string]any) any { return mstr(o, "metadata", "namespace") }
		case "age":
			c.age = true
		default:
			c.cell = func(map[string]any) any { return "" }
		}
		cols = append(cols, c)
	}
	return cols
}

// crdPrinterColumns builds columns from the captured CRD's
// spec.versions[version].additionalPrinterColumns (JSONPath cells). Returns nil
// when the CRD isn't captured or has no additional columns.
func (h *handler) crdPrinterColumns(group, version, resource string, at time.Time) []tableCol {
	if group == "" {
		return nil // core kinds are never CRDs
	}
	crd := h.findCRD(group, resource, at)
	if crd == nil {
		return nil
	}
	var extra []any
	for _, v := range marr(crd, "spec", "versions") {
		vm := asMap(v)
		if mstr(vm, "name") == version {
			extra = marr(vm, "additionalPrinterColumns")
			break
		}
	}
	if len(extra) == 0 {
		return nil
	}
	cols := []tableCol{nameColumn()}
	for _, c := range extra {
		cm := asMap(c)
		jp := mstr(cm, "jsonPath")
		prio := int(mint(cm, "priority"))
		cols = append(cols, tableCol{
			name: mstr(cm, "name"), typ: mstr(cm, "type"), priority: prio, desc: mstr(cm, "description"),
			cell: jsonPathCell(jp),
		})
	}
	cols = append(cols, ageColumn())
	return cols
}

// findCRD locates the captured CustomResourceDefinition for a group+resource.
func (h *handler) findCRD(group, resource string, at time.Time) map[string]any {
	body, code, err := h.store.ReconstructAt("/apis/apiextensions.k8s.io/v1/customresourcedefinitions", at)
	if err != nil || code != 200 {
		return nil
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil {
		return nil
	}
	for _, raw := range list.Items {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		if mstr(m, "spec", "group") == group && mstr(m, "spec", "names", "plural") == resource {
			return m
		}
	}
	return nil
}

// jsonPathCell compiles a CRD additionalPrinterColumns JSONPath into a cell func.
func jsonPathCell(expr string) func(map[string]any) any {
	if expr == "" {
		return func(map[string]any) any { return "" }
	}
	jp := jsonpath.New("col").AllowMissingKeys(true)
	// k8s CRD JSONPath is a bare path like ".spec.replicas"; jsonpath wants {…}.
	if err := jp.Parse("{" + expr + "}"); err != nil {
		return func(map[string]any) any { return "" }
	}
	return func(o map[string]any) any {
		var sb strings.Builder
		if err := jp.Execute(&sb, o); err != nil {
			return ""
		}
		return sb.String()
	}
}
