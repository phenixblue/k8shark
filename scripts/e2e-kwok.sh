#!/usr/bin/env bash
# k8shark + KWOK end-to-end test.
#
# Validates the closed loop from docs/kwok.md: capture a real cluster, replay it
# with the writable overlay, point a real `kwok` at the mock server, create a
# Pod, and assert that the pod-scheduling shim binds it to a node (#160) and KWOK
# drives it to Running.
#
# Manually triggered — NOT part of `make e2e` / the auto e2e CI job — because it
# needs the `kwok` binary, which isn't on the base runner. Run via:
#     make e2e-kwok
#     ./scripts/e2e-kwok.sh
#   or the "e2e-kwok" GitHub Actions workflow (workflow_dispatch).
#
# Prerequisites: kind, kubectl, kwok, and a built kshrk binary (make build).
set -euo pipefail

# ── Configuration ──────────────────────────────────────────────────────────────
PROJ_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="k8shark-kwok-$$"
CAPTURE_FILE="/tmp/k8shark-kwok-$$.kshrk"
CAPTURE_CONFIG="/tmp/k8shark-kwok-cfg-$$.yaml"
KIND_KUBECONFIG="/tmp/k8shark-kwok-kind-$$.yaml"
MOCK_KUBECONFIG="/tmp/k8shark-kwok-mock-$$.yaml"
SERVER_LOG="/tmp/k8shark-kwok-server-$$.log"
KWOK_LOG="/tmp/k8shark-kwok-kwok-$$.log"
BINARY="${BINARY:-${PROJ_ROOT}/kshrk}"
POD_NS="default"
POD_NAME="kwok-demo"
READY_TIMEOUT="${READY_TIMEOUT:-90}" # seconds to wait for Running
PASS=0
FAIL=0

# ── Helpers ────────────────────────────────────────────────────────────────────
log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
pass() { printf '  \033[1;32m[OK]   %s\033[0m\n' "$*"; PASS=$((PASS + 1)); }
fail() { printf '  \033[1;31m[FAIL] %s\033[0m\n' "$*"; FAIL=$((FAIL + 1)); }
die()  { printf '\n\033[1;31mFATAL: %s\033[0m\n' "$*" >&2; exit 1; }
skip() { printf '\n\033[1;33mSKIP: %s\033[0m\n' "$*" >&2; exit 0; }

kmock() { kubectl --kubeconfig "$MOCK_KUBECONFIG" --request-timeout=10s "$@"; }

# ── Cleanup ────────────────────────────────────────────────────────────────────
SERVER_PID=""
KWOK_PID=""
cleanup() {
  [[ -n "$KWOK_PID" ]] && { kill "$KWOK_PID" 2>/dev/null || true; wait "$KWOK_PID" 2>/dev/null || true; }
  [[ -n "$SERVER_PID" ]] && { kill "$SERVER_PID" 2>/dev/null || true; wait "$SERVER_PID" 2>/dev/null || true; }
  info "Deleting KinD cluster '$CLUSTER_NAME'..."
  kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
  rm -f "$CAPTURE_FILE" "$CAPTURE_CONFIG" "$KIND_KUBECONFIG" "$MOCK_KUBECONFIG" "$SERVER_LOG" "$KWOK_LOG"
}
trap cleanup EXIT

# ── Phase 1: Prerequisites ─────────────────────────────────────────────────────
log "Checking prerequisites"
for tool in kind kubectl; do
  command -v "$tool" >/dev/null 2>&1 || die "'$tool' not found in PATH"
  pass "$tool found at $(command -v "$tool")"
done
[[ -x "$BINARY" ]] || die "kshrk binary not found at '$BINARY'. Run 'make build' first."
pass "kshrk binary found at $BINARY"
# kwok is the one tool the base runner lacks; skip (not fail) so a local run
# without it is a no-op rather than a red build.
if ! command -v kwok >/dev/null 2>&1; then
  skip "'kwok' not found in PATH — install from https://kwok.sigs.k8s.io/docs/user/install/ to run this test"
fi
pass "kwok found at $(command -v kwok) ($(kwok --version 2>/dev/null | head -1))"

