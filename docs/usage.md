# Usage

## Prerequisites

- `kubectl` in your `PATH`
- A valid `~/.kube/config` (or `KUBECONFIG` env set) pointing at a cluster — for `capture` only
- Go 1.22+ if building from source

## Installation

### Homebrew

```sh
brew install phenixblue/tap/k8shark
```

### go install

```sh
go install github.com/phenixblue/k8shark@latest
```

The binary is named `kshrk`.

### From source

```sh
git clone https://github.com/phenixblue/k8shark
cd k8shark
make build
# binary written to ./kshrk
```

---

## Capture

Run `kshrk capture` while connected to the cluster. It polls the Kubernetes API at defined intervals and packages all responses into a `.tar.gz` archive.

```sh
kshrk capture --config k8shark.yaml
```

The command shows a spinner while running, then prints a summary:

```
Starting capture -> ./capture.tar.gz
  capturing |
Capture complete
  Output:    ./capture.tar.gz (1.2 MB)
  Records:   480 across 12 resource path(s)
  Duration:  10m0s
```

### Capture flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--config` | | `./k8shark.yaml` | Path to config file |
| `--output` | `-o` | `./k8shark-<timestamp>.tar.gz` | Output archive path |
| `--duration` | | from config | Override capture duration (e.g. `5m`) |
| `--kubeconfig` | | `$KUBECONFIG` / `~/.kube/config` | Source cluster kubeconfig |
| `--verbose` | `-v` | false | Log every API path as it is fetched |
| `--redact-secrets` | | false | Redact Secret `data`/`stringData` values from the archive after capture |
| `--allow-secret` | | | `namespace/name` of secret to preserve when `--redact-secrets` is set (repeatable) |

The `--config` flag auto-discovers `./k8shark.yaml` if not specified.

---

## Validate

`kshrk validate` parses and validates a capture config file without connecting to a cluster or making any API calls. Use it to catch mistakes before starting a capture.

```sh
kshrk validate --config k8shark.yaml
```

On success it prints a summary:

```
✓ Config valid (10 resources, 4 namespaces, duration 10m)
```

Errors exit non-zero:

```
error: resources[2] (pods): invalid interval "0s": must be > 0
```

Warnings are printed to stderr but do **not** cause a non-zero exit:

```
warning: resources[5] (storageclasses): cluster-scoped resource has 'namespaces:' set — this will be ignored at capture time
warning: resources[3] (events): interval 2s is very short and may produce a large archive
```

### Checks performed

**Hard errors** (exit 1):
- Missing `resource` or `version` field
- Unparseable `duration` or `interval` strings (e.g. `"0s"` interval)
- `logs` < 0

**Warnings** (exit 0, printed to stderr):
- Capture `duration` > 2 h — may produce a very large archive
- Resource `interval` < 5 s — may produce a very large archive
- Well-known cluster-scoped resource (`nodes`, `persistentvolumes`,
  `storageclasses`, `namespaces`, `clusterroles`, etc.) with `namespaces:` set
  — the capture engine auto-corrects this at runtime but it is likely a mistake
- `output` path already exists — the file will be overwritten

---

## Inspect

`kshrk inspect` reads a capture archive and prints a summary of its contents without starting a server.

```sh
kshrk inspect capture.tar.gz
```

Example output:

```
Capture ID:   a1b2c3d4-e5f6-7890-abcd-ef1234567890
Captured:     2026-04-09T08:00:00Z → 2026-04-09T08:10:00Z  (10m0s)
Kubernetes:   v1.29.0
Server:       https://192.168.1.100:6443
Archive:      capture.tar.gz (1245184 bytes)
Records:      480

RESOURCE              GROUP  VERSION  NAMESPACED  NAMESPACES              RECORDS
deployments           apps   v1       yes         default,production      80
nodes                        v1       no          -                       10
pods                         v1       yes         default,kube-system     320
secrets                      v1       yes         default                 40
statefulsets          apps   v1       yes         production              30
```

Use `-o json` or `-o yaml` for machine-readable output:

```sh
kshrk inspect capture.tar.gz -o json
kshrk inspect capture.tar.gz -o yaml
```

### Inspect flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--output` | `-o` | `table` | Output format: `table`, `json`, or `yaml` |

---

## Open

`kshrk open` extracts the archive, starts a local mock HTTPS API server on `127.0.0.1`, and writes a kubeconfig pointing at it.

```sh
kshrk open capture.tar.gz
```

Output:

