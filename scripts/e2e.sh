#!/usr/bin/env bash
# k8shark end-to-end test script.
#
# Creates a KinD cluster, deploys a variety of Kubernetes resources, runs a
# short capture, opens the mock replay server, and asserts that kubectl
# commands against the capture return the expected data.
#
# Called automatically by:  make e2e
# Can also be run directly: ./scripts/e2e.sh
# Env:  NODE_IMAGE=kindest/node:v1.32.3   pin a Kubernetes version
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
# Pin the Kubernetes version, e.g. NODE_IMAGE=kindest/node:v1.32.3. Empty means
# whatever kind's own default is for the installed kind version. Same knob
# scripts/conformance.sh already exposes — capture reads the generic
# REST/discovery surface, so the interesting question this makes answerable is
# "how far back does that actually hold", across the whole 100+ assertion suite
# rather than just the conformance diff.
NODE_IMAGE="${NODE_IMAGE:-}"
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

assert_equals() {
  local desc="$1" got="$2" want="$3"
  if [[ "$got" == "$want" ]]; then
    pass "$desc"
  else
    fail "$desc"
    info "want: $want"
    info "got:  $got"
  fi
}

# sorted_names runs a jsonpath query against a kubectl output and returns a
# sorted, newline-separated list of names — for stable round-trip comparison.
sorted_names() {
  local kc="$1"; shift
  kubectl --kubeconfig "$kc" --request-timeout=10s "$@" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
    | sort
}

