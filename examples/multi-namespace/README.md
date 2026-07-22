# multi-namespace

Captures the same resource types (`Pods`, `Deployments`, `Services`) from
**three** namespaces in one config, and shows how `kubectl get -A` aggregates
across them in a replayed capture.

## What's in `capture.kshrk`

A 45-second capture from a KinD cluster with:

- `team-frontend/storefront` — 2-replica Deployment
- `team-backend/orders-api` — 2-replica Deployment + Service
- `team-data/redis` — 1-replica Deployment

## Run it

```sh
kshrk open examples/multi-namespace/capture.kshrk
```

Export the printed kubeconfig, then compare cluster-wide vs. per-namespace
views:

```sh
export KUBECONFIG=~/.kube/k8shark-<id>.yaml

kubectl get pods -A -o wide                # aggregates across all 3 namespaces
kubectl get deployments -n team-backend
kubectl get svc -n team-backend
kubectl get pods -n team-data
```

## Re-capture it yourself

`config.yaml` names the three namespaces explicitly (`namespaces:
[team-frontend, team-backend, team-data]`). To capture *every* namespace in
the cluster instead of naming each one, swap that for the wildcard form —
see the comment at the bottom of `config.yaml` and
[docs/config.md](../../docs/config.md#wildcard-namespaces):

```yaml
namespaces: ["*"]
```

```sh
kshrk capture --config examples/multi-namespace/config.yaml
```

## Next step

See [crash-loop](../crash-loop/) for a capture that includes pod logs and
demonstrates diagnosing an unhealthy workload.