```
k8shark mock server running
  Address:    https://127.0.0.1:54321
  Kubeconfig: ~/.kube/k8shark-abc123def456.yaml

Run: export KUBECONFIG=~/.kube/k8shark-abc123def456.yaml
Then use kubectl normally against the capture.

Press Ctrl+C to stop.
```

Set `KUBECONFIG` and use `kubectl` as you would against a live cluster:

```sh
export KUBECONFIG=~/.kube/k8shark-abc123def456.yaml

kubectl get nodes
kubectl get pods -A
kubectl get pods -n production -o yaml
kubectl describe deployment my-app -n production
kubectl get pvc -n staging
kubectl top pods -n production   # not supported — only read API calls
```

The server stays running until `Ctrl+C`.

### Open flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | random | Port for the mock API server |
| `--kubeconfig-out` | `~/.kube/k8shark-<id>.yaml` | Where to write the generated kubeconfig |
| `--at` | latest records | Pin replay to a specific timestamp (see below) |
| `--verbose` / `-v` | false | Log every request the server receives |

---

## Time-travel replay with `--at`

Every captured record is timestamped. `--at` lets you replay the capture as it looked at a specific point in time — the server returns the most recent record for each resource whose timestamp is ≤ the given time.

```sh
kshrk open capture.tar.gz --at 2026-04-09T10:30:00Z
```

The timestamp must be in RFC3339 format. Use UTC (`Z`) or include an offset (`+05:30`).

This is useful when you have a long capture (e.g. 1h) and want to compare cluster state before and after an incident.

---

## Redact

Secret values can be removed from a capture archive in two ways:

### Option A — `kshrk redact` (post-capture)

Produces a **new** archive with all Kubernetes Secret `data` and `stringData` values replaced by `"REDACTED"`. The original archive is not modified.

```sh
kshrk redact --in capture.tar.gz
# writes capture-redacted.tar.gz by default
```

```sh
kshrk redact --in capture.tar.gz --out safe-capture.tar.gz \
  --allow-secret default/pull-secret \
  --allow-secret kube-system/bootstrap-token
```

### Option B — `--redact-secrets` flag on `kshrk capture` (at collection time)

Pass `--redact-secrets` to have the archive redacted **in-place** immediately after capture completes, before any data is stored on disk in final form:

```sh
kshrk capture --config k8shark.yaml --redact-secrets
kshrk capture --config k8shark.yaml --redact-secrets \
  --allow-secret default/pull-secret
```

The final archive at the configured output path will already be redacted. No intermediate unredacted file is retained.

### Redact flags (both commands)

| Flag | Command | Default | Description |
|------|---------|---------|-------------|
| `--in` | `redact` | (required) | Source capture archive |
| `--out` | `redact` | `<in>-redacted.tar.gz` | Output archive path |
| `--redact-secrets` | `capture` | false | Redact secrets in-place after capture |
| `--allow-secret` | both | | `namespace/name` of secret to preserve (repeatable) |

Secret metadata (name, namespace, labels, annotations, type) is always preserved so you can still count and identify secrets by kind.

---

## kubectl compatibility

The mock server is designed to be a drop-in replacement for `kubectl`'s real server. Supported behaviours:

| kubectl feature | Status |
|-----------------|--------|
| `kubectl get <resource>` | ✅ |
| `kubectl get <resource> -n <ns>` | ✅ |
| `kubectl get <resource> -A` | ✅ aggregates across captured namespaces |
| `kubectl get <resource> <name>` | ✅ extracted from parent list |
| `kubectl get -o yaml / -o json` | ✅ |
| `kubectl get -o wide` | ✅ uses captured Table column definitions |
| `kubectl describe` | ✅ |
| `kubectl explain` | ✅ uses captured OpenAPI spec |
| Short names (`po`, `svc`, `deploy`, `pvc`, `pv`, …) | ✅ |
| Label selectors (`-l app=foo`) | ✅ |
| Field selectors (`--field-selector status.phase=Running`) | ✅ |
| Watch (`-w`) | ✅ synthetic event stream |
| Write operations (`apply`, `delete`, `edit`, …) | ⛔ returns `405 Method Not Allowed` — mock server is read-only |
| `kubectl exec` / `kubectl cp` / `kubectl port-forward` / `kubectl attach` | ⛔ returns `405 Method Not Allowed` with a clear error referencing k8shark capture replay |
| `kubectl logs` | ✅ if captured via `logs: N` in capture config; helpful stub returned when not captured |
| `kubectl top` | ❌ metrics API not captured |

---

## Capturing pod logs

To capture pod logs alongside resource state, add a `logs` field to a `pods` resource entry in your capture config. The value is the number of tail lines to capture per pod:

