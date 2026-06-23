#!/usr/bin/env bash
# Spin up a KinD cluster full of deliberately *unhealthy* workloads so a
# k8shark capture has rich pod events to show off: CrashLoopBackOff, OOMKilled,
# ImagePullBackOff, FailedScheduling, probe flapping, unbound PVCs and Job
# backoff. Use this when kind-up.sh's healthy workloads are too quiet to
# demonstrate the Timeline / pod-detail Conditions & restart sparkline.
#
# Usage:
#   ./scripts/kind-chaos.sh                 # create cluster + deploy chaos workloads
#   ./scripts/kind-chaos.sh --reset         # delete existing cluster first, then recreate
#   ./scripts/kind-chaos.sh --churn 180     # after deploying, bounce healthy pods for 180s
#                                           #   to generate Scheduled/Created/Killing events
#
# Tear down with:  make kind-down  (or: kind delete cluster --name k8shark-dev)
set -euo pipefail

CLUSTER_NAME="k8shark-dev"
KIND_KUBECONFIG="${HOME}/.kube/k8shark-dev.yaml"
NS="k8shark-chaos"
NS_JOBS="k8shark-chaos-jobs"
RESET=false
CHURN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --reset) RESET=true; shift ;;
    --churn) CHURN="${2:?--churn needs a number of seconds}"; shift 2 ;;
    -h|--help) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
ok()   { printf '  \033[1;32m[OK]\033[0m  %s\n' "$*"; }
die()  { printf '\n\033[1;31mFATAL: %s\033[0m\n' "$*" >&2; exit 1; }

# ── Prerequisites ──────────────────────────────────────────────────────────────
log "Checking prerequisites"
for tool in kind kubectl; do
  command -v "$tool" >/dev/null 2>&1 || die "'$tool' not found in PATH"
  ok "$tool: $(command -v "$tool")"
done

# ── Optional reset ─────────────────────────────────────────────────────────────
if $RESET && kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  log "Deleting existing cluster '$CLUSTER_NAME'"
  kind delete cluster --name "$CLUSTER_NAME"
fi

# ── Create KinD cluster ────────────────────────────────────────────────────────
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  log "Cluster '$CLUSTER_NAME' already exists — skipping creation"
  kind get kubeconfig --name "$CLUSTER_NAME" > "$KIND_KUBECONFIG"
  chmod 0600 "$KIND_KUBECONFIG"
else
  log "Creating KinD cluster '$CLUSTER_NAME'"
  kind create cluster \
    --name "$CLUSTER_NAME" \
    --kubeconfig "$KIND_KUBECONFIG" \
    --wait 90s
  ok "Cluster '$CLUSTER_NAME' ready"
fi

KC=(--kubeconfig "$KIND_KUBECONFIG")

# ── Namespaces ───────────────────────────────────────────────────────────────
log "Creating namespaces"
for ns in "$NS" "$NS_JOBS"; do
  kubectl "${KC[@]}" get namespace "$ns" &>/dev/null || kubectl "${KC[@]}" create namespace "$ns"
done
ok "Namespaces: $NS, $NS_JOBS"

# ── Eventful workloads ─────────────────────────────────────────────────────────
log "Deploying chaos workloads into '$NS'"

# 1. Healthy baseline (also the churn target). Gives a "good" contrast row.
kubectl "${KC[@]}" apply -n "$NS" -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: healthy-web
  labels: { app: healthy-web, k8shark.io/scenario: healthy }
spec:
  replicas: 2
  selector: { matchLabels: { app: healthy-web } }
  template:
    metadata: { labels: { app: healthy-web } }
    spec:
      containers:
        - name: nginx
          image: nginx:alpine
          resources: { requests: { cpu: 10m, memory: 16Mi }, limits: { memory: 64Mi } }
YAML
ok "healthy-web        — 2 healthy replicas (baseline)"

