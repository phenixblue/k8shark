# Mock-server conformance

k8shark's mock API server (`internal/server`, started by `kshrk open`) only has
value if standard tooling — `kubectl`, `client-go`, `k9s` — can talk to a
capture *as if* it were a live cluster. That holds only while the server stays
faithful to the real Kubernetes API contract: discovery documents, OpenAPI,
list/get envelopes, health, status codes, and error shapes.

The `conformance` workflow measures that faithfulness and catches regressions,
per [#136](https://github.com/phenixblue/k8shark/issues/136).

## How it works

`scripts/conformance.sh` runs a **differential comparison**:

1. Create a pinned KinD cluster and deploy a spread of resources (core / apps /
   batch).
2. Capture it and start the mock replay server (`kshrk open`).
3. `scripts/conformance_diff.py` reaches both the live apiserver and the mock
   through `kubectl proxy` (uniform plain-HTTP access with real status codes and
   error bodies) and diffs the in-scope endpoints, normalizing volatile fields
   (`resourceVersion`, `creationTimestamp`, `uid`, `managedFields`, `status`, …).

In-scope surface: discovery (`/api`, `/apis`, `/api/v1`, `/apis/<g>/<v>`,
per-resource `kind`/`namespaced`/`shortNames`/`verbs`/subresources), `/version`,
OpenAPI v2/v3, resource LIST/GET envelopes and item structure, health
(`/healthz`, `/readyz`, `/livez`), and error shapes (not-found, unknown
group/version).

Run it locally (needs `kind`, `kubectl`, `jq`, `python3`, `curl`):

```sh
make build
./scripts/conformance.sh            # tears down on exit
KEEP=1 ./scripts/conformance.sh     # leave cluster + mock running to poke at
```

## Why not the CNCF conformance suite?

The upstream CNCF suite (Sonobuoy / hydrophone / `e2e.test`) is a **non-goal**.
Its runners deploy a test pod *inside* the target cluster, and every
`[Conformance]` spec's shared `framework.BeforeEach` **creates a namespace**
before asserting anything. A read-only replay rejects that write by design, so
the suite fails in setup on all ~446 conformance specs regardless of read
fidelity — it measures the wrong thing here. The differential comparison above
is the meaningful signal. (This was verified empirically; see #136.)

## Baseline / accepted divergences

The check fails only on divergences **not** listed in
`scripts/conformance-baseline.json`, so it gates on *new* drift rather than
pre-existing gaps. Regenerate the baseline after an intentional change with:

```sh
WRITE_BASELINE=1 ./scripts/conformance.sh
```

Currently accepted divergences (all in `internal/server/handler.go`):

| Key | What | Why it's accepted (for now) |
|-----|------|------------------------------|
| `errors::404 not-found Status object` | The 404 body omits `reason: "NotFound"` and `details`. | Cosmetic-ish; `code: 404` is present. Candidate fix. |
| `errors::404 for unknown group/version` | Unknown `/apis/<g>/<v>` returns `200` + empty `APIResourceList` instead of `404`. | The router synthesizes a list for any group path. Candidate fix. |
| `version::/version keys` | `/version` omits newer keys (`emulationMajor` etc.) and hard-codes `major`/`minor`. | `/version` is a stub; `gitVersion` is correct. Version-dependent. |

Removing an entry here (by fixing the underlying behavior) tightens the gate.
