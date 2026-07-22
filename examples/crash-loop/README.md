# crash-loop

A `Pod` stuck in `CrashLoopBackOff`, with **current and previous container
logs** captured, `Events`, and the owning `Deployment`. Demonstrates
`kshrk diagnose`, `kubectl logs --previous`, and `kubectl describe` replay.

## What's in `capture.kshrk`

A 30-second capture of `crash-demo/flaky-worker`: a busybox container that
prints a fake connection error and exits 1 on a loop. By capture time it had
already restarted several times and was sitting in `CrashLoopBackOff`.

## Run it

```sh
kshrk open examples/crash-loop/capture.kshrk
```

Export the printed kubeconfig, then investigate the failure exactly as you
would against a live cluster:

```sh
export KUBECONFIG=~/.kube/k8shark-<id>.yaml

kubectl get pods -n crash-demo -o wide          # STATUS: CrashLoopBackOff
kubectl describe pod -n crash-demo -l app=flaky-worker   # Events history
kubectl logs -n crash-demo -l app=flaky-worker            # current container log
kubectl logs -n crash-demo -l app=flaky-worker --previous # log from the terminated attempt
```

Or skip straight to the offline findings engine, no server needed:

```sh
kshrk diagnose examples/crash-loop/capture.kshrk
```

```
SEVERITY  CATEGORY  OBJECT                             FINDING
CRITICAL  workload  crash-demo/flaky-worker-897c5486c  CrashLoopBackOff — CrashLoopBackOff
WARNING   workload  crash-demo/flaky-worker-897c5486c  Container without resource requests — a container has no resources.requests
WARNING   workload  crash-demo/flaky-worker            Deployment not fully available — 0/1 replicas ready
INFO      workload  crash-demo/flaky-worker-897c5486c  Container without resource limits — a container has no resources.limits
```

## Re-capture it yourself

`config.yaml` sets `logs: 50` and `previousLogs: true` on the `pods` entry so
both the current and terminated container logs are captured (see
[docs/config.md](../../docs/config.md#capturing-pod-logs)), and `dedup: false`
on `events` so every event is kept rather than only the first occurrence.

Deploy a container that always exits non-zero and give it a minute to cycle
into `CrashLoopBackOff` before capturing — otherwise you'll only catch the
`Error` terminated state between restarts, not the backoff wait:

```sh
kubectl create namespace crash-demo
kubectl create deployment flaky-worker -n crash-demo --image=busybox:1.36 \
  -- /bin/sh -c "echo starting up; sleep 3; echo 'fatal: connection refused to db:5432'; exit 1"

# wait ~1-2 minutes for CrashLoopBackOff, then:
kshrk capture --config examples/crash-loop/config.yaml
```

## Next step

See [rolling-update](../rolling-update/) for a capture spanning a Deployment
rollout, and `kshrk diff` / `kshrk transitions` over that window.
