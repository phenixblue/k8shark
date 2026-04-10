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
  --output "$INLINE_REDACTED_FILE"

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
redact_out=$("$BINARY" redact \
  --in "$CAPTURE_FILE" \
  --out "$REDACTED_FILE" 2>&1) || true
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

# ── Phase 10: Summary ───────────────────────────────────────────────────────────
log "Test summary"
printf '  Passed: \033[1;32m%d\033[0m\n' "$PASS"
printf '  Failed: \033[1;31m%d\033[0m\n' "$FAIL"
if [[ "$FAIL" -gt 0 ]]; then
  printf '\n\033[1;31mE2E tests FAILED (%d failure(s))\033[0m\n' "$FAIL" >&2
  exit 1
fi
printf '\n\033[1;32mAll %d E2E assertions passed!\033[0m\n' "$PASS"
