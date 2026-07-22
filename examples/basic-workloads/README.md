# basic-workloads

The smallest useful capture: one `Deployment` (2 replicas), the `Pods` it
manages, its `Service`, and cluster `Nodes`, all in a single namespace. Start
here if this is your first time using k8shark.

## What's in `capture.kshrk`

A 45-second capture from a KinD cluster with:

- `basic-demo/web` — an `nginx:alpine` Deployment with 2 replicas and a
  matching Service
- The 2 Pods backing that Deployment
- The 1-node KinD cluster's Node object

## Run it

```sh
kshrk open examples/basic-workloads/capture.kshrk
```

This prints a kubeconfig path. Export it and use `kubectl` as normal:

```sh
export KUBECONFIG=~/.kube/k8shark-<id>.yaml   # path printed by `kshrk open`

kubectl get pods -n basic-demo -o wide
kubectl get deployment web -n basic-demo
kubectl describe deployment web -n basic-demo
kubectl get svc -n basic-demo
kubectl get nodes
```

Or explore it in the browser instead:

```sh
kshrk ui examples/basic-workloads/capture.kshrk
```

You can also inspect the archive without starting a server:

```sh
kshrk inspect examples/basic-workloads/capture.kshrk
```

## Re-capture it yourself

`config.yaml` is the exact config used to produce `capture.kshrk`. Point it at
any cluster (a KinD cluster is enough — see
[docs/development.md](../../docs/development.md#kind-dev-cluster) for
`make kind-up`) that has a `basic-demo` namespace with a Deployment, or adjust
the `namespaces:` field to match your own:

```sh
kshrk capture --config examples/basic-workloads/config.yaml
```

## Next step

See [multi-namespace](../multi-namespace/) for capturing the same resource
types across several namespaces at once.
