# Closed-loop controller dev with KWOK

`kshrk replay --writable` gives a controller a writable Kubernetes API over a
captured cluster (see the [writable overlay](usage.md#replay)). On its own the
overlay has no controllers, so a Pod a client creates never leaves `Pending` —
nothing schedules it and nothing runs it. Pairing the overlay with
[KWOK](https://kwok.sigs.k8s.io) (Kubernetes WithOut Kubelet) closes that loop:
KWOK plays the kubelet, and a small built-in **scheduling shim** plays the one
piece of the scheduler that KWOK needs.

```
your controller ──create Pod──▶ kshrk overlay ──shim binds nodeName──▶ Pod
        ▲                                                                │
        └──────────── observes Running ◀── KWOK sets status ◀── watch ───┘
```

## The scheduling shim

KWOK's "Pod → Running" behavior only fires once a Pod is **bound to a node**
(`spec.nodeName`) — assigning that is the scheduler's job, which replay has no
equivalent of. So in writable replay `kshrk` binds an unscheduled Pod on create:

- **On by default** under `--writable` (turn it off with `--schedule-pods=false`
  if you're testing your own scheduler). A Pod that never schedules is more
  surprising than one that does.
- Binds round-robin over the **known nodes** — those in the capture plus any
  created in the overlay.
- If the capture has **no nodes**, it synthesizes one (`kwok-node-0`) annotated
  `kwok.x-k8s.io/node: fake` so a stock `kwok` run manages it. This happens at
  startup, so `kubectl get nodes` shows it before any Pod exists.
- A Pod that already sets `spec.nodeName` is left alone — the shim is a
  scheduler, not an override.

Beyond binding, the overlay stamps a freshly created Pod with
`status.phase: Pending` — exactly what the real apiserver does on create, and
what KWOK's `pod-ready` stage selects on. It does **not** drive the Pod to
`Running` — that's KWOK's job, which is what makes the loop realistic rather than
faked by `kshrk`.

## Quickstart: `--with-kwok`

If a [`kwok` binary](https://kwok.sigs.k8s.io/docs/user/install/) is on your
`PATH`, `kshrk` can start and manage it for you — it implies `--writable`, runs
`kwok --manage-all-nodes` with the bundled stages, and stops it on exit:

```sh
kshrk replay capture.kshrk --with-kwok
export KUBECONFIG=/Users/you/.kube/k8shark-<id>.yaml   # path is printed
kubectl run demo --image=nginx && kubectl get pod demo -w   # Pending → Running
```

That's the whole loop in one process. The manual walkthrough below is equivalent
and useful when you want to run (or customize) `kwok` yourself.

## Walkthrough

Prerequisites: a `.kshrk` capture, the `kshrk` binary, and the
[`kwok` binary](https://kwok.sigs.k8s.io/docs/user/install/) on your `PATH`.

**1. Start writable replay.** It writes a kubeconfig and prints its path:

```sh
kshrk replay capture.kshrk --writable
#   Kubeconfig: /Users/you/.kube/k8shark-<id>.yaml
#   Writable:   on (client writes land in an in-memory overlay)
```

**2. Point KWOK at the same kubeconfig** (in another terminal):

```sh
export KUBECONFIG=/Users/you/.kube/k8shark-<id>.yaml
# --manage-all-nodes also manages nodes that came from the capture; drop it to
# manage only kshrk's synthesized `kwok.x-k8s.io/node: fake` nodes.
# --config supplies the lifecycle Stages — standalone kwok loads none on its own
# (it exits with "no stages found" otherwise).
kwok --kubeconfig="$KUBECONFIG" --manage-all-nodes \
  --config /path/to/k8shark/examples/kwok-stages.yaml
```

`examples/kwok-stages.yaml` is KWOK's own "fast" default Stages (node → `Ready`,
Pod → `Running`, Job Pod → `Succeeded`, delete). To customize timing or
conditions, edit it or supply your own (see
[KWOK Stages](https://kwok.sigs.k8s.io/docs/user/stages-configuration/)).

**3. Run your controller** against `$KUBECONFIG`, then create a workload:

```sh
kubectl --kubeconfig="$KUBECONFIG" run demo --image=nginx
kubectl --kubeconfig="$KUBECONFIG" get pod demo -w
# Pending → (shim binds nodeName) → KWOK → Running
```

Your controller observes the Pod reach `Running` — its own write, taken to a
realistic terminal state, without a real kubelet or a live cluster.

## Validating the loop

An end-to-end test drives exactly this scenario — capture a KinD cluster, replay
it `--writable`, run a real `kwok`, create a Pod, and assert the shim binds it to
a node and KWOK takes it to `Running`:

```sh
make e2e-kwok           # requires kind, kubectl, and the kwok binary
```

It is **manually triggered** (it needs the `kwok` binary, which the base CI
runner lacks), so it is not part of `make e2e` or the automatic e2e job. In CI it
runs from the **e2e-kwok** GitHub Actions workflow (Actions tab → Run workflow),
which installs `kwok` first.

## Closing more of the loop: `--with-controller-manager`

KWOK plays the kubelet; `--with-controller-manager` adds a real
`kube-controller-manager` reconciling a curated set of controllers against the
same writable overlay, so a Deployment you create actually gets a ReplicaSet
and Pods, a CronJob spawns Jobs on schedule, and a Service gets Endpoints and
an EndpointSlice — all without a full second cluster:

```sh
kshrk replay capture.kshrk --with-kwok --with-controller-manager
export KUBECONFIG=/Users/you/.kube/k8shark-<id>.yaml
kubectl create deployment demo --image=nginx --replicas=2
kubectl get pods -w   # ReplicaSet + Pods appear, KWOK takes them to Running
```

**Curated controller set** (`internal/k8sbin` + `cmd/controllermanager.go`):
`namespace`, `serviceaccount`, `resourcequota`, `garbagecollector`,
`daemonset`, `deployment`, `replicaset`, `statefulset`, `job`, `cronjob`,
`endpoint`, `endpointslice`, `endpointslicemirroring`, `disruption` — pure
API-object reconcilers that only need CRUD+watch against the overlay, not a
real kubelet, storage provisioner, cloud provider, or node lifecycle. Started
with `--leader-elect=false` (a single local process needs no leader election)
and `--use-service-account-credentials=false`.

**Binary sourcing.** Kubernetes only publishes `kube-controller-manager` for
`linux/{amd64,arm64}` — there's no macOS or Windows build. On Linux,
`--with-controller-manager` downloads the official release binary from
`dl.k8s.io` (matching the capture's Kubernetes version) and verifies it
against the published SHA-256 checksum. On other platforms it instead
downloads and verifies the official Kubernetes **source** tarball from the
same `dl.k8s.io` origin and compiles it with the host's Go toolchain — the
same approach [KWOK's own docs recommend](https://kwok.sigs.k8s.io/docs/user/kwokctl-platform-specific-binaries/)
for platforms it doesn't publish binaries for either. Either way, the result is
cached under the OS cache directory, keyed by version and GOOS/GOARCH, so this
only runs once per version per machine. `go` must be on `PATH` for the
source-build path.

**API defaulting.** A real apiserver defaults dozens of fields on every write
(e.g. an empty `Deployment.spec.strategy.type` becomes `RollingUpdate` with a
25%/25% `rollingUpdate` struct); the writable overlay has no apiserver behind
it, so `internal/server/writes.go`'s `applyKnownDefaults` hand-applies the
specific, long-stable defaults these controllers assume are already
present — real controller code unconditionally dereferences fields like
`Spec.Strategy.RollingUpdate.MaxSurge` or `Spec.BackoffLimit`, so a missing
default panics deep inside the controller (caught by client-go's recover, so
it doesn't crash the process, but the object never reconciles) rather than
erroring cleanly.

## Non-goals

Even with `--with-controller-manager`, this intentionally does **not** provide
a full control plane:

- **Scheduling fit** — the pod-scheduling shim is round-robin, not a real
  scheduler (no resource/affinity/taint evaluation).
- **A real kubelet/container runtime** — KWOK fakes Pod status transitions; it
  never runs a container, so anything that depends on actual process/exit-code
  behavior (`kubectl exec`, `kubectl logs`, a Job ever reaching `Succeeded`)
  doesn't work. See the "exec, cp, port-forward, and proxy are not supported"
  error for the replay server's own explicit stance on this.
- **PersistentVolume binding, HorizontalPodAutoscaler, node-lifecycle,
  certificate-signing, and the other kube-controller-manager controllers**
  outside the curated set above — they need a real storage provisioner, a real
  metrics-server, or real node health, which nothing here provides. A full
  control plane (`kwokctl`, a separate throwaway cluster) is still the answer
  for those.
- **Full CNCF conformance** via replay — measured directly; see
  [docs/conformance.md](conformance.md) and the upstream `[Conformance]` suite
  results for what this closes and what it doesn't.

See [#160](https://github.com/phenixblue/k8shark/issues/160) for the tracking
issue and [#123](https://github.com/phenixblue/k8shark/issues/123) for the
writable-overlay epic.
