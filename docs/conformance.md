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
(`/healthz`, `/readyz`, `/livez`), error shapes (not-found, unknown
group/version), and `?fieldSelector=` queries (see below).

Run it locally (needs `kind`, `kubectl`, `jq`, `python3`):

```sh
make build
./scripts/conformance.sh            # tears down on exit
KEEP=1 ./scripts/conformance.sh     # leave cluster + mock running to poke at
```

Set `CONFORMANCE_MD=/path/report.md` to also write a Markdown summary (the CI
workflow uses this for the job summary and posts it as a sticky PR comment that
is updated in place on each run).

## Field selectors, and keeping their tables in sync

`?fieldSelector=` is the one part of the read surface where fidelity depends on
data the mock cannot derive from the capture. kubectl contributes nothing here —
cli-runtime forwards the raw string and holds no per-kind table — so everything a
client expects is owed by the server. The apiserver evaluates a selector in two
independent layers:

| Layer | Upstream source | What it decides |
|-------|-----------------|-----------------|
| Validation + aliasing | `AddFieldLabelConversionFunc` in `pkg/apis/<group>/<version>/conversion.go` | Which labels are accepted at all; unaccepted → **400**. Rewrites aliases (`spec.host` → `spec.nodeName`). |
| Matching | `ToSelectableFields` in `pkg/registry/.../strategy.go` | Which labels resolve to a value; a label absent here reads as `""`. |

The two lists genuinely differ, so `internal/store/fieldselector.go` keeps them
separate. Pods accept `status.podIPs` but never select on it, so upstream accepts
`status.podIPs=10.0.0.1` and then matches nothing. Collapsing the layers would
either reject a key upstream accepts or match on one it ignores.

Those tables are hand-maintained: upstream registers them in
`k8s.io/kubernetes`'s *internal* API packages, which are not importable from
`k8s.io/api` or `client-go`, so there is nothing to reflect over at build time.
Three mechanisms keep them honest, each covering the others' blind spots:

1. **The differential** (section G of `conformance_diff.py`) — probes real
   `?fieldSelector=` queries against both servers and compares status code *and*
   matched set. The live apiserver *is* the table, so an upstream change shows up
   as a new divergence. Blind spot: it only exercises kinds and keys the capture
   actually contains.
2. **`scripts/fieldselector_drift.py`** — parses the upstream source at the
   Kubernetes minor pinned in `go.mod` and diffs it against
   `scripts/fieldselector-snapshot.json`. Covers the kinds the capture lacks (a
   KinD capture has no CertificateSigningRequest). Runs weekly and on PRs that
   touch the tables (`.github/workflows/fieldselector-drift.yml`); needs network.
3. **`TestFieldSelectorTables_*`** in `internal/store` — asserts the Go tables
   match that snapshot, offline, on every `make test`. Without it the snapshot
   could track upstream perfectly while the Go code drifted from both.

When (2) fails, upstream changed something. Read the diff, update
`internal/store/fieldselector.go`, then refresh the snapshot:

```sh
make fieldselector-drift-update
```

## Why not the CNCF conformance suite?

The upstream CNCF suite (Sonobuoy / hydrophone / `e2e.test`) is a **non-goal**
for this workflow. Its runners deploy a test pod *inside* the target cluster,
and every `[Conformance]` spec's shared `framework.BeforeEach` **creates a
namespace** before asserting anything. A read-only replay rejects that write
by design, so the suite fails in setup on all ~446 conformance specs
regardless of read fidelity — it measures the wrong thing here. The
differential comparison above is the meaningful signal. (This was verified
empirically; see #136.)

Pairing a writable replay with `--with-kwok --with-controller-manager` (see
[docs/kwok.md](kwok.md#closing-more-of-the-loop---with-controller-manager))
gets past that specific setup blocker and clears a small slice of the suite —
the pure CRUD/discovery/pod-static-lifecycle specs that don't need a real
kubelet — but the bulk of the suite still needs a real container runtime and
isn't a goal here either; treat it as an exploratory signal, not a gate.

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
| `errors::404 for unknown group/version` | Unknown `/apis/<g>/<v>` returns `200` + empty `APIResourceList` instead of `404`. | The router synthesizes a list for any group path. Candidate fix. |
| `version::/version keys` | `/version` omits newer keys (`emulationMajor` etc.) and hard-codes `major`/`minor`. | `/version` is a stub; `gitVersion` is correct. Version-dependent. |

The not-found Status object divergence (404 body missing `reason: "NotFound"`
and `details`) was fixed in #177 for a real, captured resource type —
`notFoundStatus` in `internal/server/handler.go` sets both — and no longer
appears in the baseline.

`Accept: …;as=PartialObjectMetadata;g=meta.k8s.io` was a divergence the
differential never covered, because nothing in the read surface it compares
requests a metadata-only projection — the mock returned the full object, which a
client decoding strictly against `PartialObjectMetadata` rejects outright. That
surfaced only when `replay --with-controller-manager` was run end to end and
kube-controller-manager's garbagecollector, which walks `ownerReferences` with a
metadata client, failed to sync any item and retried forever (#329). It is now
projected in `internal/store/responses_codec.go`; a `Status` body is deliberately
passed through unprojected so a 404 stays decodable as `Status`.

Removing an entry here (by fixing the underlying behavior) tightens the gate.
