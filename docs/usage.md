# Usage

## Prerequisites

- `kubectl` in your `PATH`
- A valid `~/.kube/config` (or `KUBECONFIG` env set) pointing at a cluster — for `capture` only
- Go 1.26+ if building from source

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

### Shell completion

`kshrk` ships tab-completion for `bash`, `zsh`, `fish`, and PowerShell. It
completes subcommand and `--flag` names, scopes positional and archive-valued
flags (`--in`, `--out`, `--before`, `--after`, `--archive`, and `capture
--output`) to `*.kshrk` files, restricts `--config` to YAML files, and offers
the valid choices for output formats (e.g. `-o table|json|yaml`).

Generate the script for your shell with `kshrk completion <shell>`:

```sh
# zsh — write into a directory on your fpath, then restart the shell
kshrk completion zsh > "${fpath[1]}/_kshrk"

# bash — Linux
kshrk completion bash > /etc/bash_completion.d/kshrk
# bash — macOS (Homebrew bash-completion@2)
kshrk completion bash > "$(brew --prefix)/etc/bash_completion.d/kshrk"

# fish
kshrk completion fish > ~/.config/fish/completions/kshrk.fish

# PowerShell — add to your profile to persist across sessions
kshrk completion powershell | Out-String | Invoke-Expression
```

To try completion in the current shell without installing it, source the script
directly, e.g. `source <(kshrk completion bash)`. Run
`kshrk completion <shell> --help` for shell-specific installation notes.

