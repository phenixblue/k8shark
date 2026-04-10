#!/usr/bin/env bash
# k8shark end-to-end test script.
#
# Creates a KinD cluster, deploys a variety of Kubernetes resources, runs a
# short capture, opens the mock replay server, and asserts that kubectl
# commands against the capture return the expected data.
#
# Called automatically by:  make e2e
# Can also be run directly: ./scripts/e2e.sh
#
# Prerequisites: kind, kubectl (must be in PATH)
set -euo pipefail

# ── Configuration ──────────────────────────────────────────────────────────────
PROJ_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="k8shark-e2e-$$"
CAPTURE_FILE="/tmp/k8shark-e2e-$$.tar.gz"
CAPTURE_CONFIG="/tmp/k8shark-e2e-$$.yaml"
KIND_KUBECONFIG="/tmp/k8shark-kind-$$.yaml"
SERVER_LOG="/tmp/k8shark-server-$$.log"
BINARY="${BINARY:-${PROJ_ROOT}/kshrk}"
PASS=0
FAIL=0

# ── Helpers ────────────────────────────────────────────────────────────────────
log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
pass() { printf '  \033[1;32m[OK]   %s\033[0m\n' "$*"; PASS=$((PASS + 1)); }
fail() { printf '  \033[1;31m[FAIL] %s\033[0m\n' "$*"; FAIL=$((FAIL + 1)); }
die()  { printf '\n\033[1;31mFATAL: %s\033[0m\n' "$*" >&2; exit 1; }

assert_contains() {
  local desc="$1" haystack="$2" needle="$3"
  if echo "$haystack" | grep -q "$needle"; then
    pass "$desc"
  else
    fail "$desc (expected '$needle' in output)"
    info "output was: $(echo "$haystack" | head -5)"
  fi
}

assert_not_empty() {
  local desc="$1" val="$2"
  if [[ -n "$val" ]]; then
    pass "$desc"
  else
    fail "$desc (got empty output)"
  fi
}

# ── Cleanup ────────────────────────────────────────────────────────────────────
SERVER_PID=""
cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  info "Deleting KinD cluster '$CLUSTER_NAME'..."
  kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
  rm -f "$CAPTURE_FILE" "$CAPTURE_CONFIG" "$KIND_KUBECONFIG" "$SERVER_LOG"
}
trap cleanup EXIT

# ── Phase 1: Prerequisites ─────────────────────────────────────────────────────
log "Checking prerequisites"
for tool in kind kubectl; do
  if command -v "$tool" >/dev/null 2>&1; then
    pass "$tool found at $(command -v "$tool")"
  else
    die "'$tool' not found in PATH"
  fi
done
if [[ -x "$BINARY" ]]; then
  pass "kshrk binary found at $BINARY"
else
  die "Binary not found at '$BINARY'. Run 'make build' first."
fi

# ── Phase 2: KinD cluster ──────────────────────────────────────────────────────
log "Creating KinD cluster '$CLUSTER_NAME'"
kind create cluster \
  --name "$CLUSTER_NAME" \
  --kubeconfig "$KIND_KUBECONFIG" \
  --wait 90s
pass "KinD cluster ready"

KC=(--kubeconfig "$KIND_KUBECONFIG")

# ── Phase 3: Deploy test resources ────────────────────────────────────────────
log "Deploying test resources"

# Namespaces
kubectl "${KC[@]}" create namespace k8shark-test
kubectl "${KC[@]}" create namespace k8shark-jobs
pass "Namespaces k8shark-test and k8shark-jobs created"

# ConfigMap + Secret
kubectl "${KC[@]}" create configmap app-config \
  --from-literal=env=production \
  --from-literal=log-level=info \
  -n k8shark-test
kubectl "${KC[@]}" create secret generic app-secret \
  --from-literal=db-password=s3cr3t \
  -n k8shark-test
pass "ConfigMap and Secret created"

# Deployments
kubectl "${KC[@]}" create deployment nginx \
  --image=nginx:alpine --replicas=2 \
  -n k8shark-test
kubectl "${KC[@]}" label deployment nginx app=nginx \
  -n k8shark-test --overwrite
kubectl "${KC[@]}" create deployment redis \
  --image=redis:7-alpine --replicas=1 \
  -n k8shark-test
kubectl "${KC[@]}" label deployment redis app=redis \
  -n k8shark-test --overwrite
pass "Deployments nginx (x2) and redis (x1) created"

# Service
kubectl "${KC[@]}" expose deployment nginx \
  --port=80 --target-port=80 \
  -n k8shark-test
pass "Service nginx created"

