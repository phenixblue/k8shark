#!/usr/bin/env bash
# Generate cluster churn while docs/demo/dashboard-capture.yaml is capturing, so
# the dashboard screenshot shows a populated Timeline instead of an empty one.
#
# A quiet cluster produces zero watch events, which is not a bug but does make
# the WATCH EVENTS tile and the "Resource transitions" chart look broken — the
# dashboard.png published before v1.0.0 showed exactly that. This script scales,
# restarts, and edits things on a loop so there is a real event stream to draw.
#
# Full recipe:
#
#   make kind-up
#   cp ~/.kube/k8shark-dev.yaml /tmp/k8shark-dev.yaml   # what the config expects
#   mkdir -p /tmp/k8shark-demo
#   ./docs/demo/dashboard-churn.sh &
#   churn=$!                                            # wait on this job only
#   kshrk capture --config docs/demo/dashboard-capture.yaml
#   wait "$churn"                                       # bare `wait` would block
#                                                       # on unrelated background
#                                                       # jobs in your shell
#   kshrk ui /tmp/k8shark-demo/cluster.kshrk --ui-port 18080 --api-port 18081
#
# Ports 18080/18081 on purpose: 8080/8081 are commonly already in use locally.
set -uo pipefail

export KUBECONFIG="${KUBECONFIG:-/tmp/k8shark-dev.yaml}"
NS=k8shark-test
# Stop a little before the capture's 90s duration so the last events still land
# inside the capture window.
DEADLINE=$((SECONDS + 80))

# Best-effort throughout: a failed scale or delete should not abort the run and
# lose the churn that already happened. Errors are surfaced, not swallowed.
#
# Only for commands whose output is meant for a human to read. Never redirect
# this to a file that another command parses: the 2>&1 folds kubectl's stderr
# into the captured stdout, so a single klog warning ends up inside the payload.
# Verified — piping a generated manifest through here and applying it fails with
# "error converting YAML to JSON: mapping values are not allowed in this
# context" the moment kubectl writes anything to stderr.
kc() { kubectl "$@" 2>&1 | sed 's/^/    /'; }

# Record the replica count before touching anything, so cleanup restores what was
# actually there rather than assuming the dev cluster's default.
NGINX_BASELINE=$(kubectl get deployment nginx -n "$NS" \
  -o jsonpath='{.spec.replicas}' 2>/dev/null) || NGINX_BASELINE=""
: "${NGINX_BASELINE:=2}"
# Churn relative to the baseline, never to fixed numbers. Scaling to a literal 4
# would be a no-op on a cluster already running 4 — the scale step would generate
# no pod events at all while still looking like it worked. Returning to the
# baseline each round also keeps the steady state during the capture window equal
# to what was there before, so the script stays as unintrusive as it can be.
NGINX_SCALED=$((NGINX_BASELINE + 2))

# Restore on every exit path, not just the happy one. Each round scales nginx up
# and back down, so being killed partway through would otherwise strand the
# deployment above its baseline — easy to hit, since the documented recipe leaves
# this running in the background during a capture.
restore_replicas() {
  echo "churn: restoring nginx to $NGINX_BASELINE replica(s)"
  kc scale deployment/nginx -n "$NS" --replicas="$NGINX_BASELINE"
}
# EXIT rather than INT/TERM directly: a signal trap converts the signal into an
# exit, and the EXIT trap then does the cleanup — so normal completion, SIGTERM
# (`kill %1`), and Ctrl+C all take one path. Trapping only INT/TERM would also
# miss a subtlety: a background job started from a non-interactive shell inherits
# SIGINT already ignored, and a trap can't re-enable a signal that was ignored on
# entry, so an INT-only trap silently never fires for the recipe's `script &`.
trap restore_replicas EXIT
trap 'exit 130' INT TERM

echo "churn: starting against $KUBECONFIG (ns=$NS, nginx baseline=$NGINX_BASELINE)"

round=0
while [ "$SECONDS" -lt "$DEADLINE" ]; do
  round=$((round + 1))
  echo "churn: round $round (${SECONDS}s elapsed)"

  # Scale a deployment up and back down: creates/deletes pods, so ReplicaSet
  # and Pod watch events both fire.
  kc scale deployment/nginx -n "$NS" --replicas="$NGINX_SCALED"
  sleep 8
  kc scale deployment/nginx -n "$NS" --replicas="$NGINX_BASELINE"
  sleep 6

  # Rollout restart: new ReplicaSet, pods terminating and starting — the most
  # visually interesting transition in the Timeline.
  kc rollout restart deployment/redis -n "$NS"
  sleep 8

  # ConfigMap create/update/delete: cheap, and exercises a non-pod resource so
  # the transitions chart is not all one kind.
  kc create configmap "churn-$round" -n "$NS" --from-literal=round="$round"
  sleep 3
  # Plain kubectl generates the manifest (see the kc() note above), piped
  # straight into apply — no temp file, so concurrent runs can't collide on one.
  kubectl create configmap "churn-$round" -n "$NS" \
    --from-literal=round="$round-updated" --dry-run=client -o yaml \
    | kc apply -f -
  sleep 3
  kc delete configmap "churn-$round" -n "$NS" --ignore-not-found
  sleep 4

  # Delete one pod and let the ReplicaSet recreate it.
  pod=$(kubectl get pods -n "$NS" -l app=nginx -o name 2>/dev/null | head -n 1)
  if [ -n "$pod" ]; then
    kc delete "$pod" -n "$NS" --grace-period=5
  fi
  sleep 4
done

# No explicit restore here: the EXIT trap above handles it on every path.
echo "churn: done after $round round(s), ${SECONDS}s"
