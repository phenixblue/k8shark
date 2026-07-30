# RBAC: what a capture needs

`kshrk capture` is **read-only** — it never creates, updates, or deletes
anything in the source cluster. Every request it makes is a `GET` (list a
resource, fetch a pod log) or a `WATCH` (for resources configured with
`watch: true`). This page states exactly what to grant so a customer (or
your own security review) can approve a capture without reading source.

## Minimal grant: an explicit resource list

If your capture config lists specific resources (the common case — see
[docs/config.md](config.md)), grant exactly those, nothing more. For example,
a config capturing `pods`, `pods/log`, `deployments`, and `services`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: k8shark-capture
rules:
  - apiGroups: [""]
    resources: ["pods", "pods/log", "services"]
    verbs: ["get", "list", "watch"]
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: k8shark-capture
subjects:
  - kind: ServiceAccount
    name: k8shark-capture
    namespace: default # wherever the capturing identity lives
roleRef:
  kind: ClusterRole
  name: k8shark-capture
  apiGroup: rbac.authorization.k8s.io
```

A `ClusterRole` + `ClusterRoleBinding` is simplest even when every resource in
the config is scoped to specific `namespaces:` — it avoids managing one
`Role`/`RoleBinding` pair per namespace. If cluster-wide access to these
resource *types* is a concern regardless of namespace scoping, use a `Role` +
`RoleBinding` per target namespace instead; the verbs and resources are the
same either way.

`watch: true` needs no additional verb beyond `watch` itself — already
included above.

## Broader grant: `all: true` / `autoDiscover: true`

Full-cluster capture (`resources: [{all: true}]` or the legacy
`autoDiscover: true`, see [docs/config.md](config.md)) walks every API group
and resource type the cluster exposes, so it needs read access to
effectively everything:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: k8shark-capture-all
rules:
  - apiGroups: ["*"]
    resources: ["*"]
    verbs: ["get", "list", "watch"]
  # Discovery/OpenAPI endpoints (/api, /apis, /openapi/*, /version) are
  # non-resource URLs — only expressible in a ClusterRole, never a Role.
  # Most clusters already grant these to every authenticated user via the
  # built-in system:discovery ClusterRole; include this rule explicitly if
  # your cluster has that default binding removed.
  - nonResourceURLs: ["/api", "/api/*", "/apis", "/apis/*", "/openapi/*", "/version"]
    verbs: ["get"]
```

This is the access level to scrutinize hardest in a security review: it's
read-only, but it's read-only *everything*, including Secrets (capture
records Secret `data`/`stringData` unless you also pass `--redact-secrets` or
configure [redaction rules](config.md#redaction)). If the concern is
specifically Secrets rather than the read-only breadth itself, redacting at
capture time is the better answer than narrowing RBAC — a `ClusterRole` that
excludes `secrets` still lets discovery enumerate them (list/watch would just
403), which produces confusing partial results (see below) rather than a
clean exclusion.

## What partial RBAC failures look like

A capture doesn't abort just because one resource type is forbidden.
Per-resource fetches record whatever the API server actually returned —
including a `403` — and the capture continues with everything else. On
replay, `kubectl get <that-resource>` against the mock server returns the
same `403` it would have during the real capture; `kshrk inspect` lists the
resource with its captured record(s), which will show the error response
rather than real objects.

Two things fail the *entire* capture outright, rather than degrading
gracefully, because the engine cannot proceed at all without them:

- **The initial preflight check** (`GET /version`) — if the provided
  kubeconfig/context can't authenticate at all, capture exits immediately
  with a clear "capture preflight failed" error naming the kubeconfig and
  server.
- **Namespace discovery** — only when a resource uses a wildcard namespace
  (`namespaces: ['*']`) or `all: true`/`autoDiscover: true` needs to expand
  namespaces itself; a `403` listing namespaces fails with "namespace
  discovery failed: check cluster permissions" rather than silently
  capturing zero namespaces.

In short: check `kshrk inspect <archive>` after a capture to confirm every
resource you expected actually has real data, not a captured `403` — a
successful (exit 0) `kshrk capture` run does not by itself guarantee every
requested resource was actually readable.
