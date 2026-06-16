# Capture Config Reference

k8shark reads a YAML config file that controls what gets captured, from which namespaces, and for how long.

## Top-level fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `duration` | duration string | `10m` | Total time to run the capture. Polling continues until time runs out. |
| `output` | string | `./k8shark-<timestamp>.tar.gz` | Path for the output archive. |
| `kubeconfig` | string | `$KUBECONFIG` → `~/.kube/config` | Path to the kubeconfig for the source cluster. |
| `resources` | list | required | Resource types to capture. See below. |
| `auto_discover` | bool | `false` | Legacy global discovery toggle. Prefer `resources: - all: true` for fine-grained control. |
| `redaction` | object | — | Optional field-level redaction rules applied after capture. See [Redaction](#redaction). |

## Resource entry fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `all` | bool | no | If `true`, this entry is a discovery directive. k8shark discovers API resources and expands them into concrete capture entries. |
| `scope` | string | no | Used with `all: true`. Allowed values: `namespaced`, `cluster`, or omitted (both). |
| `group` | string | yes | API group. Use `""` (empty string) for core group resources (`pods`, `nodes`, etc.). |
| `version` | string | yes | API version, e.g. `v1`, `v1beta1`. |
| `resource` | string | yes | Plural resource name, e.g. `pods`, `deployments`. |
| `namespaces` | list of strings | no | Namespaces to poll. **Omit entirely for cluster-scoped resources** (nodes, persistentvolumes, storageclasses, etc.). |
| `interval` | duration string | `30s` | How often to re-poll this resource during the capture window. |
| `dedup` | bool | `true` | Skip writing a record when the response body is identical to the previous poll for the same API path. Set `false` to force writing every poll. |
| `watch` | bool | `false` | Start a Kubernetes watch stream (`?watch=1`) for this resource in addition to polling. Watch events are recorded with an `event_type` field. |
| `logs` | int | `0` | (pods only) Tail-line count for per-container log capture. `0` disables. Logs are stored per (pod, container) so `kubectl logs <pod> -c <c>` replays correctly. |
| `previousLogs` | bool | `false` | (pods only, requires `logs > 0`) Also capture each container's previous-container log via `?previous=true`. Useful for CrashLoopBackOff pods where the interesting output is in the terminated previous container. |

When `all: true` is set, `group`/`version`/`resource` are not required for that entry.

### Simplified discovery with `all: true`

Use `all: true` to auto-discover resources via Kubernetes API discovery (including CRDs):

```yaml
duration: 10m
output: ./cluster-snapshot.tar.gz

resources:
  - all: true
```

Scope filtering:

```yaml
resources:
  - all: true
    scope: namespaced  # or cluster
```

Discovery with namespace restriction for namespaced resources:

```yaml
resources:
  - all: true
    scope: namespaced
    namespaces: [production, staging]
    interval: 20s
```

Mix discovered entries with explicit overrides:

```yaml
resources:
  - all: true
  - group: mygroup.io
    version: v1
    resource: widgets
    namespaces: [default]
    interval: 10s
```

### Namespaced vs. cluster-scoped resources

- **Namespaced resources** (pods, deployments, services, PVCs, etc.): specify `namespaces:` with a list of namespaces to capture. Without `namespaces:`, the resource is skipped.
- **Cluster-scoped resources** (nodes, persistentvolumes, storageclasses, namespaces, clusterroles, etc.): **do not include `namespaces:`**. k8shark fetches these from the cluster root path (e.g. `/api/v1/nodes`).

> **Warning**: if you include `namespaces:` for a cluster-scoped resource, k8shark will warn during capture and automatically fall back to the correct cluster-scoped path.

### Wildcard namespaces

Use `"*"` as a namespace value to automatically capture from all namespaces discoverable at capture start:

```yaml
- group: apps
  version: v1
  resource: deployments
  namespaces: ["*"]
  interval: 30s
```

**Behaviour:**
- `namespaces: ["*"]` expands to every namespace present at the start of the capture. Namespaces created _during_ the capture are not included.
- Mixed lists such as `namespaces: ["production", "*"]` are supported — explicit namespaces appear first, then all remaining discovered namespaces are appended, deduplicated.
- If namespace discovery fails (e.g. RBAC permissions), the capture exits with a clear error.
- A well-known cluster-scoped resource (nodes, persistentvolumes, etc.) with `namespaces: ["*"]` emits a warning and falls back to a cluster-scoped fetch.

### Response deduplication

By default, k8shark writes the first poll result for each API path and skips
consecutive polls whose response body bytes are unchanged. This can
significantly reduce archive size for stable resources.

Use `dedup: false` on a resource when every poll must be preserved:

```yaml
- group: ""
  version: v1
  resource: events
  namespaces: [default]
  interval: 10s
  dedup: false
```

### Watch streaming alongside polling

Set `watch: true` to supplement polling with watch events. This captures
short-lived changes that may occur between poll intervals.

```yaml
- group: ""
  version: v1
  resource: pods
  namespaces: [default]
  interval: 30s
  watch: true
```

Behavior:

- poll-based capture remains active (first poll + interval polling)
- watch events are recorded with `event_type` values such as `ADDED`,
  `MODIFIED`, and `DELETED`
- watch streams reconnect automatically when disconnected

## Example configs

### Minimal: just pods in one namespace

```yaml
duration: 5m
output: ./capture.tar.gz

resources:
  - group: ""
    version: v1
    resource: pods
    namespaces: [default]
    interval: 30s
```

### Workload snapshot: pods, deployments, nodes

```yaml
duration: 10m
output: ./workload-capture.tar.gz

resources:
  - group: ""
    version: v1
    resource: pods
    namespaces: [default, kube-system, production]
    interval: 30s

  - group: ""
    version: v1
    resource: nodes
    interval: 60s

  - group: apps
    version: v1
    resource: deployments
    namespaces: [default, production]
    interval: 30s

  - group: apps
    version: v1
    resource: daemonsets
    namespaces: [kube-system]
    interval: 60s

  - group: ""
    version: v1
    resource: events
    namespaces: [default, production]
    interval: 10s
```

### Storage troubleshooting: PVCs, PVs, StorageClasses

```yaml
duration: 10m
output: ./storage-capture.tar.gz

resources:
  - group: ""
    version: v1
    resource: persistentvolumeclaims
    namespaces: [default, production, staging]
    interval: 30s

  - group: ""
    version: v1
    resource: persistentvolumes   # cluster-scoped — no namespaces:
    interval: 30s

  - group: "storage.k8s.io"
    version: v1
    resource: storageclasses      # cluster-scoped — no namespaces:
    interval: 60s

  - group: ""
    version: v1
    resource: pods
    namespaces: [default, production, staging]
    interval: 30s
```

### Full production support capture

```yaml
duration: 30m
output: ./support-capture.tar.gz
kubeconfig: /path/to/customer.kubeconfig

resources:
  # Core workloads
  - group: ""
    version: v1
    resource: pods
    namespaces: [default, kube-system, production, staging]
    interval: 30s

  - group: ""
    version: v1
    resource: nodes
    interval: 60s

  - group: ""
    version: v1
    resource: events
    namespaces: [default, production, staging]
    interval: 10s

  # Controllers
  - group: apps
    version: v1
    resource: deployments
    namespaces: [default, production, staging]
    interval: 30s

  - group: apps
    version: v1
    resource: daemonsets
    namespaces: [default, kube-system, production]
    interval: 30s

  - group: apps
    version: v1
    resource: statefulsets
    namespaces: [default, production, staging]
    interval: 30s

  - group: apps
    version: v1
    resource: replicasets
    namespaces: [default, production, staging]
    interval: 30s

  # Jobs
  - group: batch
    version: v1
    resource: jobs
    namespaces: [default, production, staging]
    interval: 30s

  - group: batch
    version: v1
    resource: cronjobs
    namespaces: [default, production, staging]
    interval: 60s

  # Networking
  - group: ""
    version: v1
    resource: services
    namespaces: [default, production, staging]
    interval: 60s

  - group: networking.k8s.io
    version: v1
    resource: ingresses
    namespaces: [default, production, staging]
    interval: 60s

  # Storage
  - group: ""
    version: v1
    resource: persistentvolumeclaims
    namespaces: [default, production, staging]
    interval: 30s

  - group: ""
    version: v1
    resource: persistentvolumes
    interval: 60s

  - group: "storage.k8s.io"
    version: v1
    resource: storageclasses
    interval: 60s

  # Config
  - group: ""
    version: v1
    resource: configmaps
    namespaces: [default, production, staging]
    interval: 60s
```

---

## Redaction

k8shark can redact sensitive field values after capture, producing a sanitised archive safe for sharing. Redaction rules live in a top-level `redaction` block and are also applied when running `kshrk redact`.

### `redaction` fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `redactSecrets` | bool | `false` | Redact all Kubernetes `Secret` `data` and `stringData` values (same as `--redact-secrets` on the CLI). |
| `allowSecrets` | list of strings | `[]` | `namespace/name` keys of secrets to preserve even when `redactSecrets: true`. |
| `rules` | list | `[]` | Field-level redaction rules. See below. |

### Redaction rule fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `fieldPath` | string | yes | JSONPath-like expression targeting the field(s) to redact. |
| `kind` | string | no | Resource kind to match, e.g. `Pod`, `ConfigMap`. Use `*` or omit to match all kinds. |
| `namespace` | string | no | Only apply rule to resources in this namespace. Omit to match all namespaces. |
| `labelSelector` | string | no | Kubernetes label selector used to scope matching resources (for example `app=sensitive,tier in (backend)`). |
| `replacement` | string | yes | Value to write in place of the redacted field. Converted to the appropriate JSON type. |
| `valueType` | string | no | Force a specific type for the replacement: `string`, `integer`, `number`, `bool`, `array`, `object`. Inferred from actual value when omitted. |

### Field path syntax

| Pattern | Meaning |
|---------|---------|
| `spec.replicas` | Exact dot-notation path |
| `spec.containers[*].env[*].value` | Wildcards through arrays |
| `spec.containers[0].name` | Specific array index |
| `**.password` | Recursive descent — matches `password` at any depth |

### Type-aware replacement

Redacted values preserve the original field's JSON type so the archive remains schema-compatible with `kubectl` and other tooling.

| Original value | Replacement string | Written as |
|----------------|--------------------|------------|
| `"my-token"` (string) | `"REDACTED"` | `"REDACTED"` |
| `3` (integer) | `"0"` | `0` |
| `true` (bool) | `"false"` | `false` |
| `[...]` (array) with `valueType: array` | any | `[]` |
| `{...}` (object) with `valueType: object` | any | `{}` |

When `valueType` is omitted the engine infers the type in this order:

1. known Kubernetes schema for the matched `kind` + `fieldPath`
2. runtime type from the captured value (fallback)

Invalid replacements (e.g. `"not-a-number"` for an integer field) fail early with a clear error.

### Example: unified config with redaction

```yaml
duration: 10m
output: ./capture.tar.gz
kubeconfig: ~/.kube/config

resources:
  - all: true

redaction:
  redactSecrets: true
  allowSecrets:
    - default/app-secret

  rules:
    # Redact ConfigMap API keys
    - fieldPath: "data.api-key"
      kind: ConfigMap
      replacement: "REDACTED"

    # Redact all Pod env var values
    - fieldPath: "spec.containers[*].env[*].value"
      kind: Pod
      replacement: "REDACTED"
      valueType: string

    # Recursive descent — any field named "password" anywhere
    - fieldPath: "**.password"
      kind: "*"
      replacement: "REDACTED"

    # Numeric field — preserve integer type
    - fieldPath: "spec.replicas"
      kind: Deployment
      replacement: "0"
      valueType: integer

    # Boolean field
    - fieldPath: "spec.automountServiceAccountToken"
      kind: Pod
      replacement: "false"
      valueType: bool

    # Namespace-scoped rule
    - fieldPath: "data.db-password"
      kind: ConfigMap
      namespace: production
      labelSelector: "app=sensitive"
      replacement: "REDACTED"
```

The same config can be passed to `kshrk redact` to apply exactly the same rules to an existing archive:

```bash
kshrk redact --in capture.tar.gz --out redacted.tar.gz --config k8shark.yaml
```

---

## Duration and interval units

Both `duration` and `interval` accept Go duration strings:

| String | Meaning |
|--------|---------|
| `30s` | 30 seconds |
| `5m` | 5 minutes |
| `1h` | 1 hour |
| `90s` | 90 seconds |

The first poll for each resource happens immediately when the capture starts; subsequent polls fire on the interval.

---

## Capturing CRD-backed resources

Custom Resource Definitions (CRDs) — Istio, cert-manager, Flux, ArgoCD,
Crossplane, Kyverno, etc. — are captured exactly like built-in resources. Use
the `group`, `version`, and `resource` fields from `kubectl api-resources`.

### Finding group / version / resource values

```bash
kubectl api-resources --api-group=networking.istio.io
# NAME              SHORTNAMES   APIVERSION                         NAMESPACED
# destinationrules  dr           networking.istio.io/v1beta1        true
# gateways          gw           networking.istio.io/v1beta1        true
# virtualservices   vs           networking.istio.io/v1beta1        true
```

### Explicit CRD entries

```yaml
duration: 10m
output: ./istio-capture.tar.gz

resources:
  - group: ""
    version: v1
    resource: pods
    namespaces: ["*"]
    interval: 30s

  # Istio networking
  - group: networking.istio.io
    version: v1beta1
    resource: virtualservices
    namespaces: ["*"]
    interval: 30s

  - group: networking.istio.io
    version: v1beta1
    resource: destinationrules
    namespaces: ["*"]
    interval: 30s

  - group: networking.istio.io
    version: v1beta1
    resource: gateways
    namespaces: ["*"]
    interval: 30s

  # cert-manager — cluster-scoped CRD (no namespaces:)
  - group: cert-manager.io
    version: v1
    resource: clusterissuers
    interval: 60s

  # cert-manager — namespaced CRD
  - group: cert-manager.io
    version: v1
    resource: certificates
    namespaces: ["*"]
    interval: 60s
```

> **Tip**: `kshrk validate --config <file>` warns when a non-core resource has
> `namespaces:` set for what might be a cluster-scoped CRD (e.g. `ClusterIssuer`).
> Remove `namespaces:` from those entries.

### Auto-discovery (capture all installed CRDs automatically)

Instead of enumerating every CRD in the config, set `auto_discover: true` to
have k8shark walk the cluster's `/apis` endpoint at capture time and
automatically add every non-core resource type it finds.

```yaml
duration: 10m
output: ./full-capture.tar.gz
auto_discover: true

# Explicit entries are still captured and take precedence over auto-discovered
# duplicates.  You can combine the two approaches.
resources:
  - group: ""
    version: v1
    resource: pods
    namespaces: ["*"]
    interval: 30s
    logs: 100

  - group: ""
    version: v1
    resource: nodes
    interval: 60s
```

**Auto-discovery behaviour:**
- Walks `/apis` once at the start of the capture.
- For each discovered non-core group-version, reads the resource list and adds
  an entry for every non-sub-resource type.
- Namespaced resources automatically use `namespaces: ["*"]` (wildcard
  expansion applies).
- Cluster-scoped resources are captured without a namespace.
- The following system groups are excluded by default:

  | Group | Reason |
  |-------|--------|
  | `metrics.k8s.io` | Live-only metrics, not persistent state |
  | `apiregistration.k8s.io` | API aggregation layer internals |
  | `apiextensions.k8s.io` | CRD definitions themselves (not instances) |
  | `authentication.k8s.io` | Token review — live-only |
  | `authorization.k8s.io` | Access review — live-only |

- Add your own exclusions with `auto_discover_exclude_groups`:

```yaml
auto_discover: true
auto_discover_exclude_groups:
  - metrics.k8s.io
  - my-internal.company.io
```

### Ecosystem-specific examples

#### Flux CD

```yaml
auto_discover: true
auto_discover_exclude_groups:
  - metrics.k8s.io

resources:
  - group: ""
    version: v1
    resource: pods
    namespaces: ["*"]
    interval: 30s
```

Flux CRDs (`kustomizations.kustomize.toolkit.fluxcd.io`,
`helmreleases.helm.toolkit.fluxcd.io`, etc.) will be picked up automatically
by `auto_discover: true`.

#### ArgoCD

```yaml
auto_discover: true

resources:
  - group: ""
    version: v1
    resource: pods
    namespaces: [argocd]
    interval: 30s
```

ArgoCD CRDs (`applications.argoproj.io`, `appprojects.argoproj.io`) are
captured automatically via `auto_discover`.