# 2. CrashLoopBackOff — container exits non-zero, restart count climbs forever.
kubectl "${KC[@]}" apply -n "$NS" -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: crashloop
  labels: { app: crashloop, k8shark.io/scenario: crashloop }
spec:
  replicas: 1
  selector: { matchLabels: { app: crashloop } }
  template:
    metadata: { labels: { app: crashloop } }
    spec:
      containers:
        - name: flaky
          image: busybox:1.36
          command: ["/bin/sh", "-c", "echo starting up; sleep 4; echo crashing; exit 1"]
YAML
ok "crashloop          — exits 1 on a loop -> CrashLoopBackOff, rising restarts"

# 3. OOMKilled — balloons anonymous memory past its limit, gets OOM-killed, repeats.
kubectl "${KC[@]}" apply -n "$NS" -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: oomkill
  labels: { app: oomkill, k8shark.io/scenario: oomkill }
spec:
  replicas: 1
  selector: { matchLabels: { app: oomkill } }
  template:
    metadata: { labels: { app: oomkill } }
    spec:
      containers:
        - name: hog
          image: busybox:1.36
          # Buffer ~300MB into a shell variable; the 100Mi limit triggers an OOM kill.
          command: ["/bin/sh", "-c", "echo eating memory; A=$(yes payload | head -c 314572800); echo done"]
          resources: { requests: { memory: 32Mi }, limits: { memory: 100Mi } }
YAML
ok "oomkill            — allocates past memory limit -> OOMKilled, rising restarts"

# 4. ImagePullBackOff — image that can never be pulled.
kubectl "${KC[@]}" apply -n "$NS" -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: bad-image
  labels: { app: bad-image, k8shark.io/scenario: imagepull }
spec:
  replicas: 1
  selector: { matchLabels: { app: bad-image } }
  template:
    metadata: { labels: { app: bad-image } }
    spec:
      containers:
        - name: missing
          image: registry.invalid/k8shark/does-not-exist:v0
YAML
ok "bad-image          — bogus image -> ErrImagePull / ImagePullBackOff (Pending)"

# 5. FailedScheduling — requests more CPU than any node can offer.
kubectl "${KC[@]}" apply -n "$NS" -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: unschedulable
  labels: { app: unschedulable, k8shark.io/scenario: unschedulable }
spec:
  replicas: 1
  selector: { matchLabels: { app: unschedulable } }
  template:
    metadata: { labels: { app: unschedulable } }
    spec:
      containers:
        - name: greedy
          image: nginx:alpine
          resources: { requests: { cpu: "64" } }   # no kind node has 64 cores
YAML
ok "unschedulable      — 64-core request -> FailedScheduling (Pending, Unschedulable)"

# 6. Liveness probe flapping — kubelet keeps killing/restarting the container.
kubectl "${KC[@]}" apply -n "$NS" -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: liveness-flap
  labels: { app: liveness-flap, k8shark.io/scenario: liveness }
spec:
  replicas: 1
  selector: { matchLabels: { app: liveness-flap } }
  template:
    metadata: { labels: { app: liveness-flap } }
    spec:
      containers:
        - name: nginx
          image: nginx:alpine
          livenessProbe:
            exec: { command: ["/bin/sh", "-c", "exit 1"] }   # always fails
            initialDelaySeconds: 5
            periodSeconds: 5
            failureThreshold: 2
YAML
ok "liveness-flap      — failing liveness probe -> Unhealthy/Killing events, restarts"

# 7. Readiness never passes — Ready=False forever, but no restarts (clean Conditions demo).
kubectl "${KC[@]}" apply -n "$NS" -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: notready
  labels: { app: notready, k8shark.io/scenario: notready }
spec:
  replicas: 1
  selector: { matchLabels: { app: notready } }
  template:
    metadata: { labels: { app: notready } }
    spec:
      containers:
        - name: nginx
          image: nginx:alpine
          readinessProbe:
            exec: { command: ["/bin/sh", "-c", "exit 1"] }   # never ready
            initialDelaySeconds: 3
            periodSeconds: 5