> **kubectl plugin:** when `kshrk` is installed as the `kubectl-k8shark` plugin
> (see [#27](https://github.com/phenixblue/k8shark/issues/27)), `kubectl`
> drives completion through its own plugin-completion mechanism rather than the
> scripts above.

---

## Capture

Run `kshrk capture` while connected to the cluster. It polls the Kubernetes API at defined intervals and packages all responses into a `.kshrk` archive.

```sh
kshrk capture --config k8shark.yaml
```

The command shows a spinner while running, then prints a summary:

```
Starting capture -> ./capture.kshrk
  capturing |
Capture complete
  Output:    ./capture.kshrk (1.2 MB)
  Records:   480 across 12 resource path(s)
  Duration:  10m0s
```

### Capture flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--config` | | `./config.yaml`, then `~/.config/kshrk/config.yaml` | Path to config file |
| `--output` | `-o` | `./k8shark-<timestamp>.kshrk` | Output archive path |
| `--duration` | | from config | Override capture duration (e.g. `5m`) |
| `--kubeconfig` | | `$KUBECONFIG` / `~/.kube/config` | Source cluster kubeconfig |
| `--auto-discover` | | false | Auto-discover and capture all available API resources |
| `--verbose` | `-v` | false | Log every API path as it is fetched |
| `--redact-secrets` | | false | Redact Secret `data`/`stringData` values from the archive after capture |
| `--allow-secret` | | | `namespace/name` of secret to preserve when `--redact-secrets` is set (repeatable) |
| `--redact-field` | | | Field redaction rule applied after capture: `<path>:<Kind>:<replacement>[:<type>]` (repeatable) |

If `--config` is not specified, k8shark looks for `./config.yaml` in the current directory first, then falls back to `~/.config/kshrk/config.yaml`.

For full-cluster capture without enumerating resources manually, prefer config syntax:

```yaml
resources:
  - all: true
```

`--auto-discover` is a convenience override that enables global discovery mode.

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
kshrk inspect capture.kshrk
```

Example output:

```
Capture ID:   a1b2c3d4-e5f6-7890-abcd-ef1234567890
Captured:     2026-04-09T08:00:00Z → 2026-04-09T08:10:00Z  (10m0s)
Kubernetes:   v1.29.0
Server:       https://192.168.1.100:6443
Archive:      capture.kshrk (1245184 bytes)
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
kshrk inspect capture.kshrk -o json
kshrk inspect capture.kshrk -o yaml
```

### Inspect flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--output` | `-o` | `table` | Output format: `table`, `json`, or `yaml` |

---

## Diagnose

`kshrk diagnose` analyzes a capture and prints **severity-ranked findings** — likely problems and their remediation — without starting a server. It is the offline equivalent of tools like popeye / kube-score, run against a `.kshrk` archive.

```sh
kshrk diagnose capture.kshrk
```

Example table output:

```
SEVERITY  CATEGORY    OBJECT              FINDING
CRITICAL  workload    prod/web-rs (+2)    CrashLoopBackOff — CrashLoopBackOff
CRITICAL  workload    prod/cache          OOMKilled — OOMKilled
WARNING   scheduling  prod/batch          Pod cannot be scheduled — 0/3 nodes available: insufficient cpu
WARNING   storage     prod/data-claim     PersistentVolumeClaim not bound — phase=Pending, storageClass=missing-sc

4 finding(s): 2 critical, 2 warning, 0 info
```

### Diagnose flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output` / `-o` | `table` | Output format: `table`, `json`, or `yaml` |
| `--at` | latest | Analyze state at a timestamp (RFC3339 or relative duration like `-5m`); must be within the capture window |
| `--severity` | `info` | Minimum severity to report: `info`, `warning`, or `critical` |
| `--category` | (all) | Only report one category: `workload`, `scheduling`, `storage`, `node`, `cluster` |
| `--fail-on` | (off) | Exit non-zero if any finding is at least this severity — for CI gating |

```sh
# Only warnings and above, scheduling category, as JSON
kshrk diagnose capture.kshrk --severity warning --category scheduling -o json

# Fail a pipeline if anything critical is found
kshrk diagnose capture.kshrk --fail-on critical
```

### Rules

| rule_id | Severity | Category | Detects |
|---------|----------|----------|---------|
| `pod.crashloopbackoff` | critical | workload | Containers in CrashLoopBackOff |
| `pod.oomkilled` | critical | workload | Containers OOMKilled |
| `pod.image-pull` | critical | workload | ImagePullBackOff / ErrImagePull / InvalidImageName |
| `pod.config-error` | critical | workload | CreateContainerConfigError / CreateContainerError (missing ConfigMap/Secret/key, bad container config) |
| `pod.container-error` | warning | workload | Container terminated with an error |
| `pod.failed` / `pod.unknown` | warning | workload | Pod in Failed / Unknown phase |
| `scheduling.unschedulable` | warning | scheduling | Pending pods that can't be scheduled (with reason) |
| `storage.pvc-unbound` | warning | storage | PersistentVolumeClaims not Bound |
| `workload.no-requests` | warning | workload | Containers without resource requests |
| `workload.no-limits` | info | workload | Containers without resource limits |
| `workload.replicas-unavailable` | warning | workload | Deployment/StatefulSet/ReplicaSet/DaemonSet not fully available |
| `node.not-ready` | critical | node | Node Ready condition not True |
| `node.pressure` | warning | node | Node under Disk/Memory/PID pressure |
| `cluster.version-skew` | warning | cluster | kubelet ≥3 minor versions from the control plane |
| `cluster.deprecated-api` | warning | cluster | Captured use of removed/deprecated API group-versions |

Rules degrade gracefully — a rule whose inputs weren't captured (e.g. no nodes) is simply skipped.

### JSON output schema

`-o json` emits a stable, documented shape (pinned by `schema_version`):

```json
{
  "schema_version": 1,
  "capture_id": "550e8400-…",
  "at": "2026-04-09T10:05:00Z",
  "summary": { "critical": 2, "warning": 2, "info": 0 },
  "findings": [
    {
      "rule_id": "pod.crashloopbackoff",
      "severity": "critical",
      "category": "workload",
      "title": "CrashLoopBackOff",
      "object": { "kind": "Pod", "namespace": "prod", "name": "web-rs", "api_path": "/api/v1/namespaces/prod/pods" },
      "evidence": "CrashLoopBackOff",
      "suggestion": "Container is repeatedly crashing — check its logs and previous exit code.",
      "count": 3
    }
  ]
}
```

`count` is always present and is the number of objects a finding represents (≥1; greater than 1 when several objects of one owner are grouped, e.g. multiple pods of a ReplicaSet). `at` is only present when the report was pinned to a timestamp with `--at`; otherwise it is omitted (the report reflects the latest records). The same findings are shown in the web UI's **Diagnostics** view.

---

## Open

`kshrk open` reads the archive, starts a local mock HTTPS API server on `127.0.0.1`, and writes a kubeconfig pointing at it.

```sh
kshrk open capture.kshrk
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
kshrk open capture.kshrk --at 2026-04-09T10:30:00Z
kshrk open capture.kshrk --at -5m
```

`--at` accepts either:

- an RFC3339 timestamp, using UTC (`Z`) or an explicit offset (`+05:30`)
- a relative duration such as `-5m` or `-1h`, interpreted relative to the capture end time

If the requested time is outside the capture window, `kshrk open` exits with a clear error.

This is useful when you have a long capture (e.g. 1h) and want to compare cluster state before and after an incident.

---

## Replay

`kshrk replay` plays a capture **forward through time** at a chosen speed, streaming captured watch events (ADDED / MODIFIED / DELETED) to clients as a replay clock advances. This is different from `open --at`, which jumps the whole view to a single instant: replay advances a clock and streams change *over time*, so controllers/operators and `kubectl get --watch` observe the cluster changing exactly as it did during capture.

```sh
# Replay the whole capture at twice its original speed
kshrk replay capture.kshrk --speed 2x

# Slow motion
kshrk replay capture.kshrk --speed 0.5x

# Loop the last 10 minutes of the capture
kshrk replay capture.kshrk --from -10m --to -1m --loop

# Start paused, then press Enter to begin
kshrk replay capture.kshrk --start-paused
```

Like `open`, it writes a kubeconfig — point `kubectl` or a controller at it:

```sh
export KUBECONFIG=~/.kube/k8shark-<id>.yaml
kubectl get pods -A --watch
```

A live status line shows the clock position, speed, and how many events have streamed:

```
replaying 14:03:12Z (+2m18s / 10m) · 2x · 47 events emitted
```

The primary use case is **local development and testing of controllers/operators**: point one at a replayed capture and watch how it reacts to a real (or reproduced-incident) sequence of changes. LIST and GET return state as-of the clock (the same time-travel semantics as `--at`), and the watch stream delivers events in timestamp order, paced by clock × speed.

> **Read-only.** The mock server replays a captured timeline; a controller's writes won't persist or feed back into the replay. You observe "how my controller reacts to this sequence," not a closed loop.
>
> **Watch events.** Replay streams the events recorded with `watch: true` (see [docs/config.md](config.md)) at full fidelity. A poll-only capture (no watch index) still replays: `kshrk replay` infers ADDED/MODIFIED/DELETED events by diffing consecutive snapshots, so you get an event stream bounded by the poll interval's resolution. Use `watch: true` when you need precise, higher-resolution events.
>
> **resourceVersion & informers.** Replay assigns a coherent, monotonic `resourceVersion` to every object so real controller informers work: a LIST returns `resourceVersion = rvAsOf(clock)`, a `WATCH?resourceVersion=X` resumes by streaming only events newer than `X` (each carrying its own RV), and a stale/unknown RV returns `410 Gone` so the informer relists cleanly. A reconnecting client resumes from its last RV without dropped or duplicated events.

### Replay flags

| Flag | Default | Description |
|------|---------|-------------|
| `--speed` | `1x` | Playback speed factor, e.g. `2x`, `3x`, `0.5x` |
| `--from` | capture start | Window start: RFC3339 or relative duration like `-10m` |
| `--to` | capture end | Window end: RFC3339 or relative duration like `-1m` |
| `--loop` | false | Restart from the window start when the end is reached |
| `--start-paused` | false | Start paused (press Enter to begin, or use the dashboard when `--ui` is set) |
| `--ui` | false | Also start the web dashboard as a replay transport (VCR), sharing the clock |
| `--ui-port` | random | Port for the dashboard when `--ui` is set |
| `--writable` | false | Accept client writes into an in-memory overlay (closed-loop controller dev) |
| `--schedule-pods` | true | Bind unscheduled pods to a node on create (the scheduler replay lacks); `--writable` only |
| `--with-kwok` | false | Also run a detected `kwok` binary against the server to drive pod/node lifecycle (implies `--writable`) — see [KWOK](kwok.md) |
| `--port` | random | Port for the mock API server |
| `--kubeconfig-out` | `~/.kube/k8shark-<id>.yaml` | Where to write the generated kubeconfig |
| `--verbose` / `-v` | false | Log every request the server receives |

> **Dashboard transport.** Pass `--ui` to drive replay from the browser (play/pause/seek/speed), or
> start from the UI side with `kshrk ui capture.kshrk --speed 2x` — both share one clock so `kubectl`
> and the dashboard stay in lockstep. See [the Replay (VCR) section of the Web UI guide](web-ui.md#replay-vcr).

> **Closed-loop controller dev.** With `--writable`, `kshrk` binds an unscheduled Pod to a node on
> create (the scheduler replay lacks), so pairing it with [KWOK](https://kwok.sigs.k8s.io) takes Pods
> to `Running` and keeps nodes `Ready`. See [Closed-loop controller dev with KWOK](kwok.md).

### Controlling playback

The replay server exposes a small HTTP control API under `/_k8shark/replay` on the same address (a reserved prefix that never collides with the Kubernetes API). A successful request returns the current status as JSON (so a script — or a future UI scrubber — can drive playback); an invalid request returns a Kubernetes-style Status JSON body with the appropriate code (`405` wrong method, `400` bad argument, `404` unknown control):

| Request | Effect |
|---------|--------|
| `GET /_k8shark/replay` | Return current status |
| `POST /_k8shark/replay/pause` | Pause the clock |
| `POST /_k8shark/replay/play` | Resume the clock |
| `POST /_k8shark/replay/speed?value=2x` | Change speed |
| `POST /_k8shark/replay/seek?to=<RFC3339\|duration>` | Seek to a time (duration is relative to the window end, e.g. `-2m`) |
| `POST /_k8shark/replay/seek?offset=<duration>` | Seek to `window start + duration`, e.g. `90s` |

```sh
# The server uses a self-signed cert, so pass -k
curl -sk https://127.0.0.1:<port>/_k8shark/replay
curl -sk -X POST https://127.0.0.1:<port>/_k8shark/replay/pause
curl -sk -X POST "https://127.0.0.1:<port>/_k8shark/replay/speed?value=0.5x"
curl -sk -X POST "https://127.0.0.1:<port>/_k8shark/replay/seek?offset=30s"
```

The status response looks like:

```json
{
  "position": "2026-04-09T10:03:12Z",
  "from": "2026-04-09T10:00:00Z",
  "to": "2026-04-09T10:10:00Z",
  "elapsed_seconds": 192,
  "total_seconds": 600,
  "speed": 2,
  "paused": false,
  "loop": false,
  "ended": false,
  "epoch": 0,
  "events_emitted": 47
}
```

---

## UI

`kshrk ui` starts a local web-based explorer and the mock Kubernetes API server for a capture archive.

```sh
kshrk ui capture.kshrk
```

Example output:

```
k8shark mock server running
  Address:    https://127.0.0.1:51325
  Kubeconfig: ~/.kube/k8shark-abc123def456.yaml

Run: export KUBECONFIG=~/.kube/k8shark-abc123def456.yaml
Then use kubectl normally against the capture.

k8shark UI running
  Address: http://127.0.0.1:53421

Open this URL in your browser. Press Ctrl+C to stop.
```

The **dashboard UI is served at `/`** (it redirects to `/v2/`). For a full walkthrough with
screenshots, see **[docs/web-ui.md](web-ui.md)**.

### UI features

- Overview dashboard with KPIs, capture details, issues, and resource transitions
- Cluster-wide namespaces, workloads, and pods lists with drill-down
- Generic object view for any kind (incl. CRDs): YAML/JSON, relationships, history, diff
- Chip/token filter bar with type-ahead, `key=value` facets, regex, and label selectors
- Resources catalog with per-type/per-group toggles applied across the UI
- Watch-event timeline and a time-travel scrubber
- Light/dark theme

### UI flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | from config (`ui.port`), else random | Port for the local UI server |
| `--api-port` | from config (`ui.api_port`), else random | Port for the mock API server |
| `--kubeconfig-out` | `~/.kube/k8shark-<id>.yaml` | Where to write the generated kubeconfig |
| `--at` | latest records | Pin UI data to a specific timestamp (RFC3339 or relative duration) |

Set consistent ports with a `ui:` block in your config file (see [docs/config.md](config.md)); CLI
flags override it.

---

## Diff

`kshrk diff` compares either two capture archives or two points in time within the same archive.

Compare two archives:

```sh
kshrk diff --before before.kshrk --after after.kshrk
```

Compare two points within one archive:

```sh
kshrk diff --archive capture.kshrk \
  --before-at 2026-04-09T10:40:00Z \
  --after-at -1m
```

Scope the output:

```sh
kshrk diff --before before.kshrk --after after.kshrk \
  --resource pods --namespace default
```

Emit machine-readable JSON instead of unified diff text:

```sh
kshrk diff --before before.kshrk --after after.kshrk --output json
```

### Diff flags

| Flag | Default | Description |
|------|---------|-------------|
| `--before` | | Before archive path |
| `--after` | | After archive path |
| `--archive` | | Single archive path for intra-archive diff |
| `--before-at` | | Before snapshot time (RFC3339 or relative duration) |
| `--after-at` | | After snapshot time (RFC3339 or relative duration) |
| `--resource` | | Limit diff to one resource type |
| `--namespace` | | Limit diff to one namespace |
| `--output`, `-o` | `text` | Output format: `text` or `json` |

Exit codes follow the usual diff convention:

- `0` when no differences are found
- `1` when differences are found

---

## Redact

Sensitive values can be removed from a capture archive in two ways.

### Option A — `kshrk redact` (post-capture)

Produces a **new** archive with Kubernetes Secret data replaced and any configured field rules applied. The original archive is not modified.

```sh
# Redact secrets only
kshrk redact --in capture.kshrk --redact-secrets

# Redact secrets + specific fields via CLI flags
kshrk redact --in capture.kshrk --redact-secrets \
  --redact-field "data.api-key:ConfigMap:REDACTED" \
  --redact-field "spec.containers[*].env[*].value:Pod:REDACTED:string"

# Reuse the redaction rules from your capture config
kshrk redact --in capture.kshrk --out safe-capture.kshrk --config k8shark.yaml

# Preserve specific secrets from redaction
kshrk redact --in capture.kshrk --redact-secrets \
  --allow-secret default/pull-secret \
  --allow-secret kube-system/bootstrap-token
```

### Option B — inline after `kshrk capture`

Pass `--redact-secrets` or `--redact-field` to have the archive redacted **in-place** immediately after capture completes. Field rules defined in the capture config `redaction:` block are applied automatically without any extra flags.

```sh
# Redact secrets at capture time
kshrk capture --config k8shark.yaml --redact-secrets

# Redact secrets + ad-hoc field rules
kshrk capture --config k8shark.yaml --redact-secrets \
  --redact-field "data.api-key:ConfigMap:REDACTED"

# Config-driven: rules in redaction.rules block run automatically
kshrk capture --config k8shark.yaml   # redaction.redactSecrets: true in config
```

The final archive at the configured output path will already be redacted. No intermediate unredacted file is retained.

### `--redact-field` format

```
<fieldPath>:<Kind>:<replacement>[:<valueType>]
```

- `fieldPath` — dot-notation path with optional `[*]` wildcards or `**` recursive descent
- `Kind` — resource kind to match (`*` matches all)
- `replacement` — string written in place of the field value
- `valueType` — optional type hint: `string`, `integer`, `number`, `bool`, `array`, `object`

Examples:

```sh
--redact-field "data.api-key:ConfigMap:REDACTED"
--redact-field "spec.containers[*].env[*].value:Pod:REDACTED:string"
--redact-field "spec.replicas:Deployment:0:integer"
--redact-field "**.password:*:REDACTED"
```

### Redact flags

| Flag | Command | Default | Description |
|------|---------|---------|-------------|
| `--in` | `redact` | (required) | Source capture archive |
| `--out` | `redact` | `<in>-redacted.kshrk` | Output archive path |
| `--redact-secrets` | both | `false` | Redact all Secret `data`/`stringData` values |
| `--allow-secret` | both | | `namespace/name` of secret to preserve (repeatable) |
| `--redact-field` | both | | Field redaction rule (repeatable). Format: `<path>:<Kind>:<replacement>[:<type>]` |
| `--config` | `redact` | | Capture config file — applies `redaction.rules` and `redaction.redactSecrets` |

Secret metadata (name, namespace, labels, annotations, type) is always preserved so you can still count and identify secrets by kind.

See [config.md](config.md#redaction) for the full `redaction:` config block reference with type-aware examples.

---

## kubectl compatibility

The mock server is designed to be a drop-in replacement for `kubectl`'s real server. Supported behaviors:

| kubectl feature | Status |
|-----------------|--------|
| `kubectl get <resource>` | ✅ |
| `kubectl get <resource> -n <ns>` | ✅ |
| `kubectl get <resource> -A` | ✅ aggregates across captured namespaces |
| `kubectl get <resource> <name>` | ✅ extracted from parent list |
| `kubectl get -o yaml / -o json` | ✅ |
| `kubectl get -o wide` | ✅ captured Table columns when present, otherwise computed to match a live cluster (see below) |
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

### How `kubectl get` tables are rendered

`kubectl get` and `-o wide` are driven by server-side **Table** responses
(`columnDefinitions` + per-object `cells`). k8shark serves the captured Table
verbatim when it fully covers a request — the most faithful output, using the
real cluster's exact cells. When a request isn't covered by a captured Table —
objects created in a writable overlay, or kinds and captures without a stored
Table — k8shark **computes** a Table so the output still matches a live cluster,
choosing columns in this order:

1. a **CRD printer** built from the captured CRD's `additionalPrinterColumns`
   (JSONPath), for custom resources;
2. the **captured cluster `columnDefinitions`** — a full `?as=Table` for a
   targeted kind, or the columns-only `?as=TableSchema` recorded on each capture
   for list-capable native kinds whose cluster-scoped list path isn't itself
   captured as a full Table (untargeted kinds, and kinds targeted only in
   specific namespaces) (see
   [archive format](archive-format.md#table-response-keys)). These are the source
   cluster's exact columns/order (and `-o wide` priorities) for its Kubernetes
   version; per-object cells are computed by the built-in printer for the kind
   where one exists, otherwise metadata columns (Name/Namespace/Age) are filled
   and the rest are `null`;
3. a **built-in printer** for core/native kinds (Node, Pod, Deployment,
   ReplicaSet, StatefulSet, DaemonSet, ReplicationController, Job, CronJob,
   Service, Endpoints, Ingress, ConfigMap, Secret, PersistentVolumeClaim,
   PersistentVolume, Namespace, ServiceAccount, Event) — a fallback used when no
   captured columns are available (e.g. RBAC-denied schema, older captures);
   columns mirror upstream kubectl, including `-o wide` columns;
4. the generic **NAME / (NAMESPACE) / AGE** table.

Because every capture records column schemas for native kinds not already
captured as a full Table at their cluster path, `kubectl get` on overlay-created
or untargeted core objects (including objects in namespaces the capture didn't
target) reflects the source cluster's columns.
The built-in printers remain a best-effort fallback: they're a curated set and a
few computed cells are simplified (e.g. Pod `STATUS` approximates kubectl's
phase/container-reason logic). Read-only replay of a captured object always uses
the verbatim captured Table.

---

## Capturing pod logs

To capture pod logs alongside resource state, add a `logs` field to a `pods` resource entry in your capture config. The value is the number of tail lines to capture per pod:

```yaml
resources:
  - version: v1
    resource: pods
    namespaces: [production, staging]
    interval: 30s
    logs: 200             # capture last 200 lines from each container
    previousLogs: true    # also fetch ?previous=true for each container (optional)
```

Log content is fetched once at the end of the capture run and stored per (pod, container) under `/api/v1/namespaces/<ns>/pods/<name>/log?container=<c>`. When you open the archive with `kshrk open`:

```sh
kubectl logs my-app-pod-abc123 -n production              # current container
kubectl logs my-app-pod-abc123 -c istio-proxy -n production
kubectl logs my-app-pod-abc123 --previous -n production   # if previousLogs: true
```

If a pod's logs were not captured (e.g. `logs` was omitted or set to `0`), `kubectl logs` returns a clear stub message instead of an error:

```
# k8shark capture replay: logs were not captured for this pod.
# To capture logs, add 'logs: 200' (or another line count) to the
# pods entry in your k8shark capture config and re-run the capture.
```

After the capture finishes, the CLI prints a summary showing how many container logs were captured and a sample of any that were skipped (with reasons), so multi-container pods, terminated containers, and RBAC denials are visible without re-running in verbose mode.

> **Note:** Large log volumes increase archive size. Use a reasonable tail-line limit (e.g. 100–500 lines). When `previousLogs: true`, "container has not been previously terminated" responses are silently dropped — only successful previous-log captures count.

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
kshrk open capture.kshrk
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
