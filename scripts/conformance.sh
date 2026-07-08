#!/usr/bin/env bash
# k8shark mock-server conformance harness (issue #136, phase 1).
#
# Stands up a KinD cluster, captures it, opens the mock replay server, then
# runs a *differential* comparison of the mock's responses against the live
# apiserver for the read/discovery surface the mock serves (discovery,
# version, OpenAPI, resource LIST/GET, health, error shapes). Emits a
# compatibility summary plus a categorized list of divergences.
#
# Unlike scripts/e2e.sh (which spot-checks a handful of kubectl assertions),
# this systematically diffs mock<->upstream and yields a compatibility score.
# It exits non-zero only on divergences NOT already recorded in
# scripts/conformance-baseline.json, so CI gates on *new* drift. See
# docs/conformance.md for the accepted-divergence rationale.
#
# Usage:   make build && ./scripts/conformance.sh
# Env:     KEEP=1     leave the cluster and mock server running on exit
#          NODE_IMAGE=kindest/node:v1.32.0   pin a Kubernetes version
#          WRITE_BASELINE=1   rewrite the baseline from the current run
#
# Prerequisites: kind, kubectl, jq, python3 (must be in PATH)
set -euo pipefail

PROJ_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="k8shark-conf-$$"
CAPTURE_FILE="/tmp/k8shark-conf-$$.kshrk"
CAPTURE_CONFIG="/tmp/k8shark-conf-$$.yaml"
KIND_KUBECONFIG="/tmp/k8shark-conf-kind-$$.yaml"
SERVER_LOG="/tmp/k8shark-conf-server-$$.log"
BINARY="${BINARY:-${PROJ_ROOT}/kshrk}"
NODE_IMAGE="${NODE_IMAGE:-}"
SERVER_PID=""
# Pin the generated mock kubeconfig under /tmp (rather than the default
# ~/.kube/k8shark-<id>) so cleanup can remove it like the other temp files.
MOCK_KUBECONFIG="/tmp/k8shark-conf-mock-$$.yaml"

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die()  { printf '\n\033[1;31mFATAL: %s\033[0m\n' "$*" >&2; exit 1; }

cleanup() {
  if [[ "${KEEP:-}" == "1" ]]; then
    info "KEEP=1 — leaving cluster '$CLUSTER_NAME' and mock (pid $SERVER_PID) running"
    info "  live kubeconfig: $KIND_KUBECONFIG"
    info "  mock kubeconfig: ${MOCK_KUBECONFIG:-<not started>}"
    return
  fi
  if [[ -n "$SERVER_PID" ]]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
  rm -f "$CAPTURE_FILE" "$CAPTURE_CONFIG" "$KIND_KUBECONFIG" "$SERVER_LOG" "$MOCK_KUBECONFIG"
}
trap cleanup EXIT

# ── Prerequisites ─────────────────────────────────────────────────────────────
log "Checking prerequisites"
for tool in kind kubectl jq python3; do
  command -v "$tool" >/dev/null 2>&1 || die "'$tool' not found in PATH"
done
[[ -x "$BINARY" ]] || die "Binary not found at '$BINARY'. Run 'make build' first."
info "all tools present; binary at $BINARY"

# ── KinD cluster ──────────────────────────────────────────────────────────────
log "Creating KinD cluster '$CLUSTER_NAME'${NODE_IMAGE:+ (image $NODE_IMAGE)}"
if [[ -n "$NODE_IMAGE" ]]; then
  kind create cluster --name "$CLUSTER_NAME" --kubeconfig "$KIND_KUBECONFIG" --image "$NODE_IMAGE" --wait 90s
else
  kind create cluster --name "$CLUSTER_NAME" --kubeconfig "$KIND_KUBECONFIG" --wait 90s
fi
KC=(--kubeconfig "$KIND_KUBECONFIG")
K8S_VERSION=$(kubectl "${KC[@]}" version -o json | jq -r '.serverVersion.gitVersion')
info "cluster ready ($K8S_VERSION)"