YAML
ok "notready           — failing readiness probe -> Ready=False (no restarts)"

# 8. Unbound PVC — references a storage class that does not exist; pod stays Pending.
kubectl "${KC[@]}" apply -n "$NS" -f - <<'YAML'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: missing-storage
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: does-not-exist
  resources: { requests: { storage: 1Gi } }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pending-pvc
  labels: { app: pending-pvc, k8shark.io/scenario: pending-pvc }
spec:
  replicas: 1
  selector: { matchLabels: { app: pending-pvc } }
  template:
    metadata: { labels: { app: pending-pvc } }
    spec:
      containers:
        - name: nginx
          image: nginx:alpine
          volumeMounts: [{ name: data, mountPath: /data }]
      volumes:
        - name: data
          persistentVolumeClaim: { claimName: missing-storage }
YAML
ok "pending-pvc        — PVC on a missing StorageClass -> Pending (unbound PVC)"

# 9. Failing Job — retries until backoffLimit is hit, leaving several failed pods.
kubectl "${KC[@]}" apply -n "$NS_JOBS" -f - <<'YAML'
apiVersion: batch/v1
kind: Job
metadata:
  name: failing-job
  labels: { app: failing-job, k8shark.io/scenario: job-backoff }
spec:
  backoffLimit: 3
  template:
    metadata: { labels: { app: failing-job } }
    spec:
      restartPolicy: Never
      containers:
        - name: worker
          image: busybox:1.36
          command: ["/bin/sh", "-c", "echo working; sleep 3; echo failing; exit 1"]
YAML
ok "failing-job        — exits 1 -> Job backoff, BackoffLimitExceeded (in $NS_JOBS)"

# ── Let events accumulate ────────────────────────────────────────────────────
log "Waiting ~45s for failures to surface (restarts, backoffs, scheduling events)"
kubectl "${KC[@]}" rollout status deployment/healthy-web -n "$NS" --timeout=90s || true
sleep 45
kubectl "${KC[@]}" get pods -A -o wide | grep -E "NAMESPACE|$NS" || true

# ── Optional churn loop ──────────────────────────────────────────────────────
if [[ "$CHURN" -gt 0 ]]; then
  log "Churning healthy-web for ${CHURN}s to generate pod-lifecycle Timeline events"
  info "(rolling restarts + pod deletions every ~12s; Ctrl-C to stop early)"
  end=$((SECONDS + CHURN))
  while [[ $SECONDS -lt $end ]]; do
    kubectl "${KC[@]}" rollout restart deployment/healthy-web -n "$NS" >/dev/null 2>&1 || true
    pod=$(kubectl "${KC[@]}" get pods -n "$NS" -l app=healthy-web -o name 2>/dev/null | head -1)
    [[ -n "$pod" ]] && kubectl "${KC[@]}" delete -n "$NS" "$pod" --grace-period=0 --force >/dev/null 2>&1 || true
    info "churned at +$((SECONDS))s"
    sleep 12
  done
  ok "Churn complete"
fi

# ── Done ───────────────────────────────────────────────────────────────────────
printf '\n\033[1;32mChaos cluster ready.\033[0m\n\n'
info "Inspect the carnage:"
printf '    export KUBECONFIG=%s\n' "$KIND_KUBECONFIG"
printf '    kubectl get pods -n %s -o wide\n' "$NS"
printf '    kubectl get events -n %s --sort-by=.lastTimestamp\n\n' "$NS"
info "Capture it with k8shark (run for a few minutes so events keep flowing):"
printf '    make build\n'
printf '    ./kshrk capture --kubeconfig %s --auto-discover --duration 3m -o chaos.khsrk\n\n' "$KIND_KUBECONFIG"
info "Then explore the capture (UI on 18080/18081):"
printf '    ./kshrk ui chaos.khsrk --port 18080 --api-port 18081\n\n'
info "Tear down when finished:"
printf '    make kind-down\n\n'
