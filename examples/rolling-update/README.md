# rolling-update

A capture spanning a `Deployment` rolling update, with `watch: true` enabled
so `kshrk transitions` sees precise ADDED/MODIFIED/DELETED events instead of
inferring them from poll snapshots. Demonstrates `kshrk diff` and
`kshrk transitions` — the two tools built for "what changed, and when."

## What's in `capture.kshrk`

A 90-second capture of `rollout-demo/api`, a 3-replica Deployment. ~20 seconds
in, its image was updated (`nginx:1.25-alpine` → `nginx:1.26-alpine`),
triggering a rolling update: a new ReplicaSet scales up as the old one scales
down, one pod at a time.

## Run it

### Time-travel diff

Compare cluster state right after the capture started against the final
state, scoped to Deployments:

```sh
kshrk diff --archive examples/rolling-update/capture.kshrk \
  --before-at 2026-07-22T21:18:56Z --after-at 2026-07-22T21:20:24Z \
  --resource deployments
```

(Use `kshrk inspect examples/rolling-update/capture.kshrk` to see the
capture's actual start/end timestamps if you re-run it yourself — they won't
match the ones above.)

The diff highlights the incremented `generation`,
`deployment.kubernetes.io/revision`, and the new container image.

### Transitions

List every state change and watch the old ReplicaSet scale down while the new
one scales up:

```sh
kshrk transitions examples/rolling-update/capture.kshrk --resource replicasets
```

### Replay

```sh
kshrk open examples/rolling-update/capture.kshrk
```

```sh
export KUBECONFIG=~/.kube/k8shark-<id>.yaml

kubectl get pods -n rollout-demo -o wide     # the 3 pods from the NEW ReplicaSet
kubectl get rs -n rollout-demo               # old RS at 0/0/0, new RS at 3/3/3
kubectl get deployment api -n rollout-demo -o jsonpath='{.spec.template.spec.containers[0].image}'
```

## Re-capture it yourself

`config.yaml` sets `watch: true` on `pods`, `deployments`, and `replicasets`
(see [docs/config.md](../../docs/config.md#watch-streaming-alongside-polling))
so transitions come from the watch-event index rather than being inferred
from 5-second poll snapshots. Start the capture, then trigger a rollout
partway through the window:

```sh
kshrk capture --config examples/rolling-update/config.yaml &

sleep 20
kubectl set image deployment/api -n rollout-demo app=nginx:1.26-alpine

wait   # let the remaining ~70s of the capture finish
```

## Next step

See [auto-discovery](../auto-discovery/) for a config that discovers every
resource type in a namespace — including a custom resource — instead of
listing each `group`/`version`/`resource` explicitly.
