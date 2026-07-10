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

- **On by default** under `--writable`. A Pod that never schedules is more
  surprising than one that does.
- Binds round-robin over the **known nodes** — those in the capture plus any
  created in the overlay.
- If the capture has **no nodes**, it synthesizes one (`kwok-node-0`) annotated
  `kwok.x-k8s.io/node: fake` so a stock `kwok` run manages it.
- A Pod that already sets `spec.nodeName` is left alone — the shim is a
  scheduler, not an override.

The shim only assigns `nodeName`. It does **not** set Pod status — that's KWOK's
job, which is what makes the loop realistic rather than faked by `kshrk`.

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
kwok --kubeconfig="$KUBECONFIG" --manage-all-nodes
```

KWOK uses its built-in default Stages, which take a bound Pod to `Running` and
keep nodes `Ready`. To customize timing or conditions, pass your own
`--config stages.yaml` (see [KWOK Stages](https://kwok.sigs.k8s.io/docs/user/stages-configuration/)).

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

## Non-goals

Standalone KWOK + the overlay covers pod/node lifecycle. It intentionally does
**not** provide the rest of a control plane:

- **Scheduling fit** — the shim is round-robin, not a real scheduler (no
  resource/affinity/taint evaluation).
- **Endpoints/EndpointSlices and the rest of kube-controller-manager** — only a
  real control plane (`kwokctl`, a separate throwaway cluster) runs these.
- **Full CNCF conformance** via replay.

See [#160](https://github.com/phenixblue/k8shark/issues/160) for the tracking
issue and [#123](https://github.com/phenixblue/k8shark/issues/123) for the
writable-overlay epic.