# DaemonSet — uses pause so no image pull required from DockerHub
kubectl "${KC[@]}" apply -n k8shark-test -f - <<'YAML'
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: log-collector
  labels:
    app: log-collector
spec:
  selector:
    matchLabels:
      app: log-collector
  template:
    metadata:
      labels:
        app: log-collector
    spec:
      tolerations:
        - operator: Exists
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.9
          resources:
            limits:
              cpu: "10m"
              memory: "16Mi"
YAML
pass "DaemonSet log-collector created"

# Job
kubectl "${KC[@]}" apply -n k8shark-jobs -f - <<'YAML'
apiVersion: batch/v1
kind: Job
metadata:
  name: data-processor
  labels:
    app: data-processor
spec:
  template:
    metadata:
      labels:
        app: data-processor
    spec:
      restartPolicy: Never
      containers:
        - name: processor
          image: busybox:1.36
          command: ["/bin/sh", "-c", "echo 'processing'; sleep 5; echo 'done'"]
YAML
pass "Job data-processor created"

# ── Phase 4: Wait for workloads ────────────────────────────────────────────────
log "Waiting for workloads to be ready (timeout 120s each)"
kubectl "${KC[@]}" rollout status deployment/nginx        -n k8shark-test --timeout=120s
kubectl "${KC[@]}" rollout status deployment/redis        -n k8shark-test --timeout=120s
kubectl "${KC[@]}" rollout status daemonset/log-collector -n k8shark-test --timeout=120s
pass "All workloads ready"

info "Resource state at time of capture:"
kubectl "${KC[@]}" get pods,deployments,daemonsets -n k8shark-test 2>/dev/null || true
kubectl "${KC[@]}" get jobs -n k8shark-jobs 2>/dev/null || true

# ── Phase 5: Write capture config ─────────────────────────────────────────────
log "Writing capture config"
cat > "$CAPTURE_CONFIG" <<YAML
duration: 20s
output: ${CAPTURE_FILE}
kubeconfig: ${KIND_KUBECONFIG}
resources:
  - version: v1
    resource: namespaces
    interval: 5s
  - version: v1
    resource: nodes
    interval: 5s
  - version: v1
    resource: pods
    namespaces: [k8shark-test, k8shark-jobs]
    interval: 5s
  - group: apps
    version: v1
    resource: deployments
    namespaces: [k8shark-test]
    interval: 5s
  - group: apps
    version: v1
    resource: daemonsets
    namespaces: [k8shark-test]
    interval: 5s
  - group: batch
    version: v1
    resource: jobs
    namespaces: [k8shark-jobs]
    interval: 5s
  - version: v1
    resource: configmaps
    namespaces: [k8shark-test]
    interval: 5s
  - version: v1
    resource: secrets
    namespaces: [k8shark-test]
    interval: 5s
  - version: v1
    resource: services
    namespaces: [k8shark-test]
    interval: 5s
YAML
pass "Capture config written to $CAPTURE_CONFIG"

# ── Phase 6: Run capture ───────────────────────────────────────────────────────
log "Running kshrk capture (20s)"
"$BINARY" --config "$CAPTURE_CONFIG" capture

if [[ ! -s "$CAPTURE_FILE" ]]; then
  die "Capture archive is missing or empty: $CAPTURE_FILE"
fi
ARCHIVE_SIZE=$(du -h "$CAPTURE_FILE" | cut -f1)
pass "Capture archive written: $(basename "$CAPTURE_FILE") ($ARCHIVE_SIZE)"

# ── Phase 7: Start mock server ─────────────────────────────────────────────────
log "Starting kshrk open (mock server)"
"$BINARY" open "$CAPTURE_FILE" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

# Wait up to 15s for the mock server to emit its kubeconfig path.
E2E_KUBECONFIG=""
for i in $(seq 1 30); do
  if grep -q "Kubeconfig:" "$SERVER_LOG" 2>/dev/null; then
    E2E_KUBECONFIG=$(grep "Kubeconfig:" "$SERVER_LOG" | awk '{print $2}')
    break
  fi
  sleep 0.5
done

if [[ -z "$E2E_KUBECONFIG" ]]; then
  die "Mock server did not emit a kubeconfig path within 15s. Log: $(cat "$SERVER_LOG")"
fi
pass "Mock server running — kubeconfig: $E2E_KUBECONFIG"

# Actively probe until the mock server responds (up to 15s).
log "Waiting for mock server to be ready"
READY=false
for i in $(seq 1 30); do
  if kubectl --kubeconfig "$E2E_KUBECONFIG" --request-timeout=2s \
      get namespaces </dev/null &>/dev/null 2>&1; then
    READY=true
    break
  fi
  sleep 0.5
