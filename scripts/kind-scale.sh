#!/usr/bin/env bash
# Spin up a multi-node KinD cluster populated at *scale*: 50+ namespaces and
# dozens of workloads, so a k8shark capture exercises the namespace list,
# search, and Timeline under realistic load. A handful of the workloads are
# deliberately unhealthy (crashloop / unschedulable) so the capture still has
# interesting events to show off — see kind-chaos.sh for an event-focused run.
#
# Usage:
#   ./scripts/kind-scale.sh                 # 50 namespaces, ~2 workloads each
#   ./scripts/kind-scale.sh --namespaces 80 # pick the namespace count
#   ./scripts/kind-scale.sh --reset         # delete existing scale cluster first
#
# Uses its own cluster (k8shark-scale) so it won't collide with kind-chaos.sh.
# Tear down with:  make kind-down  (removes both dev clusters)
set -euo pipefail

CLUSTER_NAME="k8shark-scale"
KIND_KUBECONFIG="${HOME}/.kube/k8shark-scale.yaml"
NS_COUNT=50
RESET=false
DRY_RUN=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --namespaces) NS_COUNT="${2:?--namespaces needs a count}"; shift 2 ;;
    --reset) RESET=true; shift ;;
    --dry-run) DRY_RUN=true; shift ;;   # generate manifests only; no cluster, no apply
    -h|--help) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
done
[[ "$NS_COUNT" -ge 1 ]] 2>/dev/null || { printf '--namespaces must be a positive integer\n' >&2; exit 2; }

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

# ── Generate manifests ───────────────────────────────────────────────────────
# Build the whole fleet as two manifests (namespaces first, then everything
# else) and apply each in a single kubectl call — far faster than per-object
# applies, and it avoids namespace-creation races. Generation needs no cluster,
# so --dry-run can produce and inspect the manifests on their own.
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
NS_FILE="$TMP/namespaces.yaml"
WL_FILE="$TMP/workloads.yaml"

log "Generating $NS_COUNT namespaces and their workloads"

# Resource block shared by every pod template — keep pods tiny so they pack in.
RES='resources: { requests: { cpu: 5m, memory: 8Mi }, limits: { memory: 48Mi } }'

emit_deploy() {  # ns name app image replicas [command-json]
  local ns="$1" name="$2" app="$3" image="$4" replicas="$5" cmd="${6:-}"
  {
    printf -- '---\napiVersion: apps/v1\nkind: Deployment\n'
    printf 'metadata: { name: %s, namespace: %s, labels: { app: %s, k8shark.io/suite: scale } }\n' "$name" "$ns" "$app"
    printf 'spec:\n  replicas: %s\n  selector: { matchLabels: { app: %s } }\n' "$replicas" "$app"
    printf '  template:\n    metadata: { labels: { app: %s, k8shark.io/suite: scale } }\n    spec:\n      containers:\n' "$app"
    printf '        - name: main\n          image: %s\n          %s\n' "$image" "$RES"
    if [[ -n "$cmd" ]]; then printf '          command: %s\n' "$cmd"; fi
  } >>"$WL_FILE"
}

for i in $(seq 1 "$NS_COUNT"); do
  ns=$(printf 'scale-%03d' "$i")

  # Namespace (+ a tier label so there's something to group/filter on).
  tier=$(( i % 3 ))
  {
    printf -- '---\napiVersion: v1\nkind: Namespace\n'
    printf 'metadata: { name: %s, labels: { k8shark.io/suite: scale, k8shark.io/tier: "t%s" } }\n' "$ns" "$tier"
  } >>"$NS_FILE"

  # ConfigMap (a non-pod object in every namespace).
  {
    printf -- '---\napiVersion: v1\nkind: ConfigMap\n'
    printf 'metadata: { name: app-config, namespace: %s }\ndata: { tier: "t%s", index: "%s" }\n' "$ns" "$tier" "$i"
  } >>"$WL_FILE"

  # Every namespace gets a web Deployment + Service.
  emit_deploy "$ns" web nginx nginx:alpine 1
  {
    printf -- '---\napiVersion: v1\nkind: Service\n'
    printf 'metadata: { name: web, namespace: %s }\n' "$ns"
    printf 'spec: { selector: { app: nginx }, ports: [{ port: 80, targetPort: 80 }] }\n'
  } >>"$WL_FILE"

  # Rotate a second workload so the fleet is varied; sprinkle in eventful ones.
  case $(( i % 6 )) in
    0) emit_deploy "$ns" cache  redis  redis:7-alpine        1 ;;
    1) emit_deploy "$ns" worker pause  registry.k8s.io/pause:3.9 3 ;;
    2) emit_deploy "$ns" api    nginx  nginx:alpine          2 ;;
    3) # crashloop — eventful
       emit_deploy "$ns" crashloop crashloop busybox:1.36 1 '["/bin/sh","-c","echo up; sleep 4; exit 1"]' ;;
    4) # unschedulable — stays Pending, no node resource used
       {
         printf -- '---\napiVersion: apps/v1\nkind: Deployment\n'
         printf 'metadata: { name: greedy, namespace: %s, labels: { app: greedy, k8shark.io/suite: scale } }\n' "$ns"
         printf 'spec:\n  replicas: 1\n  selector: { matchLabels: { app: greedy } }\n'
         printf '  template:\n    metadata: { labels: { app: greedy, k8shark.io/suite: scale } }\n    spec:\n      containers:\n'
         printf '        - name: main\n          image: nginx:alpine\n          resources: { requests: { cpu: "64" } }\n'
       } >>"$WL_FILE" ;;
    5) # short-lived Job
       {
         printf -- '---\napiVersion: batch/v1\nkind: Job\n'
         printf 'metadata: { name: oneshot, namespace: %s, labels: { app: oneshot, k8shark.io/suite: scale } }\n' "$ns"
         printf 'spec:\n  template:\n    metadata: { labels: { app: oneshot, k8shark.io/suite: scale } }\n    spec:\n      restartPolicy: Never\n      containers:\n'
         printf '        - name: main\n          image: busybox:1.36\n          %s\n' "$RES"
         printf '          command: ["/bin/sh","-c","echo working; sleep 8; echo done"]\n'
       } >>"$WL_FILE" ;;
  esac
