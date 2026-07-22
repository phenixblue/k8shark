# auto-discovery

A capture config with **zero** `group`/`version`/`resource` entries. A single
`all: true` directive walks the cluster's API discovery at capture time and
captures every resource type present in one namespace — built-in Kubernetes
resources and a custom resource (CRD) alike — without listing any of them by
name. Demonstrates the `all: true` / `auto_discover` config field described in
[docs/config.md](../../docs/config.md#simplified-discovery-with-all-true) and
[Capturing CRD-backed resources](../../docs/config.md#capturing-crd-backed-resources).

## What's in `capture.kshrk`

A 30-second capture of the `auto-demo` namespace, which has:

- A toy `Widget` custom resource (`widgets.demo.k8shark.io`, defined in
  [`manifests/widget-crd.yaml`](manifests/widget-crd.yaml)) with two
  instances, `gizmo` and `sprocket`
- A `widget-controller` Deployment (1 replica) + a `widget-config` ConfigMap

`config.yaml` never mentions `widgets`, `deployments`, or `configmaps` by
name — `all: true` discovered all of them:

```
RESOURCE                  GROUP                      VERSION  ITEMS  NAMESPACES
configmaps                                           v1       2      auto-demo
pods                                                 v1       1      auto-demo
secrets                                              v1       0      auto-demo
serviceaccounts                                      v1       1      auto-demo
deployments                apps                       v1       1      auto-demo
replicasets                apps                       v1       1      auto-demo
widgets                    demo.k8shark.io            v1       2      auto-demo
...and 27 more resource types known to the cluster, most with 0 items here
```

(Run `kshrk inspect examples/auto-discovery/capture.kshrk` for the
full 34-row table.) Resource types with no instances in this namespace — like
`secrets` and `services` here — still get a capture entry; `all: true`
discovers *types*, not just types that happen to be populated.

## Run it

```sh
kshrk open examples/auto-discovery/capture.kshrk
```

Export the printed kubeconfig, then treat the CRD exactly like a built-in
resource — including schema-aware commands like `explain`:

```sh
export KUBECONFIG=~/.kube/k8shark-<id>.yaml

kubectl get widgets -n auto-demo
kubectl describe widget gizmo -n auto-demo
kubectl explain widget.spec        # works — the CRD's OpenAPI schema replays too
kubectl get deployment widget-controller -n auto-demo
```

Or inspect the archive offline:

```sh
kshrk inspect examples/auto-discovery/capture.kshrk
kshrk diagnose examples/auto-discovery/capture.kshrk
```

## Re-capture it yourself

`config.yaml` is:

```yaml
resources:
  - all: true
    scope: namespaced
    namespaces: [auto-demo]
    interval: 10s
```

`scope: namespaced` plus an explicit `namespaces:` list keeps discovery
contained to one namespace — without `namespaces:`, a namespaced `all: true`
directive expands to *every* namespace in the cluster (see
[docs/config.md](../../docs/config.md#simplified-discovery-with-all-true)),
which is rarely what you want on a shared cluster.

Apply the CRD and demo resources, then capture:

```sh
kubectl create namespace auto-demo
kubectl apply -f examples/auto-discovery/manifests/widget-crd.yaml
kubectl wait --for=condition=Established crd/widgets.demo.k8shark.io --timeout=30s
kubectl apply -f examples/auto-discovery/manifests/demo-resources.yaml

kshrk capture --config examples/auto-discovery/config.yaml
```

**Gotcha:** `all: true` also picks up whatever the namespace already carries
by default — here that's the `default` ServiceAccount and its token Secret.
Real clusters will surface far more of this incidental state than our two
Widgets and one Deployment. Pair `all: true` with
[`redactSecrets: true`](../../docs/config.md#redaction) if you're capturing
a namespace you plan to share.

## Next step

This is the last stop in the example tour. From here,
[docs/config.md](../../docs/config.md) covers the full config reference,
including redaction rules and ecosystem-specific `auto_discover` recipes for
Flux, ArgoCD, and other CRD-heavy setups.
