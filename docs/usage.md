# Usage

## Prerequisites

- `kubectl` in your `PATH`
- A valid `~/.kube/config` (or `KUBECONFIG` env set) pointing at a cluster — for `capture` only
- Go 1.26+ if building from source

## Installation

### Homebrew

```sh
brew install --cask phenixblue/tap/k8shark
```

To follow the **prerelease (release-candidate) channel** instead of stable:

```sh
brew install --cask phenixblue/tap/k8shark@rc
```

`k8shark@rc` tracks the latest `-rc` release; `k8shark` tracks the latest stable.
They install the same `kshrk` binary, so use one or the other (not both at once).
Both channels are Homebrew *casks*, not formulae — GoReleaser deprecated formula
generation in favor of casks, and `@` in a Ruby class name is invalid regardless
(`k8shark@rc` could only ever be a cask, which uses a `cask "name" do` string, not
a class name) — hence `--cask` on both.

### go install

```sh
go install github.com/phenixblue/k8shark@latest
```

For a specific prerelease, pin the tag, e.g. `go install github.com/phenixblue/k8shark@v0.5.1-rc.1`.

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
flags (`--out`, `--before`, `--after`, `--archive`, and `capture --out`) to
`*.kshrk` files, restricts `--config` to YAML files, and offers the valid
choices for output formats (e.g. `-o table|json|yaml`).

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
| `--out` | | `./k8shark-<timestamp>.kshrk` | Output archive path |
| `--duration` | | from config | Override capture duration (e.g. `5m`) |
| `--kubeconfig` | | `$KUBECONFIG` / `~/.kube/config` | Source cluster kubeconfig |
| `--auto-discover` | | false | Auto-discover and capture all available API resources |
| `--verbose` | `-v` | false | Log every API path as it is fetched |
| `--redact-secrets` | | false | Redact Secret `data`/`stringData` values from the archive after capture |
| `--allow-secret` | | | `namespace/name` of secret to preserve when `--redact-secrets` is set (repeatable) |
| `--redact-field` | | | Field redaction rule applied after capture: `<path>:<Kind>:<replacement>[:<type>]` (repeatable) |
| `--encrypt` | | false | Encrypt the output archive with a passphrase (prompts if no source is given) |
| `--encrypt-passphrase-file` | | | Read the encryption passphrase from this file instead of prompting |
| `--encrypt-recipient` | | | age recipient public key (`age1...`) to encrypt to (repeatable) |
| `--encrypt-recipients-file` | | | File of age recipient public keys, one per line |