done
[[ "$READY" == "true" ]] || \
  die "Mock server did not become ready within 15s. Log:\n$(cat "$SERVER_LOG" 2>/dev/null)"
pass "Mock server is ready"

# All assertion kubectl calls get a 10s per-request timeout.
EKC=(--kubeconfig "$E2E_KUBECONFIG" --request-timeout=10s)

# ── Phase 8: E2E assertions ────────────────────────────────────────────────────
log "Running E2E assertions against the mock server"

# ── Discovery ──
out=$(kubectl "${EKC[@]}" api-versions 2>&1) || true
assert_contains "discovery: v1 present"      "$out" "^v1$"
assert_contains "discovery: apps/v1 present" "$out" "apps/v1"
assert_contains "discovery: batch/v1 present" "$out" "batch/v1"

# ── Namespaces ──
out=$(kubectl "${EKC[@]}" get namespaces -o name 2>&1) || true
assert_contains "namespace/k8shark-test present" "$out" "k8shark-test"
assert_contains "namespace/k8shark-jobs present" "$out" "k8shark-jobs"

# ── Nodes ──
out=$(kubectl "${EKC[@]}" get nodes -o name 2>&1) || true
assert_not_empty "nodes list non-empty" "$out"

# ── Pods ──
out=$(kubectl "${EKC[@]}" get pods -n k8shark-test -o name 2>&1) || true
assert_not_empty "pods present in k8shark-test" "$out"

# ── Deployments ──
out=$(kubectl "${EKC[@]}" get deployments -n k8shark-test -o name 2>&1) || true
assert_contains "deployment/nginx present" "$out" "nginx"
assert_contains "deployment/redis present" "$out" "redis"

# ── Single-item GET ──
out=$(kubectl "${EKC[@]}" get deployment nginx -n k8shark-test \
  -o jsonpath='{.metadata.name}' 2>&1) || true
assert_contains "single-item GET: deployment/nginx" "$out" "nginx"

# ── DaemonSet ──
out=$(kubectl "${EKC[@]}" get daemonsets -n k8shark-test -o name 2>&1) || true
assert_contains "daemonset/log-collector present" "$out" "log-collector"

# ── Job ──
out=$(kubectl "${EKC[@]}" get jobs -n k8shark-jobs -o name 2>&1) || true
assert_contains "job/data-processor present" "$out" "data-processor"

# ── ConfigMap ──
out=$(kubectl "${EKC[@]}" get configmaps -n k8shark-test -o name 2>&1) || true
assert_contains "configmap/app-config present" "$out" "app-config"

# ── Service ──
out=$(kubectl "${EKC[@]}" get services -n k8shark-test -o name 2>&1) || true
assert_contains "service/nginx present" "$out" "nginx"

# ── Label selector ──
out=$(kubectl "${EKC[@]}" get pods -n k8shark-test -l app=nginx -o name 2>&1) || true
assert_not_empty "label selector app=nginx matches pods" "$out"
if echo "$out" | grep -qi "redis"; then
  fail "label selector app=nginx unexpectedly returned redis pods"
else
  pass "label selector correctly excludes non-matching pods"
fi

# ── Field selector ──
out=$(kubectl "${EKC[@]}" get pods -n k8shark-test \
  --field-selector='metadata.namespace=k8shark-test' -o name 2>&1) || true
assert_not_empty "field selector metadata.namespace returns pods" "$out"

# ── Watch: --request-timeout=5s lets kubectl exit naturally when the server
#           closes the stream (our server honours timeoutSeconds).
WATCH_LOG=$(mktemp)
kubectl --kubeconfig "$E2E_KUBECONFIG" --request-timeout=5s \
  get pods -n k8shark-test --watch -o name >"$WATCH_LOG" 2>&1 || true
out=$(cat "$WATCH_LOG")
rm -f "$WATCH_LOG"
assert_not_empty "watch: initial pod events received" "$out"

# ── Phase 9: Summary ───────────────────────────────────────────────────────────
log "Test summary"
printf '  Passed: \033[1;32m%d\033[0m\n' "$PASS"
printf '  Failed: \033[1;31m%d\033[0m\n' "$FAIL"
if [[ "$FAIL" -gt 0 ]]; then
  printf '\n\033[1;31mE2E tests FAILED (%d failure(s))\033[0m\n' "$FAIL" >&2
  exit 1
fi
printf '\n\033[1;32mAll %d E2E assertions passed!\033[0m\n' "$PASS"