# ── Cleanup ────────────────────────────────────────────────────────────────────
SERVER_PID=""
# Phase 9c (encrypt/decrypt) temp paths and background-server PIDs. Declared
# here (before Phase 9c assigns them) so cleanup() can always reference them
# under `set -u`, and so a `set -e` exit partway through Phase 9c — leaving
# the private age identity file or a background mock server behind — is
# still cleaned up rather than relying on the rm/kill calls at the end of
# each Phase 9c block, which an early exit skips.
ENC_PASS_FILE=""
ENC_WRONG_PASS_FILE=""
ENC_FILE=""
DEC_FILE=""
ENC_DEC_SERVER_LOG=""
ENC_DEC_KC=""
ENC_DEC_SERVER_PID=""
ENC_OPEN_SERVER_LOG=""
ENC_OPEN_KC=""
ENC_OPEN_SERVER_PID=""
KEYGEN_SRC=""
AGE_IDENTITY_FILE=""
ENC_RECIPIENT_FILE=""
DEC_RECIPIENT_FILE=""
REC_SERVER_LOG=""
REC_KC=""
REC_SERVER_PID=""
# Phase 8f (writable overlay) background server.
WRITABLE_SERVER_LOG=""
WRITABLE_KUBECONFIG=""
WRITABLE_SERVER_PID=""
cleanup() {
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  for pid in "$ENC_DEC_SERVER_PID" "$ENC_OPEN_SERVER_PID" "$REC_SERVER_PID" "$WRITABLE_SERVER_PID"; do
    if [[ -n "$pid" ]]; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    fi
  done
  info "Deleting KinD cluster '$CLUSTER_NAME'..."
  kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
  # Phase 9c's paths may still be "" if the script exited before reaching
  # that phase; filter those out rather than passing empty strings to rm.
  local cleanup_paths=(
    "$CAPTURE_FILE" "$CAPTURE_CONFIG" "$KIND_KUBECONFIG" "$SERVER_LOG"
    "$ENC_PASS_FILE" "$ENC_WRONG_PASS_FILE" "$ENC_FILE" "$DEC_FILE"
    "$ENC_DEC_SERVER_LOG" "$ENC_DEC_KC" "$ENC_OPEN_SERVER_LOG" "$ENC_OPEN_KC"
    "$KEYGEN_SRC" "$AGE_IDENTITY_FILE" "$ENC_RECIPIENT_FILE" "$DEC_RECIPIENT_FILE"
    "$REC_SERVER_LOG" "$REC_KC" "$WRITABLE_SERVER_LOG" "$WRITABLE_KUBECONFIG"
  )
  for p in "${cleanup_paths[@]}"; do
    [[ -n "$p" ]] && rm -f "$p"
  done
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
log "Creating KinD cluster '$CLUSTER_NAME'${NODE_IMAGE:+ (image $NODE_IMAGE)}"
if [[ -n "$NODE_IMAGE" ]]; then
  kind create cluster \
    --name "$CLUSTER_NAME" \
    --kubeconfig "$KIND_KUBECONFIG" \
    --image "$NODE_IMAGE" \
    --wait 90s
else
  kind create cluster \
    --name "$CLUSTER_NAME" \
    --kubeconfig "$KIND_KUBECONFIG" \
    --wait 90s
fi
pass "KinD cluster ready"
info "Kubernetes server: $(kubectl --kubeconfig "$KIND_KUBECONFIG" version -o json 2>/dev/null | jq -r '.serverVersion.gitVersion' 2>/dev/null || echo unknown)"

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

# StatefulSet with PVC (nginx with persistent storage)
kubectl "${KC[@]}" apply -n k8shark-test -f - <<'YAML'
apiVersion: v1
kind: Service
metadata:
  name: nginx-service
  labels:
    app: nginx-stateful
spec:
  ports:
  - port: 80
    name: web
  clusterIP: None
  selector:
    app: nginx-stateful
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: web
spec:
  serviceName: "nginx-service"
  replicas: 3
  selector:
    matchLabels:
      app: nginx-stateful
  template:
    metadata:
      labels:
        app: nginx-stateful
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
        ports:
        - containerPort: 80
          name: web
        volumeMounts:
        - name: www
          mountPath: /usr/share/nginx/html
  volumeClaimTemplates:
  - metadata:
      name: www
    spec:
      accessModes: ["ReadWriteOnce"]
      storageClassName: "standard"
      resources:
        requests:
          storage: 1Gi
YAML
pass "StatefulSet web (x3) with PVCs created"

# ── Phase 4: Wait for workloads ────────────────────────────────────────────────
log "Waiting for workloads to be ready (timeout 120s each)"
kubectl "${KC[@]}" rollout status deployment/nginx        -n k8shark-test --timeout=120s
kubectl "${KC[@]}" rollout status deployment/redis        -n k8shark-test --timeout=120s
kubectl "${KC[@]}" rollout status daemonset/log-collector -n k8shark-test --timeout=120s
kubectl "${KC[@]}" rollout status statefulset/web         -n k8shark-test --timeout=120s
pass "All workloads ready"

# ── Phase 4b: Snapshot live cluster state (for round-trip comparison later) ───
log "Snapshotting live cluster state"
LIVE_POD_NAMES=$(sorted_names      "$KIND_KUBECONFIG" get pods         -n k8shark-test)
LIVE_DEPLOY_NAMES=$(sorted_names   "$KIND_KUBECONFIG" get deployments  -n k8shark-test)
LIVE_DS_NAMES=$(sorted_names       "$KIND_KUBECONFIG" get daemonsets   -n k8shark-test)
LIVE_STS_NAMES=$(sorted_names      "$KIND_KUBECONFIG" get statefulsets -n k8shark-test)
LIVE_JOB_NAMES=$(sorted_names      "$KIND_KUBECONFIG" get jobs         -n k8shark-jobs)
LIVE_NODE_NAMES=$(sorted_names     "$KIND_KUBECONFIG" get nodes)
LIVE_NGINX_REPLICAS=$(kubectl --kubeconfig "$KIND_KUBECONFIG" get deployment nginx \
  -n k8shark-test -o jsonpath='{.spec.replicas}' 2>/dev/null)
LIVE_NGINX_IMAGE=$(kubectl --kubeconfig "$KIND_KUBECONFIG" get deployment nginx \
  -n k8shark-test -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)
pass "Live state snapshot saved"

info "Resource state at time of capture:"
kubectl "${KC[@]}" get pods,deployments,daemonsets,statefulsets,pvc -n k8shark-test 2>/dev/null || true
kubectl "${KC[@]}" get jobs -n k8shark-jobs 2>/dev/null || true
kubectl "${KC[@]}" get persistentvolumes 2>/dev/null || true

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
    logs: 50
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
  - group: apps
    version: v1
    resource: statefulsets
    namespaces: [k8shark-test]
    interval: 5s
  - version: v1
    resource: persistentvolumeclaims
    namespaces: [k8shark-test]
    interval: 5s
  - version: v1
    resource: persistentvolumes
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

# ── Phase 6b: kshrk inspect ───────────────────────────────────────────────────
log "Testing kshrk inspect"

# Table output
out=$("$BINARY" inspect "$CAPTURE_FILE" 2>&1) || true
assert_contains "inspect: Capture ID present"          "$out" "Capture ID:"
assert_contains "inspect: Kubernetes version present"  "$out" "Kubernetes:"
assert_contains "inspect: Record count present"         "$out" "Records:"
assert_contains "inspect: pods resource listed"         "$out" "pods"
assert_contains "inspect: deployments resource listed"  "$out" "deployments"
assert_contains "inspect: secrets resource listed"      "$out" "secrets"

# JSON output
out=$("$BINARY" inspect "$CAPTURE_FILE" -o json 2>&1) || true
INSPECT_RECORDS=$(echo "$out" | jq -r '.record_count' 2>/dev/null || echo "")
INSPECT_VERSION=$(echo "$out" | jq -r '.kubernetes_version' 2>/dev/null || echo "")
assert_not_empty "inspect -o json: kubernetes_version present" "$INSPECT_VERSION"
if [[ -n "$INSPECT_RECORDS" && "$INSPECT_RECORDS" -gt 0 ]]; then
  pass "inspect -o json: record_count > 0 ($INSPECT_RECORDS)"
else
  fail "inspect -o json: expected record_count > 0, got '${INSPECT_RECORDS}'"
fi

# ── Phase 6c: capture with --redact-secrets and --allow-secret ────────────────
log "Testing kshrk capture --redact-secrets --allow-secret"
INLINE_REDACTED_FILE="${CAPTURE_FILE%.tar.gz}-inline-redacted.tar.gz"

# Capture again with inline redaction; allow app-secret so we can verify it is
# preserved while other secrets are redacted.
"$BINARY" --config "$CAPTURE_CONFIG" capture \
  --redact-secrets \
  --allow-secret "k8shark-test/app-secret" \
  --out "$INLINE_REDACTED_FILE"

if [[ -s "$INLINE_REDACTED_FILE" ]]; then
  pass "capture --redact-secrets: output archive created"
else
  fail "capture --redact-secrets: output archive missing or empty"
fi

INLINE_SERVER_LOG="/tmp/k8shark-inline-server-$$.log"
INLINE_SERVER_PID=""
"$BINARY" open "$INLINE_REDACTED_FILE" >"$INLINE_SERVER_LOG" 2>&1 &
INLINE_SERVER_PID=$!
INLINE_KUBECONFIG=""
for i in $(seq 1 30); do
  if grep -q "Kubeconfig:" "$INLINE_SERVER_LOG" 2>/dev/null; then
    INLINE_KUBECONFIG=$(grep "Kubeconfig:" "$INLINE_SERVER_LOG" | awk '{print $2}')
    break
  fi
  sleep 0.5
done

if [[ -z "$INLINE_KUBECONFIG" ]]; then
  fail "capture --redact-secrets: mock server did not start within 15s"
else
  pass "capture --redact-secrets: mock server started"

  for i in $(seq 1 20); do
    if kubectl --kubeconfig "$INLINE_KUBECONFIG" --request-timeout=2s \
        get namespaces </dev/null &>/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done

  REDACTED_B64="UkVEQUNURUQ="

  # app-secret should be preserved (allowlisted).
  app_val=$(kubectl --kubeconfig "$INLINE_KUBECONFIG" --request-timeout=10s \
    get secret app-secret -n k8shark-test \
    -o jsonpath='{.data.db-password}' 2>/dev/null || echo "")
  if [[ "$app_val" != "$REDACTED_B64" && -n "$app_val" ]]; then
    pass "capture --redact-secrets: allowlisted secret (app-secret) data preserved"
  elif [[ -z "$app_val" ]]; then
    fail "capture --redact-secrets: could not read app-secret data"
  else
    fail "capture --redact-secrets: app-secret was redacted despite --allow-secret"
  fi

  # Other secrets in k8shark-test should be redacted.
  other_secret=$(kubectl --kubeconfig "$INLINE_KUBECONFIG" --request-timeout=10s \
    get secrets -n k8shark-test -o name 2>/dev/null \
    | grep -v "app-secret" | head -1 || echo "")
  if [[ -n "$other_secret" ]]; then
    other_data=$(kubectl --kubeconfig "$INLINE_KUBECONFIG" --request-timeout=10s \
      get "$other_secret" -n k8shark-test \
      -o jsonpath='{range .data.*}{@}{"\n"}{end}' 2>/dev/null | head -1 || echo "")
    if [[ "$other_data" == "$REDACTED_B64" ]]; then
      pass "capture --redact-secrets: non-allowlisted secret data redacted"
    elif [[ -z "$other_data" ]]; then
      info "capture --redact-secrets: other secret had no data fields to check"
    else
      fail "capture --redact-secrets: non-allowlisted secret not redacted (got '$other_data')"
    fi
  else
    info "capture --redact-secrets: no other secrets found in k8shark-test to verify"
  fi
fi

if [[ -n "$INLINE_SERVER_PID" ]]; then
  kill "$INLINE_SERVER_PID" 2>/dev/null || true
  wait "$INLINE_SERVER_PID" 2>/dev/null || true
fi
rm -f "$INLINE_REDACTED_FILE" "$INLINE_SERVER_LOG"

# ── Phase 6d: inline capture with --redact-field ─────────────────────────────
log "Testing kshrk capture --redact-field (inline field-level redaction)"
INLINE_FIELD_FILE="${CAPTURE_FILE%.tar.gz}-inline-field.tar.gz"
INLINE_FIELD_SERVER_LOG="/tmp/k8shark-inline-field-server-$$.log"
INLINE_FIELD_KC="/tmp/k8shark-inline-field-kc-$$.yaml"
INLINE_FIELD_SERVER_PID=""

# Capture with --redact-field targeting ConfigMap data.env; leave data.log-level
# untouched so we can assert field-level selectivity.
"$BINARY" --config "$CAPTURE_CONFIG" capture \
  --redact-field "data.env:ConfigMap:REDACTED" \
  --out "$INLINE_FIELD_FILE"

if [[ -s "$INLINE_FIELD_FILE" ]]; then
  pass "capture --redact-field: output archive created"
else
  fail "capture --redact-field: output archive missing or empty"
fi

"$BINARY" open "$INLINE_FIELD_FILE" --kubeconfig-out "$INLINE_FIELD_KC" \
  >"$INLINE_FIELD_SERVER_LOG" 2>&1 &
INLINE_FIELD_SERVER_PID=$!
for i in $(seq 1 30); do
  if [[ -s "$INLINE_FIELD_KC" ]]; then break; fi
  sleep 0.5
done

if [[ ! -s "$INLINE_FIELD_KC" ]]; then
  fail "capture --redact-field: mock server did not start within 15s"
else
  pass "capture --redact-field: mock server started"
  for i in $(seq 1 20); do
    if kubectl --kubeconfig "$INLINE_FIELD_KC" --request-timeout=2s \
        get namespaces </dev/null &>/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done

  # data.env should be replaced.
  env_val=$(kubectl --kubeconfig "$INLINE_FIELD_KC" --request-timeout=10s \
    get configmap app-config -n k8shark-test \
    -o jsonpath='{.data.env}' 2>/dev/null || echo "")
  assert_equals "capture --redact-field: data.env replaced with REDACTED" \
    "$env_val" "REDACTED"

  # data.log-level should be untouched (not covered by the rule).
  loglevel_val=$(kubectl --kubeconfig "$INLINE_FIELD_KC" --request-timeout=10s \
    get configmap app-config -n k8shark-test \
    -o jsonpath='{.data.log-level}' 2>/dev/null || echo "")
  assert_equals "capture --redact-field: data.log-level preserved (not in rule)" \
    "$loglevel_val" "info"

  # Secrets should NOT be redacted (no --redact-secrets flag used).
  REDACTED_B64_INLINE="UkVEQUNURUQ="
  app_val=$(kubectl --kubeconfig "$INLINE_FIELD_KC" --request-timeout=10s \
    get secret app-secret -n k8shark-test \
    -o jsonpath='{.data.db-password}' 2>/dev/null || echo "")
  if [[ -n "$app_val" && "$app_val" != "$REDACTED_B64_INLINE" ]]; then
    pass "capture --redact-field: secret data NOT redacted (no --redact-secrets)"
  elif [[ -z "$app_val" ]]; then
    info "capture --redact-field: could not read secret data (may be ok)"
  else
    fail "capture --redact-field: secret was unexpectedly redacted"
  fi
fi

if [[ -n "$INLINE_FIELD_SERVER_PID" ]]; then
  kill "$INLINE_FIELD_SERVER_PID" 2>/dev/null || true
  wait "$INLINE_FIELD_SERVER_PID" 2>/dev/null || true
fi
rm -f "$INLINE_FIELD_FILE" "$INLINE_FIELD_SERVER_LOG" "$INLINE_FIELD_KC"

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
#           closes the stream (our server honors timeoutSeconds).
WATCH_LOG=$(mktemp)
kubectl --kubeconfig "$E2E_KUBECONFIG" --request-timeout=5s \
  get pods -n k8shark-test --watch -o name >"$WATCH_LOG" 2>&1 || true
out=$(cat "$WATCH_LOG")
rm -f "$WATCH_LOG"
assert_not_empty "watch: initial pod events received" "$out"

# ── StatefulSet ──
out=$(kubectl "${EKC[@]}" get statefulsets -n k8shark-test -o name 2>&1) || true
assert_contains "statefulset/web present" "$out" "web"

out=$(kubectl "${EKC[@]}" get statefulset web -n k8shark-test \
  -o jsonpath='{.metadata.name}' 2>&1) || true
assert_contains "single-item GET: statefulset/web" "$out" "web"

# ── PersistentVolumeClaims ──
out=$(kubectl "${EKC[@]}" get pvc -n k8shark-test -o name 2>&1) || true
assert_not_empty "PVCs present in k8shark-test" "$out"
assert_contains "PVC www-web-0 present" "$out" "www-web-0"

# ── PersistentVolumes ──
out=$(kubectl "${EKC[@]}" get pv -o name 2>&1) || true
assert_not_empty "PersistentVolumes present (cluster-scoped)" "$out"

# ── Phase 8b: kshrk redact ────────────────────────────────────────────────────
log "Testing kshrk redact"
REDACTED_FILE="${CAPTURE_FILE%.tar.gz}-redacted.tar.gz"
REDACTED_SERVER_LOG="/tmp/k8shark-redacted-server-$$.log"
REDACTED_KC="/tmp/k8shark-redacted-kc-$$.yaml"
REDACTED_SERVER_PID=""

# Run redact on the original (un-redacted) capture without an allowlist so
# all secrets (including app-secret) are redacted.
redact_out=$("$BINARY" redact "$CAPTURE_FILE" \
  --out "$REDACTED_FILE" \
  --redact-secrets 2>&1) || true
assert_contains "redact: success message present"       "$redact_out" "Redacted"
assert_contains "redact: reported secrets redacted"     "$redact_out" "secret"
if [[ -s "$REDACTED_FILE" ]]; then
  pass "redact: output archive created"
else
  fail "redact: output archive missing or empty"
fi

# Count redacted secrets from message (expect > 0).
REDACTED_COUNT=$(echo "$redact_out" | grep -oE '[0-9]+ secret' | grep -oE '[0-9]+' || echo "0")
if [[ "$REDACTED_COUNT" -gt 0 ]]; then
  pass "redact: $REDACTED_COUNT secret(s) redacted"
else
  fail "redact: expected > 0 secrets redacted, got 0"
fi

# Open the redacted archive and verify secret data values are REDACTED.
# Use --kubeconfig-out so this server writes its kubeconfig to a private temp
# file and does NOT overwrite the original server's kubeconfig (both archives
# share the same capture ID, which would stomp the original kubeconfig path
# causing Phase 9 round-trip queries to hit a dead server port).
"$BINARY" open "$REDACTED_FILE" --kubeconfig-out "$REDACTED_KC" >"$REDACTED_SERVER_LOG" 2>&1 &
REDACTED_SERVER_PID=$!
REDACTED_KUBECONFIG=""
for i in $(seq 1 30); do
  if [[ -s "$REDACTED_KC" ]]; then
    REDACTED_KUBECONFIG="$REDACTED_KC"
    break
  fi
  sleep 0.5
done

if [[ -z "$REDACTED_KUBECONFIG" ]]; then
  fail "redact: mock server for redacted archive did not start within 15s"
else
  pass "redact: mock server for redacted archive started"

  # Wait for the redacted server to be ready.
  for i in $(seq 1 20); do
    if kubectl --kubeconfig "$REDACTED_KUBECONFIG" --request-timeout=2s \
        get namespaces </dev/null &>/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done

  REDACTED_B64="UkVEQUNURUQ="

  # app-secret should be redacted (no allowlist applied).
  app_val=$(kubectl --kubeconfig "$REDACTED_KUBECONFIG" --request-timeout=10s \
    get secret app-secret -n k8shark-test \
    -o jsonpath='{.data.db-password}' 2>/dev/null || echo "")
  if [[ "$app_val" == "$REDACTED_B64" ]]; then
    pass "redact: secret data replaced with REDACTED"
  elif [[ -z "$app_val" ]]; then
    fail "redact: could not read app-secret data from redacted archive"
  else
    fail "redact: app-secret data was not redacted (got '$app_val')"
  fi
fi

# Kill the redacted mock server.
if [[ -n "$REDACTED_SERVER_PID" ]]; then
  kill "$REDACTED_SERVER_PID" 2>/dev/null || true
  wait "$REDACTED_SERVER_PID" 2>/dev/null || true
fi
rm -f "$REDACTED_FILE" "$REDACTED_SERVER_LOG" "$REDACTED_KC"

# ── Phase 8d: kshrk redact --redact-field (CLI field-level redaction) ─────────
log "Testing kshrk redact --redact-field (field-level redaction via CLI)"
FIELD_REDACTED_FILE="${CAPTURE_FILE%.tar.gz}-field-redacted.tar.gz"
FIELD_REDACTED_SERVER_LOG="/tmp/k8shark-field-redacted-server-$$.log"
FIELD_REDACTED_KC="/tmp/k8shark-field-redacted-kc-$$.yaml"
FIELD_REDACTED_SERVER_PID=""

# Target only data.env in ConfigMaps; leave data.log-level and all secrets alone.
field_redact_out=$("$BINARY" redact "$CAPTURE_FILE" \
  --out "$FIELD_REDACTED_FILE" \
  --redact-field "data.env:ConfigMap:REDACTED" \
  2>&1) || true

assert_contains "field-redact: success message present"   "$field_redact_out" "Redacted"
assert_contains "field-redact: field count in output"     "$field_redact_out" "field"
if [[ -s "$FIELD_REDACTED_FILE" ]]; then
  pass "field-redact: output archive created"
else
  fail "field-redact: output archive missing or empty"
fi

FIELD_COUNT=$(echo "$field_redact_out" | grep -oE '[0-9]+[[:space:]]+field' \
  | grep -oE '^[0-9]+' || echo "0")
if [[ "$FIELD_COUNT" -gt 0 ]]; then
  pass "field-redact: $FIELD_COUNT field(s) reported redacted"
else
  fail "field-redact: expected > 0 fields redacted, got 0"
fi

"$BINARY" open "$FIELD_REDACTED_FILE" --kubeconfig-out "$FIELD_REDACTED_KC" \
  >"$FIELD_REDACTED_SERVER_LOG" 2>&1 &
FIELD_REDACTED_SERVER_PID=$!
for i in $(seq 1 30); do
  if [[ -s "$FIELD_REDACTED_KC" ]]; then break; fi
  sleep 0.5
done

if [[ ! -s "$FIELD_REDACTED_KC" ]]; then
  fail "field-redact: mock server did not start within 15s"
else
  pass "field-redact: mock server started"
  for i in $(seq 1 20); do
    if kubectl --kubeconfig "$FIELD_REDACTED_KC" --request-timeout=2s \
        get namespaces </dev/null &>/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done

  # data.env should be overwritten by the rule.
  env_val=$(kubectl --kubeconfig "$FIELD_REDACTED_KC" --request-timeout=10s \
    get configmap app-config -n k8shark-test \
    -o jsonpath='{.data.env}' 2>/dev/null || echo "")
  assert_equals "field-redact: data.env replaced with REDACTED" "$env_val" "REDACTED"

  # data.log-level is NOT in the rule and must remain intact.
  loglevel_val=$(kubectl --kubeconfig "$FIELD_REDACTED_KC" --request-timeout=10s \
    get configmap app-config -n k8shark-test \
    -o jsonpath='{.data.log-level}' 2>/dev/null || echo "")
  assert_equals "field-redact: data.log-level preserved (not in rule)" \
    "$loglevel_val" "info"

  # No --redact-secrets flag → secret data must NOT be redacted.
  REDACTED_B64_FTEST="UkVEQUNURUQ="
  secret_val=$(kubectl --kubeconfig "$FIELD_REDACTED_KC" --request-timeout=10s \
    get secret app-secret -n k8shark-test \
    -o jsonpath='{.data.db-password}' 2>/dev/null || echo "")
  if [[ -n "$secret_val" && "$secret_val" != "$REDACTED_B64_FTEST" ]]; then
    pass "field-redact: secret data NOT redacted (no --redact-secrets)"
  elif [[ -z "$secret_val" ]]; then
    info "field-redact: could not read secret data to verify"
  else
    fail "field-redact: secret was unexpectedly redacted"
  fi
fi

if [[ -n "$FIELD_REDACTED_SERVER_PID" ]]; then
  kill "$FIELD_REDACTED_SERVER_PID" 2>/dev/null || true
  wait "$FIELD_REDACTED_SERVER_PID" 2>/dev/null || true
fi
rm -f "$FIELD_REDACTED_FILE" "$FIELD_REDACTED_SERVER_LOG" "$FIELD_REDACTED_KC"

# ── Phase 8e: kshrk redact --config with redaction.rules ──────────────────────
log "Testing kshrk redact --config (config-driven redaction.rules + redactSecrets)"
CFGREDACT_FILE="${CAPTURE_FILE%.tar.gz}-cfgredact.tar.gz"
CFGREDACT_CONFIG="/tmp/k8shark-cfgredact-$$.yaml"
CFGREDACT_SERVER_LOG="/tmp/k8shark-cfgredact-server-$$.log"
CFGREDACT_KC="/tmp/k8shark-cfgredact-kc-$$.yaml"
CFGREDACT_SERVER_PID=""

# Redact data.log-level in ConfigMaps (via rules), redact all secrets except
# app-secret (which is allowlisted); leave data.env untouched.
cat > "$CFGREDACT_CONFIG" <<YAML
duration: 5m
resources: []
redaction:
  redactSecrets: true
  allowSecrets:
    - k8shark-test/app-secret
  rules:
    - fieldPath: "data.log-level"
      kind: ConfigMap
      replacement: SANITIZED
YAML
pass "config-redact: redaction config written"

cfg_redact_out=$("$BINARY" redact "$CAPTURE_FILE" \
  --out "$CFGREDACT_FILE" \
  --config "$CFGREDACT_CONFIG" \
  2>&1) || true

assert_contains "config-redact: success message present" "$cfg_redact_out" "Redacted"
if [[ -s "$CFGREDACT_FILE" ]]; then
  pass "config-redact: output archive created"
else
  fail "config-redact: output archive missing or empty"
fi

"$BINARY" open "$CFGREDACT_FILE" --kubeconfig-out "$CFGREDACT_KC" \
  >"$CFGREDACT_SERVER_LOG" 2>&1 &
CFGREDACT_SERVER_PID=$!
for i in $(seq 1 30); do
  if [[ -s "$CFGREDACT_KC" ]]; then break; fi
  sleep 0.5
done

if [[ ! -s "$CFGREDACT_KC" ]]; then
  fail "config-redact: mock server did not start within 15s"
else
  pass "config-redact: mock server started"
  for i in $(seq 1 20); do
    if kubectl --kubeconfig "$CFGREDACT_KC" --request-timeout=2s \
        get namespaces </dev/null &>/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done

  # data.log-level should be replaced by the config rule.
  loglevel_val=$(kubectl --kubeconfig "$CFGREDACT_KC" --request-timeout=10s \
    get configmap app-config -n k8shark-test \
    -o jsonpath='{.data.log-level}' 2>/dev/null || echo "")
  assert_equals "config-redact: data.log-level replaced via config rule" \
    "$loglevel_val" "SANITIZED"

  # data.env is NOT in the rules and must remain "production".
  env_val=$(kubectl --kubeconfig "$CFGREDACT_KC" --request-timeout=10s \
    get configmap app-config -n k8shark-test \
    -o jsonpath='{.data.env}' 2>/dev/null || echo "")
  assert_equals "config-redact: data.env preserved (not in rules)" \
    "$env_val" "production"

  # app-secret is allowlisted → its data must be preserved.
  REDACTED_B64_CFG="UkVEQUNURUQ="
  app_val=$(kubectl --kubeconfig "$CFGREDACT_KC" --request-timeout=10s \
    get secret app-secret -n k8shark-test \
    -o jsonpath='{.data.db-password}' 2>/dev/null || echo "")
  if [[ -n "$app_val" && "$app_val" != "$REDACTED_B64_CFG" ]]; then
    pass "config-redact: allowlisted app-secret data preserved"
  elif [[ -z "$app_val" ]]; then
    fail "config-redact: could not read app-secret data"
  else
    fail "config-redact: app-secret redacted despite allowSecrets in config"
  fi

  # A non-allowlisted secret (if any exists in k8shark-test) should be redacted.
  other_secret=$(kubectl --kubeconfig "$CFGREDACT_KC" --request-timeout=10s \
    get secrets -n k8shark-test -o name 2>/dev/null \
    | grep -v "app-secret" | head -1 || echo "")
  if [[ -n "$other_secret" ]]; then
    other_data=$(kubectl --kubeconfig "$CFGREDACT_KC" --request-timeout=10s \
      get "$other_secret" -n k8shark-test \
      -o jsonpath='{range .data.*}{@}{"\n"}{end}' 2>/dev/null | head -1 || echo "")
    if [[ "$other_data" == "$REDACTED_B64_CFG" ]]; then
      pass "config-redact: non-allowlisted secret data redacted (redactSecrets: true)"
    elif [[ -z "$other_data" ]]; then
      info "config-redact: other secret had no data fields to verify"
    else
      fail "config-redact: non-allowlisted secret not redacted (got '$other_data')"
    fi
  else
    info "config-redact: no other secrets in k8shark-test to verify redactSecrets"
  fi
fi

if [[ -n "$CFGREDACT_SERVER_PID" ]]; then
  kill "$CFGREDACT_SERVER_PID" 2>/dev/null || true
  wait "$CFGREDACT_SERVER_PID" 2>/dev/null || true
fi
rm -f "$CFGREDACT_FILE" "$CFGREDACT_CONFIG" "$CFGREDACT_SERVER_LOG" "$CFGREDACT_KC"

# ── Phase 8c: kubectl logs ────────────────────────────────────────────────────
log "Testing kubectl logs via mock server"

# Get the name of one nginx pod from the mock server.
NGINX_POD=$(kubectl "${EKC[@]}" get pods -n k8shark-test -l app=nginx \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
if [[ -z "$NGINX_POD" ]]; then
  fail "kubectl logs: could not find an nginx pod in mock server"
else
  log_out=$(kubectl --kubeconfig "$E2E_KUBECONFIG" --request-timeout=10s \
    logs "$NGINX_POD" -n k8shark-test 2>&1) || true
  assert_not_empty "kubectl logs: captured nginx pod log is non-empty" "$log_out"
fi

# A pod whose logs were not captured should return the k8shark stub message.
# kubectl logs first fetches the pod (which would 404 for a nonexistent pod),
# so we hit the /log sub-resource directly via curl to test the stub path.
# Use -k because the mock server uses a self-signed TLS cert.
SERVER_ADDR=$(kubectl config --kubeconfig "$E2E_KUBECONFIG" view \
  --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || echo "")
stub_out=$(curl -sk "${SERVER_ADDR}/api/v1/namespaces/k8shark-test/pods/nonexistent-pod/log" 2>&1) || true
assert_contains "kubectl logs: stub message mentions k8shark"       "$stub_out" "k8shark"
assert_contains "kubectl logs: stub message mentions not captured" "$stub_out" "not captured"

# ── Phase 8f: Writable overlay smoke test ──────────────────────────────────────
# Exercises the write verbs (create/patch/apply/scale/delete) against
# --writable on every PR, not just the workflow_dispatch-only e2e-kwok job
# (#252) — kwok itself isn't needed for these, just the overlay.
log "Testing writable overlay (create/patch/apply/scale/delete)"

WRITABLE_KUBECONFIG="/tmp/k8shark-e2e-writable-$$.yaml"
WRITABLE_SERVER_LOG="/tmp/k8shark-e2e-writable-server-$$.log"
# No --loop: a loop wrap would reset the overlay mid-test.
"$BINARY" replay "$CAPTURE_FILE" --writable --kubeconfig-out "$WRITABLE_KUBECONFIG" \
  >"$WRITABLE_SERVER_LOG" 2>&1 &
WRITABLE_SERVER_PID=$!

WRITABLE_READY=false
for i in $(seq 1 30); do
  if [[ -s "$WRITABLE_KUBECONFIG" ]] && kubectl --kubeconfig "$WRITABLE_KUBECONFIG" \
      --request-timeout=2s get namespaces &>/dev/null; then
    WRITABLE_READY=true
    break
  fi
  sleep 0.5
done
if [[ "$WRITABLE_READY" != "true" ]]; then
  fail "writable overlay: server did not become ready within 15s"
  info "log: $(cat "$WRITABLE_SERVER_LOG" 2>/dev/null)"
else
  WKC=(--kubeconfig "$WRITABLE_KUBECONFIG" --request-timeout=10s)

  # create (namespace + configmap)
  if kubectl "${WKC[@]}" create namespace e2e-writable >/dev/null 2>&1 &&
     kubectl "${WKC[@]}" create configmap smoke-cm --from-literal=k=v1 -n e2e-writable >/dev/null 2>&1; then
    pass "writable overlay: create namespace + configmap"
  else
    fail "writable overlay: create namespace + configmap"
  fi

  # patch
  kubectl "${WKC[@]}" patch configmap smoke-cm -n e2e-writable \
    --type=merge -p '{"data":{"k":"v2"}}' >/dev/null 2>&1 || true
  patched_val=$(kubectl "${WKC[@]}" get configmap smoke-cm -n e2e-writable \
    -o jsonpath='{.data.k}' 2>/dev/null || echo "")
  assert_equals "writable overlay: patch updates configmap data" "$patched_val" "v2"

  # apply (create-via-apply a new object)
  kubectl "${WKC[@]}" apply -n e2e-writable -f - >/dev/null 2>&1 <<'YAML' || true
apiVersion: v1
kind: ConfigMap
metadata: { name: smoke-cm-applied }
data: { via: apply }
YAML
  applied_val=$(kubectl "${WKC[@]}" get configmap smoke-cm-applied -n e2e-writable \
    -o jsonpath='{.data.via}' 2>/dev/null || echo "")
  assert_equals "writable overlay: apply creates a new object" "$applied_val" "apply"

  # scale (against the captured nginx Deployment)
  kubectl "${WKC[@]}" scale deployment/nginx -n k8shark-test --replicas=3 >/dev/null 2>&1 || true
  scaled_replicas=$(kubectl "${WKC[@]}" get deployment nginx -n k8shark-test \
    -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "")
  assert_equals "writable overlay: scale updates spec.replicas" "$scaled_replicas" "3"

  # delete
  kubectl "${WKC[@]}" delete configmap smoke-cm -n e2e-writable >/dev/null 2>&1 || true
  # A broken server/kubeconfig would also make `get` fail, which a plain
  # non-zero-exit check can't tell apart from a real delete. Check the raw
  # HTTP status instead of kubectl's human-readable error text (which isn't
  # guaranteed to contain any particular substring — a tombstoned overlay
  # object 404s with "... was deleted in the writable overlay", not "NotFound").
  writable_addr=$(kubectl config --kubeconfig "$WRITABLE_KUBECONFIG" view \
    --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || echo "")
  delete_code=$(curl -sk -o /dev/null -w '%{http_code}' \
    "${writable_addr}/api/v1/namespaces/e2e-writable/configmaps/smoke-cm") || true
  assert_equals "writable overlay: delete removes the object (404 on re-GET)" "$delete_code" "404"
fi

if [[ -n "$WRITABLE_SERVER_PID" ]]; then
  kill "$WRITABLE_SERVER_PID" 2>/dev/null || true
  wait "$WRITABLE_SERVER_PID" 2>/dev/null || true
fi
rm -f "$WRITABLE_KUBECONFIG" "$WRITABLE_SERVER_LOG"

# ── Phase 9: Round-trip comparison (live cluster vs. mock server) ─────────────
log "Round-trip comparison: live cluster vs. mock server"

MOCK_POD_NAMES=$(sorted_names      "$E2E_KUBECONFIG" get pods         -n k8shark-test)
MOCK_DEPLOY_NAMES=$(sorted_names   "$E2E_KUBECONFIG" get deployments  -n k8shark-test)
MOCK_DS_NAMES=$(sorted_names       "$E2E_KUBECONFIG" get daemonsets   -n k8shark-test)
MOCK_STS_NAMES=$(sorted_names      "$E2E_KUBECONFIG" get statefulsets -n k8shark-test)
MOCK_JOB_NAMES=$(sorted_names      "$E2E_KUBECONFIG" get jobs         -n k8shark-jobs)
MOCK_NODE_NAMES=$(sorted_names     "$E2E_KUBECONFIG" get nodes)
MOCK_NGINX_REPLICAS=$(kubectl --kubeconfig "$E2E_KUBECONFIG" --request-timeout=10s \
  get deployment nginx -n k8shark-test \
  -o jsonpath='{.spec.replicas}' 2>/dev/null)
MOCK_NGINX_IMAGE=$(kubectl --kubeconfig "$E2E_KUBECONFIG" --request-timeout=10s \
  get deployment nginx -n k8shark-test \
  -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)

assert_equals "round-trip: pod names in k8shark-test"         "$MOCK_POD_NAMES"     "$LIVE_POD_NAMES"
assert_equals "round-trip: deployment names in k8shark-test"  "$MOCK_DEPLOY_NAMES"  "$LIVE_DEPLOY_NAMES"
assert_equals "round-trip: daemonset names in k8shark-test"   "$MOCK_DS_NAMES"      "$LIVE_DS_NAMES"
assert_equals "round-trip: statefulset names in k8shark-test" "$MOCK_STS_NAMES"     "$LIVE_STS_NAMES"
assert_equals "round-trip: job names in k8shark-jobs"         "$MOCK_JOB_NAMES"     "$LIVE_JOB_NAMES"
assert_equals "round-trip: node names"                        "$MOCK_NODE_NAMES"    "$LIVE_NODE_NAMES"
assert_equals "round-trip: deployment/nginx spec.replicas"    "$MOCK_NGINX_REPLICAS" "$LIVE_NGINX_REPLICAS"
assert_equals "round-trip: deployment/nginx container image"  "$MOCK_NGINX_IMAGE"   "$LIVE_NGINX_IMAGE"

# ── Phase 9b: all=true auto-discovery capture ──────────────────────────────────
log "Testing 'resources: - all: true' auto-discovery capture"

ALL_TRUE_CAPTURE_FILE="/tmp/k8shark-e2e-all-true-$$.tar.gz"
ALL_TRUE_CONFIG="/tmp/k8shark-e2e-all-true-$$.yaml"
ALL_TRUE_SERVER_LOG="/tmp/k8shark-all-true-server-$$.log"
ALL_TRUE_KC="/tmp/k8shark-all-true-kc-$$.yaml"
ALL_TRUE_SERVER_PID=""

cat > "$ALL_TRUE_CONFIG" <<YAML
duration: 15s
output: ${ALL_TRUE_CAPTURE_FILE}
kubeconfig: ${KIND_KUBECONFIG}
resources:
  - all: true
    interval: 5s
YAML
pass "all=true capture config written"

"$BINARY" --config "$ALL_TRUE_CONFIG" capture
if [[ ! -s "$ALL_TRUE_CAPTURE_FILE" ]]; then
  fail "all=true capture: archive missing or empty"
else
  ALL_TRUE_SIZE=$(du -h "$ALL_TRUE_CAPTURE_FILE" | cut -f1)
  pass "all=true capture: archive written ($ALL_TRUE_SIZE)"
fi

# Open the all=true archive and assert key resources are accessible.
"$BINARY" open "$ALL_TRUE_CAPTURE_FILE" --kubeconfig-out "$ALL_TRUE_KC" >"$ALL_TRUE_SERVER_LOG" 2>&1 &
ALL_TRUE_SERVER_PID=$!

for i in $(seq 1 30); do
  if [[ -s "$ALL_TRUE_KC" ]]; then break; fi
  sleep 0.5
done

if [[ ! -s "$ALL_TRUE_KC" ]]; then
  fail "all=true capture: mock server did not start within 15s"
else
  pass "all=true capture: mock server started"

  for i in $(seq 1 20); do
    if kubectl --kubeconfig "$ALL_TRUE_KC" --request-timeout=2s \
        get namespaces </dev/null &>/dev/null 2>&1; then
      break
    fi
    sleep 0.5
  done

  AT_KC=(--kubeconfig "$ALL_TRUE_KC" --request-timeout=10s)

  # Core namespaced resources.
  out=$(kubectl "${AT_KC[@]}" get pods -n k8shark-test -o name 2>&1) || true
  assert_not_empty "all=true: pods present in k8shark-test" "$out"

  out=$(kubectl "${AT_KC[@]}" get deployments -n k8shark-test -o name 2>&1) || true
  assert_contains "all=true: deployment/nginx present" "$out" "nginx"

  out=$(kubectl "${AT_KC[@]}" get configmaps -n k8shark-test -o name 2>&1) || true
  assert_contains "all=true: configmap/app-config present" "$out" "app-config"

  out=$(kubectl "${AT_KC[@]}" get services -n k8shark-test -o name 2>&1) || true
  assert_not_empty "all=true: services present in k8shark-test" "$out"

  # Core cluster-scoped resources.
  out=$(kubectl "${AT_KC[@]}" get nodes -o name 2>&1) || true
  assert_not_empty "all=true: nodes present (cluster-scoped)" "$out"

  out=$(kubectl "${AT_KC[@]}" get namespaces -o name 2>&1) || true
  assert_contains "all=true: namespace/k8shark-test present" "$out" "k8shark-test"
fi

if [[ -n "$ALL_TRUE_SERVER_PID" ]]; then
  kill "$ALL_TRUE_SERVER_PID" 2>/dev/null || true
  wait "$ALL_TRUE_SERVER_PID" 2>/dev/null || true
fi
rm -f "$ALL_TRUE_CAPTURE_FILE" "$ALL_TRUE_CONFIG" "$ALL_TRUE_SERVER_LOG" "$ALL_TRUE_KC"

# ── Phase 9c: encrypt -> decrypt -> open -> kubectl (passphrase + recipient) ──
log "Testing capture -> encrypt -> decrypt -> open -> kubectl"

ENC_PASS_FILE=$(mktemp /tmp/k8shark-e2e-encpass-XXXXXX)
ENC_WRONG_PASS_FILE=$(mktemp /tmp/k8shark-e2e-encpass-wrong-XXXXXX)
echo "correct-horse-battery-staple-e2e" >"$ENC_PASS_FILE"
echo "definitely-the-wrong-passphrase" >"$ENC_WRONG_PASS_FILE"

# -- Passphrase mode --
ENC_FILE="/tmp/k8shark-e2e-encrypted-$$.kshrk"
"$BINARY" --config "$CAPTURE_CONFIG" capture \
  --encrypt-passphrase-file "$ENC_PASS_FILE" \
  --out "$ENC_FILE"
if [[ -s "$ENC_FILE" ]]; then
  pass "capture --encrypt-passphrase-file: encrypted archive created"
else
  fail "capture --encrypt-passphrase-file: archive missing or empty"
fi

# inspect with no key must reject the encrypted archive outright.
out=$("$BINARY" inspect "$ENC_FILE" 2>&1) || true
assert_contains "inspect (no passphrase): rejects encrypted archive" "$out" "provide --decrypt-passphrase-file"

# inspect with the wrong passphrase must fail with the documented message.
out=$("$BINARY" inspect "$ENC_FILE" --decrypt-passphrase-file "$ENC_WRONG_PASS_FILE" 2>&1) || true
assert_contains "inspect (wrong passphrase): fails with documented message" "$out" "incorrect passphrase or key"

# inspect with the correct passphrase succeeds.
out=$("$BINARY" inspect "$ENC_FILE" --decrypt-passphrase-file "$ENC_PASS_FILE" 2>&1) || true
assert_contains "inspect (correct passphrase): succeeds" "$out" "Capture ID:"

# kshrk decrypt to a standalone plaintext archive.
DEC_FILE="/tmp/k8shark-e2e-decrypted-$$.kshrk"
"$BINARY" decrypt "$ENC_FILE" --decrypt-passphrase-file "$ENC_PASS_FILE" --out "$DEC_FILE"
if [[ -s "$DEC_FILE" ]]; then
  pass "kshrk decrypt: plaintext archive created"
else
  fail "kshrk decrypt: output archive missing or empty"
fi

# Open the decrypted archive (no key needed) and confirm kubectl works.
ENC_DEC_SERVER_LOG="/tmp/k8shark-enc-dec-server-$$.log"
ENC_DEC_KC="/tmp/k8shark-enc-dec-kc-$$.yaml"
ENC_DEC_SERVER_PID=""
"$BINARY" open "$DEC_FILE" --kubeconfig-out "$ENC_DEC_KC" >"$ENC_DEC_SERVER_LOG" 2>&1 &
ENC_DEC_SERVER_PID=$!
for i in $(seq 1 30); do
  if [[ -s "$ENC_DEC_KC" ]]; then break; fi
  sleep 0.5
done
if [[ ! -s "$ENC_DEC_KC" ]]; then
  fail "open (decrypted archive): mock server did not start within 15s"
else
  ENC_DEC_READY=false
  for i in $(seq 1 20); do
    if kubectl --kubeconfig "$ENC_DEC_KC" --request-timeout=2s \
        get namespaces </dev/null &>/dev/null 2>&1; then
      ENC_DEC_READY=true
      break
    fi
    sleep 0.5
  done
  if [[ "$ENC_DEC_READY" != "true" ]]; then
    fail "open (decrypted archive): mock server did not become ready within 10s"
  else
    out=$(kubectl --kubeconfig "$ENC_DEC_KC" --request-timeout=10s get pods -n k8shark-test -o name 2>&1) || true
    assert_contains "open (decrypted archive): pods present" "$out" "^pod/"
  fi
fi
if [[ -n "$ENC_DEC_SERVER_PID" ]]; then
  kill "$ENC_DEC_SERVER_PID" 2>/dev/null || true
  wait "$ENC_DEC_SERVER_PID" 2>/dev/null || true
  ENC_DEC_SERVER_PID=""
fi

# Open the STILL-encrypted archive directly, decrypting on the fly — no
# separate decrypt step.
ENC_OPEN_SERVER_LOG="/tmp/k8shark-enc-open-server-$$.log"
ENC_OPEN_KC="/tmp/k8shark-enc-open-kc-$$.yaml"
ENC_OPEN_SERVER_PID=""
"$BINARY" open "$ENC_FILE" --decrypt-passphrase-file "$ENC_PASS_FILE" \
  --kubeconfig-out "$ENC_OPEN_KC" >"$ENC_OPEN_SERVER_LOG" 2>&1 &
ENC_OPEN_SERVER_PID=$!
for i in $(seq 1 30); do
  if [[ -s "$ENC_OPEN_KC" ]]; then break; fi
  sleep 0.5
done
if [[ ! -s "$ENC_OPEN_KC" ]]; then
  fail "open --decrypt-passphrase-file (still-encrypted archive): mock server did not start within 15s"
else
  ENC_OPEN_READY=false
  for i in $(seq 1 20); do
    if kubectl --kubeconfig "$ENC_OPEN_KC" --request-timeout=2s \
        get namespaces </dev/null &>/dev/null 2>&1; then
      ENC_OPEN_READY=true
      break
    fi
    sleep 0.5
  done
  if [[ "$ENC_OPEN_READY" != "true" ]]; then
    fail "open --decrypt-passphrase-file (still-encrypted archive): mock server did not become ready within 10s"
  else
    out=$(kubectl --kubeconfig "$ENC_OPEN_KC" --request-timeout=10s get pods -n k8shark-test -o name 2>&1) || true
    assert_contains "open --decrypt-passphrase-file: pods present (decrypted on the fly)" "$out" "^pod/"
  fi
fi
if [[ -n "$ENC_OPEN_SERVER_PID" ]]; then
  kill "$ENC_OPEN_SERVER_PID" 2>/dev/null || true
  wait "$ENC_OPEN_SERVER_PID" 2>/dev/null || true
  ENC_OPEN_SERVER_PID=""
fi

rm -f "$ENC_FILE" "$DEC_FILE" "$ENC_PASS_FILE" "$ENC_WRONG_PASS_FILE" \
  "$ENC_DEC_SERVER_LOG" "$ENC_DEC_KC" "$ENC_OPEN_SERVER_LOG" "$ENC_OPEN_KC"

# -- Recipient-key mode --
# Generate a throwaway age X25519 keypair via filippo.io/age, which is
# already a module dependency of this codebase (see go.mod/go.sum) and
# downloadable through the Go toolchain, rather than depending on the
# separate age-keygen binary being present in every environment this script
# runs in. Requires the go toolchain, unlike the rest of this script — check
# explicitly so a missing 'go' fails clearly here instead of aborting the
# whole script under set -e with a bare "command not found".
if ! command -v go >/dev/null 2>&1; then
  fail "encrypt-recipient: 'go' not found in PATH, cannot generate a test age keypair"
else
  # go run requires a .go suffix, which mktemp's template can't preserve
  # portably (see the passphrase-file fix above) — so create the file
  # securely via mktemp, then atomically rename it to add the suffix.
  KEYGEN_SRC=$(mktemp /tmp/k8shark-e2e-keygen-XXXXXX)
  mv "$KEYGEN_SRC" "$KEYGEN_SRC.go"
  KEYGEN_SRC="$KEYGEN_SRC.go"
  cat >"$KEYGEN_SRC" <<'GOEOF'
package main

import (
	"fmt"

	"filippo.io/age"
)

func main() {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		panic(err)
	}
	fmt.Println(id.String())
	fmt.Println(id.Recipient().String())
}
GOEOF
  KEYPAIR=$(cd "$PROJ_ROOT" && go run "$KEYGEN_SRC") || true
  rm -f "$KEYGEN_SRC"
  AGE_IDENTITY=$(echo "$KEYPAIR" | sed -n '1p')
  AGE_RECIPIENT=$(echo "$KEYPAIR" | sed -n '2p')

  if [[ -z "$AGE_IDENTITY" || -z "$AGE_RECIPIENT" ]]; then
    fail "encrypt-recipient: failed to generate an age keypair"
  else
    pass "encrypt-recipient: generated a test age keypair"

    AGE_IDENTITY_FILE=$(mktemp /tmp/k8shark-e2e-age-identity-XXXXXX)
    echo "$AGE_IDENTITY" >"$AGE_IDENTITY_FILE"

    ENC_RECIPIENT_FILE="/tmp/k8shark-e2e-encrypted-recipient-$$.kshrk"
    "$BINARY" --config "$CAPTURE_CONFIG" capture \
      --encrypt-recipient "$AGE_RECIPIENT" \
      --out "$ENC_RECIPIENT_FILE"
    if [[ -s "$ENC_RECIPIENT_FILE" ]]; then
      pass "capture --encrypt-recipient: encrypted archive created"
    else
      fail "capture --encrypt-recipient: archive missing or empty"
    fi

    DEC_RECIPIENT_FILE="/tmp/k8shark-e2e-decrypted-recipient-$$.kshrk"
    "$BINARY" decrypt "$ENC_RECIPIENT_FILE" \
      --decrypt-identity-file "$AGE_IDENTITY_FILE" \
      --out "$DEC_RECIPIENT_FILE"
    if [[ -s "$DEC_RECIPIENT_FILE" ]]; then
      pass "kshrk decrypt --decrypt-identity-file: plaintext archive created"
    else
      fail "kshrk decrypt --decrypt-identity-file: output archive missing or empty"
    fi

    REC_SERVER_LOG="/tmp/k8shark-recipient-server-$$.log"
    REC_KC="/tmp/k8shark-recipient-kc-$$.yaml"
    REC_SERVER_PID=""
    "$BINARY" open "$DEC_RECIPIENT_FILE" --kubeconfig-out "$REC_KC" >"$REC_SERVER_LOG" 2>&1 &
    REC_SERVER_PID=$!
    for i in $(seq 1 30); do
      if [[ -s "$REC_KC" ]]; then break; fi
      sleep 0.5
    done
    if [[ ! -s "$REC_KC" ]]; then
      fail "open (recipient-decrypted archive): mock server did not start within 15s"
    else
      REC_READY=false
      for i in $(seq 1 20); do
        if kubectl --kubeconfig "$REC_KC" --request-timeout=2s \
            get namespaces </dev/null &>/dev/null 2>&1; then
          REC_READY=true
          break
        fi
        sleep 0.5
      done
      if [[ "$REC_READY" != "true" ]]; then
        fail "open (recipient-decrypted archive): mock server did not become ready within 10s"
      else
        out=$(kubectl --kubeconfig "$REC_KC" --request-timeout=10s get pods -n k8shark-test -o name 2>&1) || true
        assert_contains "open (recipient-decrypted archive): pods present" "$out" "^pod/"
      fi
    fi
    if [[ -n "$REC_SERVER_PID" ]]; then
      kill "$REC_SERVER_PID" 2>/dev/null || true
      wait "$REC_SERVER_PID" 2>/dev/null || true
      REC_SERVER_PID=""
    fi

    rm -f "$ENC_RECIPIENT_FILE" "$DEC_RECIPIENT_FILE" "$AGE_IDENTITY_FILE" "$REC_SERVER_LOG" "$REC_KC"
  fi
fi

# ── Phase 10: Summary ───────────────────────────────────────────────────────────
log "Test summary"
printf '  Passed: \033[1;32m%d\033[0m\n' "$PASS"
printf '  Failed: \033[1;31m%d\033[0m\n' "$FAIL"
if [[ "$FAIL" -gt 0 ]]; then
  printf '\n\033[1;31mE2E tests FAILED (%d failure(s))\033[0m\n' "$FAIL" >&2
  exit 1
fi
printf '\n\033[1;32mAll %d E2E assertions passed!\033[0m\n' "$PASS"
