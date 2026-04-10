# Capture Config Reference

k8shark reads a YAML config file that controls what gets captured, from which namespaces, and for how long.

## Top-level fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `duration` | duration string | `10m` | Total time to run the capture. Polling continues until time runs out. |
| `output` | string | `./k8shark-<timestamp>.tar.gz` | Path for the output archive. |
| `kubeconfig` | string | `$KUBECONFIG` → `~/.kube/config` | Path to the kubeconfig for the source cluster. |
| `resources` | list | required | Resource types to capture. See below. |

## Resource entry fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `group` | string | yes | API group. Use `""` (empty string) for core group resources (`pods`, `nodes`, etc.). |
| `version` | string | yes | API version, e.g. `v1`, `v1beta1`. |
| `resource` | string | yes | Plural resource name, e.g. `pods`, `deployments`. |
| `namespaces` | list of strings | no | Namespaces to poll. **Omit entirely for cluster-scoped resources** (nodes, persistentvolumes, storageclasses, etc.). |
| `interval` | duration string | `30s` | How often to re-poll this resource during the capture window. |

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

## Duration and interval units

Both `duration` and `interval` accept Go duration strings:

| String | Meaning |
|--------|---------|
| `30s` | 30 seconds |
| `5m` | 5 minutes |
| `1h` | 1 hour |
| `90s` | 90 seconds |

The first poll for each resource happens immediately when the capture starts; subsequent polls fire on the interval.