done

ns_objs=$(grep -c '^kind: Namespace' "$NS_FILE" || true)
wl_objs=$(grep -cE '^kind: (Deployment|Service|ConfigMap|Job)' "$WL_FILE" || true)
ok "Generated $ns_objs namespaces and $wl_objs workload objects"

if $DRY_RUN; then
  out="./scale-manifests"
  mkdir -p "$out"
  cp "$NS_FILE" "$WL_FILE" "$out/"
  ok "Dry run — wrote manifests to $out/ (no cluster created, nothing applied)"
  exit 0
fi

# ── Optional reset ─────────────────────────────────────────────────────────────
if $RESET && kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  log "Deleting existing cluster '$CLUSTER_NAME'"
  kind delete cluster --name "$CLUSTER_NAME"
fi

# ── Create multi-node KinD cluster ───────────────────────────────────────────
# 1 control-plane + 2 workers, with maxPods raised so a few hundred tiny pods
# fit comfortably (default is 110/node).
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  log "Cluster '$CLUSTER_NAME' already exists — skipping creation"
  kind get kubeconfig --name "$CLUSTER_NAME" > "$KIND_KUBECONFIG"
  chmod 0600 "$KIND_KUBECONFIG"
else
  log "Creating multi-node KinD cluster '$CLUSTER_NAME' (1 control-plane + 2 workers)"
  kind create cluster \
    --name "$CLUSTER_NAME" \
    --kubeconfig "$KIND_KUBECONFIG" \
    --wait 120s \
    --config - <<'YAML'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
kubeadmConfigPatches:
  - |
    kind: KubeletConfiguration
    maxPods: 250
nodes:
  - role: control-plane
  - role: worker
  - role: worker
YAML
  ok "Cluster '$CLUSTER_NAME' ready"
fi

KC=(--kubeconfig "$KIND_KUBECONFIG")

# ── Apply ────────────────────────────────────────────────────────────────────
log "Creating namespaces"
kubectl "${KC[@]}" apply -f "$NS_FILE" >/dev/null
ok "Namespaces applied"

log "Deploying workloads (single bulk apply)"
kubectl "${KC[@]}" apply -f "$WL_FILE" >/dev/null
ok "Workloads applied"

# ── Settle ───────────────────────────────────────────────────────────────────
log "Waiting ~60s for pods to schedule and events to flow"
sleep 60
printf '\n'
info "Pods by phase:"
kubectl "${KC[@]}" get pods -A -l k8shark.io/suite=scale \
  --no-headers 2>/dev/null | awk '{print $4}' | sort | uniq -c | sort -rn | sed 's/^/      /'
total_pods=$(kubectl "${KC[@]}" get pods -A -l k8shark.io/suite=scale --no-headers 2>/dev/null | wc -l | tr -d ' ')
total_ns=$(kubectl "${KC[@]}" get ns -l k8shark.io/suite=scale --no-headers 2>/dev/null | wc -l | tr -d ' ')

# ── Done ───────────────────────────────────────────────────────────────────────
printf '\n\033[1;32mScale cluster ready: %s namespaces, %s pods.\033[0m\n\n' "$total_ns" "$total_pods"
info "Inspect it:"
printf '    export KUBECONFIG=%s\n' "$KIND_KUBECONFIG"
printf '    kubectl get ns -l k8shark.io/suite=scale\n'
printf '    kubectl get pods -A -l k8shark.io/suite=scale -o wide\n\n'
info "Capture it with k8shark:"
printf '    make build\n'
printf '    ./kshrk capture --kubeconfig %s --auto-discover --duration 5m -o scale.kshrk\n\n' "$KIND_KUBECONFIG"
info "Then explore the capture (UI on 18080/18081):"
printf '    ./kshrk ui scale.kshrk --port 18080 --api-port 18081\n\n'
info "Tear down when finished:"
printf '    make kind-down\n\n'
