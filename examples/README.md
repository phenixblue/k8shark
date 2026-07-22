# Examples

Self-contained, runnable k8shark examples. Each subdirectory ships a
pre-recorded capture (`capture.kshrk`) so you can explore it immediately with
no live cluster required, plus the `config.yaml` used to record it and a
`README.md` walking through what to try.

| Example | Demonstrates |
|---------|--------------|
| [basic-workloads](basic-workloads/) | A minimal first capture: Pods, a Deployment, a Service, in one namespace |
| [multi-namespace](multi-namespace/) | Capturing the same resource types across several namespaces in one config |
| [crash-loop](crash-loop/) | Pod logs (current + previous), `kshrk diagnose`, investigating a `CrashLoopBackOff` |
| [rolling-update](rolling-update/) | Watch-driven capture across a Deployment rollout; `kshrk diff` and `kshrk transitions` |
| [auto-discovery](auto-discovery/) | `all: true` discovers every resource type in a namespace, built-in and CRD alike, with zero `group`/`version`/`resource` entries |

## Quick start

```sh
# Build kshrk if you haven't already
make build
export PATH="$PWD:$PATH"

# Replay any example — starts a mock API server + writes a kubeconfig
kshrk open examples/basic-workloads/capture.kshrk

# In another shell
export KUBECONFIG=~/.kube/k8shark-<id>.yaml   # path printed above
kubectl get pods -n basic-demo -o wide
```

Or browse it visually:

```sh
kshrk ui examples/basic-workloads/capture.kshrk
```

Each example's own `README.md` has the specific `kubectl` / `kshrk` commands
worth trying for that scenario.

## Re-capturing an example against your own cluster

Every example's `config.yaml` is the exact config used to produce its
`capture.kshrk`, scoped to the namespace(s)/resources that scenario needs. Point
`--kubeconfig` at any cluster with matching namespaces and workloads (a local
KinD cluster works fine — see
[docs/development.md](../docs/development.md#kind-dev-cluster)), or copy a
`config.yaml` and adjust `namespaces:` to match your own setup:

```sh
kshrk capture --config examples/crash-loop/config.yaml
```

For a generic, full-cluster starting point instead of one of these narrow
scenarios, see [`k8shark.yaml`](k8shark.yaml) in this directory and
[docs/config.md](../docs/config.md) for the full config reference.

## A note on archive size

Every capture — regardless of how few resources it targets — includes the
cluster's full API discovery documents and OpenAPI v2/v3 schemas, so that
`kubectl explain`, `kubectl api-resources`, and discovery-dependent tooling
replay faithfully. On a modern Kubernetes version this fixed cost alone is
roughly 1 MB compressed, which is why even these narrowly-scoped examples
land in the 1-2 MB range rather than the few hundred KB you'd expect from
their small resource counts.
