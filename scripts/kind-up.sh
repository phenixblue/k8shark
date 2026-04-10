#!/usr/bin/env bash
# Spin up a KinD cluster with k8shark test resources for manual exploration.
#
# Usage:
#   ./scripts/kind-up.sh          # create cluster + deploy resources
#   ./scripts/kind-up.sh --reset  # delete existing cluster first, then recreate
#
# Tear down with:  make kind-down  (or: kind delete cluster --name k8shark-dev)
set -euo pipefail

CLUSTER_NAME="k8shark-dev"
KIND_KUBECONFIG="${HOME}/.kube/k8shark-dev.yaml"
RESET=false
for arg in "$@"; do [[ "$arg" == "--reset" ]] && RESET=true; done

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
  # Refresh the kubeconfig in case it was lost.
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

# ── Deploy test resources ──────────────────────────────────────────────────────
log "Deploying test resources"

# Namespaces (idempotent)
for ns in k8shark-test k8shark-jobs; do
  kubectl "${KC[@]}" get namespace "$ns" &>/dev/null 2>&1 || \
    kubectl "${KC[@]}" create namespace "$ns"
done
ok "Namespaces: k8shark-test, k8shark-jobs"

# ConfigMap + Secret
kubectl "${KC[@]}" apply -n k8shark-test -f - <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  env: production
  log-level: info
YAML
kubectl "${KC[@]}" apply -n k8shark-test -f - <<'YAML'
apiVersion: v1
kind: Secret
metadata:
  name: app-secret
stringData:
  db-password: s3cr3t
YAML
ok "ConfigMap app-config, Secret app-secret"

# Deployments
kubectl "${KC[@]}" apply -n k8shark-test -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nginx
  labels:
    app: nginx
spec:
  replicas: 2
  selector:
    matchLabels:
      app: nginx
  template:
    metadata:
      labels:
        app: nginx
    spec:
      containers:
        - name: nginx
          image: nginx:alpine
YAML
kubectl "${KC[@]}" apply -n k8shark-test -f - <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: redis
  labels:
    app: redis
spec:
  replicas: 1
  selector:
    matchLabels:
      app: redis
  template:
    metadata:
      labels:
        app: redis
    spec:
      containers:
        - name: redis
          image: redis:7-alpine
YAML
ok "Deployments: nginx (x2), redis (x1)"

# Service
kubectl "${KC[@]}" apply -n k8shark-test -f - <<'YAML'
apiVersion: v1
kind: Service
metadata:
  name: nginx
spec:
  selector:
    app: nginx
  ports:
    - port: 80
      targetPort: 80
YAML
ok "Service: nginx"

# DaemonSet (pause image — no external registry needed)
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
ok "DaemonSet: log-collector"

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
ok "Job: data-processor"

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
ok "StatefulSet: web (x3) with PVCs"

# ── Wait for workloads ─────────────────────────────────────────────────────────
log "Waiting for workloads (timeout 120s each)"
kubectl "${KC[@]}" rollout status deployment/nginx        -n k8shark-test --timeout=120s
kubectl "${KC[@]}" rollout status deployment/redis        -n k8shark-test --timeout=120s
kubectl "${KC[@]}" rollout status daemonset/log-collector -n k8shark-test --timeout=120s
kubectl "${KC[@]}" rollout status statefulset/web         -n k8shark-test --timeout=120s
ok "All workloads ready"

# ── Done ───────────────────────────────────────────────────────────────────────
printf '\n\033[1;32mCluster ready.\033[0m\n'
info "export KUBECONFIG=${KIND_KUBECONFIG}"
info "kubectl get pods,statefulsets,pvc -n k8shark-test"
printf '\n  export KUBECONFIG=%s\n\n' "$KIND_KUBECONFIG"
printf '  kubectl get pods -A\n'
printf '  make kind-down    # when finished\n\n'