See [Encryption](#encryption) below for the full picture, including decrypting on read and combining with redaction.

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
kshrk validate --config examples/k8shark.yaml
```

On success it prints a summary:

```
✓ Config valid (11 resource(s), all namespaces, duration 10m0s)
```

Errors exit 2 (see [exit codes](stability-policy.md#exit-codes)):

```
resources[0]: 'resource' field is required
```

Warnings are printed to stderr but do **not** cause a non-zero exit. For example (illustrative only — the exact indices and resources depend on your own config, not the `examples/k8shark.yaml` command above):

```
warning: resources[5] (storageclasses): cluster-scoped resource has 'namespaces:' set — this will be ignored at capture time
warning: resources[3] (events): interval 2s is very short and may produce a large archive
```

### Checks performed

**Hard errors** (exit 2):
- No `resources:` entries at all, and `autoDiscover` isn't set
- Missing `resource` field (unless `all: true`)
- Missing `version` field (unless `all: true`)
- Invalid `scope` value with `all: true` (must be `namespaced`, `cluster`, or empty)
- Unparseable `duration` or `interval` strings (e.g. a typo like `"5munites"`) —
  note `"0s"` is *not* an error, it parses fine (see the short-interval warning below)
- `logs` < 0
- `duration` too short (< 5s) when using discovery/wildcard capture
  (`autoDiscover: true`, an `all: true` entry, or a `namespaces: ['*']` entry) —
  discovery needs time to enumerate resources before the capture window closes

**Warnings** (exit 0, printed to stderr):
- Capture `duration` > 2 h — may produce a very large archive
- Resource `interval` < 5 s — may produce a very large archive
- Well-known cluster-scoped resource (`nodes`, `persistentvolumes`,
  `storageclasses`, `namespaces`, `clusterroles`, etc.) with `namespaces:` set
  — the capture engine auto-corrects this at runtime but it is likely a mistake
- A resource in a group that isn't one of Kubernetes' own built-in API groups
  (i.e. likely CRD-backed) with `namespaces:` set — verify it's actually
  namespaced and not a cluster-scoped custom resource
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

Exit codes follow the `diff(1)`/`git diff --exit-code` convention:

- `0` — no findings at or above `--fail-on` (or `--fail-on` not set)
- `1` — `--fail-on` tripped: at least one finding at or above the given severity
- `2` — the command failed (bad archive, invalid flags, ...)

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
| `--api-port` | random | Port for the mock API server |
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

> **Read-only by default.** Without `--writable` (below), the mock server replays a captured timeline; a controller's writes won't persist or feed back into the replay. You observe "how my controller reacts to this sequence," not a closed loop. Pass `--writable` to accept writes into an in-memory overlay instead — see [Closed-loop controller dev with KWOK](kwok.md).
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
| `--with-controller-manager` | false | Also run kube-controller-manager (downloaded/built to match the capture's Kubernetes version) against the server, with a curated controller set, to reconcile Deployments/ReplicaSets/DaemonSets/StatefulSets/Jobs/CronJobs/Endpoints (implies `--writable`) — see [KWOK](kwok.md#closing-more-of-the-loop---with-controller-manager) |
| `--api-port` | random | Port for the mock API server |
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
| `--ui-port` | from config (`ui.port`), else random | Port for the local UI server |
| `--api-port` | from config (`ui.apiPort`), else random | Port for the mock API server |
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
  --from 2026-04-09T10:40:00Z \
  --to -1m
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
| `--from` | | Before snapshot time, with `--archive` (RFC3339 or relative duration) |
| `--to` | | After snapshot time, with `--archive` (RFC3339 or relative duration) |
| `--resource` | | Limit diff to one resource type |
| `--namespace` | | Limit diff to one namespace |
| `--output`, `-o` | `text` | Output format: `text` or `json` |

Exit codes follow the `diff(1)`/`git diff --exit-code` convention:

- `0` — no differences found
- `1` — differences found
- `2` — the command failed (bad archive, invalid flags, ...)

---

## Query

`kshrk query` evaluates a kubectl-style JSONPath expression against **every** object captured in the archive — across all resource types and namespaces at once — instead of one list at a time. Useful for questions like "every container image in this capture" or "every pod's phase" without knowing in advance which resource types to look at.

```sh
kshrk query capture.kshrk '{.spec.containers[*].image}'
```

Example table output:

```
RESOURCE     NAMESPACE  NAME    VALUE
pods         default    web-1   "nginx:alpine"
pods         default    web-2   "nginx:alpine"
deployments  default    web     "nginx:alpine"

3 match(es)
```

Objects that don't have the queried field are skipped rather than reported as errors, so one expression can safely run across dissimilar resource types.

Scope the query and pin it to a point in time:

```sh
kshrk query capture.kshrk '{.status.phase}' --resource pods --at -5m
```

Emit machine-readable JSON instead of a table:

```sh
kshrk query capture.kshrk '{.spec.replicas}' --resource deployments -o json
```

### Query flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output`, `-o` | `table` | Output format: `table` or `json` |
| `--at` | latest | Query state at a timestamp (RFC3339 or relative duration like `-5m`); must be within the capture window |
| `--resource` | (all) | Limit the query to one resource type, e.g. `pods` |
| `--namespace` | (all) | Limit the query to one namespace |
| `--text` | (off) | Treat the expression as a plain substring instead of JSONPath (see [Full-text search](#full-text-search) below) |
| `--regex` | (off) | Treat the expression as a regular expression instead of JSONPath — mutually exclusive with `--text` |

Expressions follow the same JSONPath syntax as `kubectl get -o jsonpath=`, including wildcards (`[*]`) and filters (`[?(@.key=="value")]`).

### Full-text search

With `--text` or `--regex`, the expression is no longer JSONPath — it's searched as plain text across **every string value in every captured object**, and across captured pod logs (current and `--previous`). This answers "where does this error string appear?" without knowing in advance whether it's in an annotation, a container command, an event message, or a log line.

```sh
kshrk query capture.kshrk 'connection refused' --text
```

```
RESOURCE     NAMESPACE   NAME                          LOCATION                                                                  SNIPPET
pods         crash-demo  flaky-worker-897c5486c-lfk2f  spec.containers[0].command[2]                                             echo starting up; sleep 3; echo 'fatal: connection refused to db:5432'; exit 1
pods         crash-demo  flaky-worker-897c5486c-lfk2f  log:worker                                                                fatal: connection refused to db:5432
pods         crash-demo  flaky-worker-897c5486c-lfk2f  log:worker (previous)                                                     fatal: connection refused to db:5432
deployments  crash-demo  flaky-worker                  metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]  …echo starting up; sleep 3; echo 'fatal: connection refused to db:5432'; exit 1"],"image":"busybox:…
deployments  crash-demo  flaky-worker                  spec.template.spec.containers[0].command[2]                               echo starting up; sleep 3; echo 'fatal: connection refused to db:5432'; exit 1

5 match(es)
```

`--regex` accepts a Go regular expression (RE2 syntax) instead of a literal substring:

```sh
kshrk query capture.kshrk 'connection (refused|reset)' --regex
```

For object matches, `LOCATION` is the JSON field path of the matched string (e.g. `spec.containers[0].command[2]`). A map key that isn't a simple identifier — common for annotations/labels, which routinely contain `.` and `/` — is bracket-quoted so the path stays unambiguous: `metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]`, not the misleading `metadata.annotations.kubectl.kubernetes.io/last-applied-configuration`. For log matches, `LOCATION` is `log:<container>`, with `(previous)` appended for `kubectl logs --previous` content. `--resource`/`--namespace` scope full-text search the same way they scope JSONPath queries; `--resource` values other than `pods` exclude log content entirely, since only pods have logs.

---

## Transitions

`kshrk transitions` lists the individual ADDED/MODIFIED/DELETED events a resource went through during the capture — the change history, in order — without starting a replay server.

**`transitions` vs. `diff`:** both answer "what changed," but at different granularity. `kshrk diff` compares two snapshots (before/after, or two `--at` points in one archive) and shows the net difference between them — use it for "what's different between point A and point B." `kshrk transitions` shows every discrete event in between — use it for "what actually happened, and in what order" (e.g. a Deployment rolling through three intermediate states between the start and end of a rollout, which a two-point `diff` would collapse into a single net change).

```sh
kshrk transitions capture.kshrk --resource replicasets --namespace prod
```

```
TIME                  EVENT     RESOURCE     NAMESPACE  NAME
2026-07-22T21:19:15Z  ADDED     replicasets  prod       api-5c59c454f5
2026-07-22T21:19:15Z  MODIFIED  replicasets  prod       api-5c59c454f5
2026-07-22T21:19:18Z  MODIFIED  replicasets  prod       api-58dd68c5db
```

Add `--diff` to also show the field-level change for each `MODIFIED` event:

```sh
kshrk transitions capture.kshrk --resource replicasets --name api-5c59c454f5 --diff
```

```
2026-07-22T21:19:15Z  ADDED     replicasets/prod/api-5c59c454f5
2026-07-22T21:19:15Z  MODIFIED  replicasets/prod/api-5c59c454f5
@@ -95,7 +95,7 @@
-    "resourceVersion": "1172019",
+    "resourceVersion": "1172025",
   "status": {
-    "replicas": 0
+    "replicas": 1
```

### Transitions flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output`, `-o` | `table` | Output format: `table` or `json` |
| `--resource` | (all) | Filter by resource name fragment (e.g. `pods`, `deployments`) |
| `--namespace` | (all) | Filter by exact namespace |
| `--name` | (all) | Filter by exact object name |
| `--from` | capture start | Start of the time window: RFC3339 or relative duration like `-10m` |
| `--to` | capture end | End of the time window: RFC3339 or relative duration like `-1m` |
| `--diff` | false | Show field-level changes for `MODIFIED` events |

### Where events come from

Each captured API path uses one of two detection modes, and a single `transitions` run can mix both across different resources in the same archive:

- **Watch-enabled paths** (`watch: true` in the capture config for that resource): events are read directly from the watch-event index, with the exact `ADDED`/`MODIFIED`/`DELETED` label and timestamp captured live from the API server's watch stream.
- **Poll-only paths**: `kshrk transitions` infers events by diffing each consecutive pair of polled snapshots for the same object. This still works with no special config, but its resolution is bounded by the resource's poll `interval` — a change that happens and reverts between two polls is invisible, and the inferred timestamp is the poll time, not the moment the change actually happened. Use `watch: true` (see [docs/config.md](config.md)) when you need precise, higher-resolution events.

### Worked example

The [rolling-update example](../examples/rolling-update) captures a 3-replica Deployment through a rolling image update, with `watch: true` enabled specifically so `transitions` sees the precise event sequence:

```sh
kshrk transitions examples/rolling-update/capture.kshrk --resource replicasets
```

This lists the old ReplicaSet scaling down and the new one scaling up, one `MODIFIED` event per replica-count change — the actual mechanics of the rollout, which `kshrk diff --archive ... --resource deployments` between the same two timestamps would only show as a single net "image changed" difference. See the example's own `README.md` for the full walkthrough, including a `kshrk diff` comparison of the same window.

---

## Redact

Sensitive values can be removed from a capture archive in two ways.

### Option A — `kshrk redact` (post-capture)

Produces a **new** archive with Kubernetes Secret data replaced and any configured field rules applied. The original archive is not modified.

```sh
# Redact secrets only
kshrk redact capture.kshrk --redact-secrets

# Redact secrets + specific fields via CLI flags
kshrk redact capture.kshrk --redact-secrets \
  --redact-field "data.api-key:ConfigMap:REDACTED" \
  --redact-field "spec.containers[*].env[*].value:Pod:REDACTED:string"

# Reuse the redaction rules from your capture config
kshrk redact capture.kshrk --out safe-capture.kshrk --config k8shark.yaml

# Preserve specific secrets from redaction
kshrk redact capture.kshrk --redact-secrets \
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
| *(positional)* | `redact` | (required) | Source capture archive, e.g. `kshrk redact capture.kshrk` |
| `--out` | `redact` | `<in>-redacted.kshrk` | Output archive path |
| `--redact-secrets` | both | `false` | Redact all Secret `data`/`stringData` values |
| `--allow-secret` | both | | `namespace/name` of secret to preserve (repeatable) |
| `--redact-field` | both | | Field redaction rule (repeatable). Format: `<path>:<Kind>:<replacement>[:<type>]` |
| `--config` | `redact` | | Capture config file — applies `redaction.rules` and `redaction.redactSecrets` |
| `--encrypt`, `--encrypt-passphrase-file`, `--encrypt-recipient`, `--encrypt-recipients-file` | `redact` | | Encrypt the output archive — see [Encryption](#encryption) |
| `--decrypt-passphrase-file`, `--decrypt-identity-file` | `redact` | | Decrypt an encrypted source archive — see [Encryption](#encryption) |

Secret metadata (name, namespace, labels, annotations, type) is always preserved so you can still count and identify secrets by kind.

See [config.md](config.md#redaction) for the full `redaction:` config block reference with type-aware examples.

---

## Encryption

Captures can contain Secret values and other sensitive cluster data.
`kshrk capture` (and `kshrk redact`) can write the archive as a single
encrypted [age](https://age-encryption.org/v1) envelope, and every command
that reads a `.kshrk` archive — `inspect`, `open`, `ui`, `replay`, `diff`,
`query`, `transitions`, `diagnose`, `redact` — decrypts it transparently
given a key. `kshrk encrypt`/`kshrk decrypt` also let you encrypt or decrypt
an existing archive standalone, after the fact (see
[Encrypting or decrypting after the fact](#encrypting-or-decrypting-after-the-fact)
below).
See [encryption-threat-model.md](encryption-threat-model.md) for what this
does and doesn't protect against, and
[archive-format.md#encryption](archive-format.md#encryption) for how the
envelope is built.

### Encrypting at capture time

Two key modes, mutually exclusive per archive:

```sh
# Passphrase — prompts interactively (with confirmation) if no source is given
kshrk capture --config k8shark.yaml --encrypt

# Passphrase from a file, for scripts/CI (no prompt)
kshrk capture --config k8shark.yaml --encrypt-passphrase-file ./pass.txt

# Or via environment variable
KSHRK_ENCRYPT_PASSPHRASE=hunter2 kshrk capture --config k8shark.yaml --encrypt

# Recipient public keys — encrypt to one or more age keypairs
age-keygen -o key.txt          # generates a private key; prints the public key
kshrk capture --config k8shark.yaml --encrypt-recipient age1abc...

# Or a file of recipients (one per line, '#' comments allowed)
kshrk capture --config k8shark.yaml --encrypt-recipients-file recipients.txt
```

There is deliberately no `--encrypt-passphrase <string>` flag — passing a
passphrase as a bare CLI argument would leak it into shell history and `ps`
output. Use `--encrypt-passphrase-file`, the environment variable, or the
interactive prompt instead.

### Decrypting on read

Every read command accepts the same decrypt flags:

| Flag | Description |
|------|-------------|
| `--decrypt-passphrase-file` | Read the passphrase from this file (first line) |
| `--decrypt-identity-file` | An age identity (private key) file, e.g. from `age-keygen` |
| `$KSHRK_DECRYPT_PASSPHRASE` | Passphrase via environment variable |

If none of these are given and the archive turns out to be encrypted, `kshrk`
prompts for a passphrase on a terminal, or fails with a clear error instead of
hanging if stdin isn't a terminal. A plaintext archive is unaffected either
way — no key is ever required to open one.

```sh
kshrk inspect capture.kshrk --decrypt-passphrase-file ./pass.txt
kshrk ui capture.kshrk --decrypt-passphrase-file ./pass.txt
kshrk open capture.kshrk --decrypt-identity-file ./key.txt
kshrk diff --before a.kshrk --after b.kshrk --decrypt-passphrase-file ./pass.txt
```

A wrong passphrase or key produces a clean error (`incorrect passphrase or
key`), not a raw crypto failure.

### Combining with redaction

`kshrk redact` reads an encrypted source with the flags above and can
re-encrypt the output with `--encrypt-*`:

```sh
kshrk redact capture.kshrk --redact-secrets \
  --decrypt-passphrase-file old-pass.txt \
  --encrypt-recipient age1abc...
```

`kshrk capture --encrypt --redact-secrets` (redacting inline right after
capture) works the same way and keeps the archive encrypted end to end — no
plaintext copy is ever written to disk in between.

If a source archive is encrypted but `redact` isn't given any `--encrypt-*`
flags, it warns and writes the redacted output in plaintext rather than
silently downgrading it without telling you.

### Encrypting or decrypting after the fact

`kshrk encrypt` and `kshrk decrypt` are standalone whole-file commands for an
archive you already have — no new capture, no redaction. Use them to encrypt
an existing plaintext archive before sharing it, produce a plaintext copy for
a tool that can't decrypt, or rotate keys (decrypt, then re-encrypt to a new
passphrase or recipient set). Both take the same encrypt/decrypt flags shown
above, and default their output to `<in>-encrypted.kshrk` /
`<in>-decrypted.kshrk` — the original archive is never modified.

```sh
# Encrypt an archive you already captured
kshrk encrypt capture.kshrk --encrypt-recipient age1abc...

# Decrypt back to plaintext
kshrk decrypt capture-encrypted.kshrk --decrypt-passphrase-file ./pass.txt

# Rotate from a passphrase to a recipient key
kshrk decrypt old.kshrk --decrypt-passphrase-file old-pass.txt --out plain.kshrk
kshrk encrypt plain.kshrk --encrypt-recipient age1new... --out new.kshrk
```

`kshrk encrypt` refuses to run against an archive that's already encrypted,
and `kshrk decrypt` refuses to run against one that isn't — both fail with a
clear error rather than silently double-wrapping or no-op copying. See
[encryption-threat-model.md#rotating-keys](encryption-threat-model.md#rotating-keys)
for the trade-offs of this two-step rotation versus `redact`'s
plaintext-free re-encrypt path.

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
| Write operations (`apply`, `delete`, `edit`, …) | ⛔ returns `405 Method Not Allowed` — read-only by default; `kshrk replay --writable` accepts writes into an in-memory overlay instead |
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
client, not just `kubectl` — this section describes that default, read-only
mode. (`kshrk replay --writable` is the exception: see [Replay](#replay).)
This section documents what is supported, what requires special capture
config, and what is intentionally unsupported.

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

### Protobuf negotiation

`client-go` and `kubectl` default to requesting Kubernetes protobuf
(`application/vnd.kubernetes.protobuf`) for built-in types, not JSON — so
"will my controller/client work against this?" depends on the mock server
handling that negotiation the same way a real API server does. It does, on
both directions:

- **Reads**: k8shark always builds responses as JSON internally. When a
  request's `Accept` header selects protobuf over JSON (the negotiation
  mirrors real apiserver behavior: highest `q`-value wins among acceptable
  types, header order breaks ties), the response is transcoded to protobuf
  before being sent — but only when the body is a built-in, scheme-known
  Kubernetes type. A CRD/unstructured object, an OpenAPI document, or a Table
  response has no protobuf representation and is always served as JSON,
  exactly like a real API server (CRDs have no compiled protobuf schema).
- **Writes** (`kshrk replay --writable`): a protobuf-encoded request body
  (`Content-Type: application/vnd.kubernetes.protobuf`) is decoded to a typed
  object and re-encoded as JSON before landing in the overlay — transparent
  to the client, which never knows the overlay stores JSON internally.

**Known gap**: watch streams are always served as JSON, never transcoded to
protobuf, regardless of what the client's `Accept` header requests. This
doesn't affect correctness — client-go's watch decoder falls back to JSON
transparently — but a client asserting on the wire format of a watch
response specifically would notice.

### Requires CRD resources to be captured

These tools or commands require CRD-backed resources to be present in the
archive. Use `autoDiscover: true` or explicit config entries (see
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
| All write operations (POST/PUT/PATCH/DELETE) | Read-only by default — `kshrk replay --writable` accepts writes into an in-memory overlay (see [Replay](#replay)) |

### Using non-kubectl clients with kshrk open

`kshrk open` writes a kubeconfig file (`kubectl config view`) that points at
the mock server. Any tool that can be configured with a kubeconfig or a
`--kubeconfig` flag will work:

```sh
# Start the mock server
kshrk open capture.kshrk
# Note the printed kubeconfig path, e.g. ~/.kube/k8shark-<id>.yaml

# Use with istioctl
istioctl analyze --kubeconfig ~/.kube/k8shark-<id>.yaml

# Use with helm
helm list --kubeconfig ~/.kube/k8shark-<id>.yaml --all-namespaces

# Use with flux
flux get all --kubeconfig ~/.kube/k8shark-<id>.yaml

# Use with k9s
k9s --kubeconfig ~/.kube/k8shark-<id>.yaml
```

> **Tip:** Export `KUBECONFIG=~/.kube/k8shark-<id>.yaml` to make all tools
> in your shell session use the mock server automatically.