```yaml
resources:
  - version: v1
    resource: pods
    namespaces: [production, staging]
    interval: 30s
    logs: 200       # capture last 200 lines from each pod
```

Log content is fetched once at the end of the capture run and stored in the archive. When you open the archive with `kshrk open`, `kubectl logs` returns the captured output:

```sh
kubectl logs my-app-pod-abc123 -n production
# outputs the captured tail log
```

If a pod's logs were not captured (e.g. `logs` was omitted or set to `0`), `kubectl logs` returns a clear stub message instead of an error:

```
# k8shark capture replay: logs were not captured for this pod.
# To capture logs, add 'logs: 200' (or another line count) to the
# pods entry in your k8shark capture config and re-run the capture.
```

> **Note:** Large log volumes increase archive size. Use a reasonable tail-line limit (e.g. 100–500 lines). Previous-container logs (`kubectl logs --previous`) are not captured.

### Resources not in the capture

If you run `kubectl get pvc` but PVCs were not included in the capture config, the server returns an empty list with a `Warning: 299` header rather than an error. kubectl displays:

```
No resources found in default namespace.
Warning: persistentvolumeclaims not found in capture; was it included in the capture config?
```

This is intentional — it avoids confusing `Error from server:` output for resources that simply weren't captured.

---

## Client compatibility

The k8shark mock API server is designed to work with any read-only Kubernetes
client, not just `kubectl`. This section documents what is supported, what
requires special capture config, and what is intentionally unsupported.

### Works out of the box

These operations work against any k8shark archive with no special config:

| Client / Command | Notes |
|-----------------|-------|
| `kubectl get`, `kubectl describe` | Full support |
| `kubectl logs` | Requires `logs: N` in capture config — see above |
| `kubectl api-resources` | Synthesised from index if discovery wasn't captured |
| `kubectl explain` | Works if OpenAPI spec was captured (always attempted) |
| `kubectl get --watch` | Synthetic watch stream; emits ADDED + BOOKMARK |
| `helm list`, `helm status` | Reads Secrets for release state — works if captured |
| `k9s` (read-only browsing) | Full support — API discovery + resource listing |

### Requires CRD resources to be captured

These tools or commands require CRD-backed resources to be present in the
archive. Use `auto_discover: true` or explicit config entries (see
[Capturing CRD-backed resources](config.md#capturing-crd-backed-resources)).

| Client / Command | Required resources |
|-----------------|--------------------|
| `istioctl analyze` | `networking.istio.io`, `security.istio.io` CRDs |
| `istioctl describe pod` | pods, services, VirtualServices, DestinationRules |
| `istioctl x precheck` | Resource lists + RBAC resources |
| `argocd app get` | `applications.argoproj.io`, `appprojects.argoproj.io` |
| `flux get all` | Flux toolkit CRDs (`kustomize.toolkit.fluxcd.io`, etc.) |

### Intentionally unsupported (always 405)

These operations require a live cluster and cannot be replayed from a snapshot.
The server returns `405 Method Not Allowed` with a clear message rather than
hanging or returning a confusing error.

| Operation | Why unsupported |
|-----------|----------------|
| `kubectl exec` / `kubectl cp` | Requires a running container |
| `kubectl port-forward` | Requires a running pod |
| `kubectl attach` | Requires a running container |
| Pod/service proxy (`/proxy/`) | Requires a running in-cluster service |
| `istioctl proxy-status` | Requires gRPC connection to Istiod |
| Istiod xDS / gRPC endpoints | Out of scope for a replay server |
| All write operations (POST/PUT/PATCH/DELETE) | Replay is read-only |

### Using non-kubectl clients with kshrk open

`kshrk open` writes a kubeconfig file (`kubectl config view`) that points at
the mock server. Any tool that can be configured with a kubeconfig or a
`--kubeconfig` flag will work:

```sh
# Start the mock server
kshrk open capture.tar.gz
# Note the printed kubeconfig path, e.g. /tmp/k8shark-kubeconfig-1234

# Use with istioctl
istioctl analyze --kubeconfig /tmp/k8shark-kubeconfig-1234

# Use with helm
helm list --kubeconfig /tmp/k8shark-kubeconfig-1234 --all-namespaces

# Use with flux
flux get all --kubeconfig /tmp/k8shark-kubeconfig-1234

# Use with k9s
k9s --kubeconfig /tmp/k8shark-kubeconfig-1234
```

> **Tip:** Export `KUBECONFIG=/tmp/k8shark-kubeconfig-1234` to make all tools
> in your shell session use the mock server automatically.