# ── Deploy a spread of resources across core / apps / batch groups ────────────
log "Deploying test resources"
kubectl "${KC[@]}" create namespace conf-test >/dev/null
kubectl "${KC[@]}" create configmap app-config --from-literal=env=prod -n conf-test >/dev/null
kubectl "${KC[@]}" create secret generic app-secret --from-literal=note=placeholder-not-a-secret -n conf-test >/dev/null
kubectl "${KC[@]}" create deployment nginx --image=nginx:alpine --replicas=2 -n conf-test >/dev/null
kubectl "${KC[@]}" expose deployment nginx --port=80 -n conf-test >/dev/null
kubectl "${KC[@]}" apply -n conf-test -f - >/dev/null <<'YAML'
apiVersion: batch/v1
kind: Job
metadata: { name: hello }
spec:
  template:
    spec:
      restartPolicy: Never
      containers: [{ name: hello, image: busybox:1.36, command: ["true"] }]
YAML
kubectl "${KC[@]}" rollout status deployment/nginx -n conf-test --timeout=120s >/dev/null
info "resources ready in namespace conf-test"

# ── Capture ───────────────────────────────────────────────────────────────────
log "Writing capture config + running capture"
cat > "$CAPTURE_CONFIG" <<YAML
duration: 15s
output: ${CAPTURE_FILE}
kubeconfig: ${KIND_KUBECONFIG}
resources:
  - { version: v1, resource: namespaces, interval: 5s }
  - { version: v1, resource: nodes, interval: 5s }
  - { version: v1, resource: pods, namespaces: [conf-test], interval: 5s }
  - { version: v1, resource: services, namespaces: [conf-test], interval: 5s }
  - { version: v1, resource: configmaps, namespaces: [conf-test], interval: 5s }
  - { version: v1, resource: secrets, namespaces: [conf-test], interval: 5s }
  - { group: apps, version: v1, resource: deployments, namespaces: [conf-test], interval: 5s }
  - { group: apps, version: v1, resource: replicasets, namespaces: [conf-test], interval: 5s }
  - { group: batch, version: v1, resource: jobs, namespaces: [conf-test], interval: 5s }
YAML
"$BINARY" --config "$CAPTURE_CONFIG" capture
[[ -s "$CAPTURE_FILE" ]] || die "capture archive missing/empty"
info "capture written: $(du -h "$CAPTURE_FILE" | cut -f1)"

# ── Open the mock replay server ───────────────────────────────────────────────
log "Opening mock replay server"
# Write the kubeconfig to our /tmp path (so cleanup can remove it) instead of
# the default ~/.kube/k8shark-<id>.
"$BINARY" open "$CAPTURE_FILE" --kubeconfig-out "$MOCK_KUBECONFIG" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
# The mock is ready once its kubeconfig has been written and it answers a
# request. Gate on those directly rather than parsing the startup log, so a
# not-yet-written line can't trip `set -e`.
for _ in $(seq 1 60); do
  if [[ -s "$MOCK_KUBECONFIG" ]] &&
     kubectl --kubeconfig "$MOCK_KUBECONFIG" --request-timeout=2s get namespaces &>/dev/null; then
    break
  fi
  sleep 0.5
done
[[ -s "$MOCK_KUBECONFIG" ]] || { cat "$SERVER_LOG"; die "mock server did not start"; }
MOCK_ADDR=$(grep "Address:" "$SERVER_LOG" | awk '{print $2}') || true
info "mock server up at ${MOCK_ADDR:-?} (kubeconfig $MOCK_KUBECONFIG)"

# ── Differential comparison ───────────────────────────────────────────────────
log "Running differential comparison (mock vs live apiserver)"
LIVE_KUBECONFIG="$KIND_KUBECONFIG" \
MOCK_KUBECONFIG="$MOCK_KUBECONFIG" \
PROBE_NS="conf-test" \
K8S_VERSION="$K8S_VERSION" \
WRITE_BASELINE="${WRITE_BASELINE:-}" \
CONFORMANCE_MD="${CONFORMANCE_MD:-}" \
CONFORMANCE_BASELINE="${CONFORMANCE_BASELINE:-${PROJ_ROOT}/scripts/conformance-baseline.json}" \
  python3 "${PROJ_ROOT}/scripts/conformance_diff.py"