# ── Phase 2: KinD cluster + short capture ──────────────────────────────────────
log "Creating KinD cluster '$CLUSTER_NAME'"
kind create cluster --name "$CLUSTER_NAME" --kubeconfig "$KIND_KUBECONFIG" --wait 90s
pass "KinD cluster ready"

log "Capturing the cluster (short)"
cat > "$CAPTURE_CONFIG" <<YAML
duration: 8s
output: ${CAPTURE_FILE}
kubeconfig: ${KIND_KUBECONFIG}
resources:
  - version: v1
    resource: namespaces
    interval: 4s
  - version: v1
    resource: nodes
    interval: 4s
  - version: v1
    resource: pods
    interval: 4s
YAML
"$BINARY" --config "$CAPTURE_CONFIG" capture
[[ -s "$CAPTURE_FILE" ]] || die "capture produced no archive at $CAPTURE_FILE"
pass "capture written to $CAPTURE_FILE"

# ── Phase 3: Writable replay ───────────────────────────────────────────────────
log "Starting kshrk replay --writable"
# No --loop: a loop wrap would reset the overlay and wipe the pod we create.
"$BINARY" replay "$CAPTURE_FILE" --writable --kubeconfig-out "$MOCK_KUBECONFIG" \
  >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!
for i in $(seq 1 30); do
  if [[ -s "$MOCK_KUBECONFIG" ]] && kmock get nodes >/dev/null 2>&1; then break; fi
  kill -0 "$SERVER_PID" 2>/dev/null || { cat "$SERVER_LOG"; die "replay server exited early"; }
  sleep 1
done
kmock get nodes >/dev/null 2>&1 || { cat "$SERVER_LOG"; die "replay server not ready after 30s"; }
pass "writable replay server ready (kubeconfig $MOCK_KUBECONFIG)"

# ── Phase 4: KWOK against the mock server ──────────────────────────────────────
log "Starting kwok against the mock server"
# --manage-all-nodes so KWOK also manages nodes that came from the capture (not
# just its own kwok.x-k8s.io/node: fake ones), and thus the pods bound to them.
kwok --kubeconfig "$MOCK_KUBECONFIG" --manage-all-nodes >"$KWOK_LOG" 2>&1 &
KWOK_PID=$!
sleep 2
kill -0 "$KWOK_PID" 2>/dev/null || { cat "$KWOK_LOG"; die "kwok exited early"; }
pass "kwok running (pid $KWOK_PID)"

# ── Phase 5: Create a pod and watch it reach Running ───────────────────────────
log "Creating pod $POD_NS/$POD_NAME and waiting for Running"
kmock run "$POD_NAME" --image=nginx --restart=Never -n "$POD_NS" \
  || { cat "$SERVER_LOG"; die "failed to create pod"; }

# The scheduling shim should bind the pod to a node immediately (before KWOK).
NODE_NAME=$(kmock get pod "$POD_NAME" -n "$POD_NS" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
if [[ -n "$NODE_NAME" ]]; then
  pass "scheduling shim bound pod to node '$NODE_NAME'"
else
  fail "pod was not assigned a nodeName by the scheduling shim"
fi

PHASE=""
for i in $(seq 1 "$READY_TIMEOUT"); do
  PHASE=$(kmock get pod "$POD_NAME" -n "$POD_NS" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  [[ "$PHASE" == "Running" ]] && break
  sleep 1
done
if [[ "$PHASE" == "Running" ]]; then
  pass "pod reached Running via KWOK (closed loop verified)"
else
  info "kwok log tail:"; tail -20 "$KWOK_LOG" 2>/dev/null || true
  fail "pod did not reach Running within ${READY_TIMEOUT}s (last phase: '${PHASE:-<none>}')"
fi

# ── Summary ────────────────────────────────────────────────────────────────────
log "Summary"
info "Passed: $PASS   Failed: $FAIL"
[[ "$FAIL" -eq 0 ]] || die "$FAIL check(s) failed"
printf '\n\033[1;32mAll k8shark + KWOK e2e checks passed.\033[0m\n'
