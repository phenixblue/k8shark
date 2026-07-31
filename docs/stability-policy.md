# Compatibility, Stability, and Deprecation Policy

This page states what `kshrk` promises to keep working, for how long, and what
is explicitly exempt — so a script, CI pipeline, or muscle-memory habit built
against one `1.x` release keeps working against the next. It takes effect at
`v1.0.0`; everything below describes the `1.x` line. The one exception is the
[archive format](#archive-format), which already carries its own
version-gated stability promise today (see
[docs/archive-format.md](archive-format.md#format-version--compatibility))
and isn't affected by the CLI's own `v1.0.0` milestone. Every other surface
below is unpromised before `v1.0.0` — backward-incompatible changes there can
land in any pre-`1.0.0` release.

k8shark follows [Semantic Versioning](https://semver.org): `vMAJOR.MINOR.PATCH`.

- **MAJOR** — a breaking change to any surface listed under
  [Stable surfaces](#stable-surfaces) below.
- **MINOR** — new features, new flags, new config keys, new subcommands.
  Backward-compatible.
- **PATCH** — bug fixes only. No new flags, no *intentional* behavior
  change to a documented surface — correcting a bug in one (making it match
  what's documented, or fixing an unambiguous defect) is exactly what a
  patch release is for.

This is a policy for **behavior**, not for source code: nothing in
`internal/` is covered (see [What is NOT covered](#what-is-not-covered)),
and refactoring the package layout is always a non-breaking change as long
as the surfaces below hold.

## Stable surfaces

### Archive format

Covered in full by [docs/archive-format.md](archive-format.md#format-version--compatibility)
— the entry layout, the JSON schemas, and the reader-compatibility promise
("the `1.x` line reads all version-1 archives for the life of the `1.x`
series"). That page is the authority; this policy just points to it.

### CLI surface

Every subcommand name, flag name and spelling, and flag semantics listed in
[docs/cli-reference.md](cli-reference.md) are stable for the `1.x` line, with
the exceptions called out [below](#flags-explicitly-not-covered):

| Command | Purpose |
|---------|---------|
| `capture` | Poll a live cluster and write a `.kshrk` archive |
| `validate` | Validate a capture config file without connecting to a cluster |
| `inspect` | Summarize an archive's contents |
| `open` | Start a static (non-replaying) mock API server from an archive |
| `replay` | Start a time-advancing mock API server, optionally with KWOK/controller-manager |
| `ui` | Start the web dashboard (and, optionally, replay) |
| `diagnose` | Analyze an archive offline and report likely problems |
| `diff` | Compare two archives, or two points in time within one archive |
| `query` | JSONPath / text / regex search across an archive |
| `transitions` | List watch-event (ADDED/MODIFIED/DELETED) history |
| `redact` | Re-write an archive with Secret/field redaction applied |
| `encrypt` / `decrypt` | Re-write an archive's encryption envelope |
| `completion` | Generate shell completion scripts |
| `version` | Print the `kshrk` version |

A flag's **name and meaning** won't change or disappear within `1.x` without
going through [deprecation](#deprecation-policy) first. Flags may gain new
optional values or new sibling flags in a minor release; a required flag never
becomes optional-with-different-default (or vice versa) without a major bump.

[docs/cli-reference.md](cli-reference.md) is the single source of truth for
the current flag list — this policy intentionally doesn't duplicate it, to
avoid the two drifting out of sync. That page is **generated from the Cobra
command definitions and drift-checked in CI**, so it can't fall behind the
binary; `docs/usage.md` is a narrative guide that covers the common paths and
deliberately doesn't enumerate every flag.

#### Flags explicitly not covered

The replay-orchestration and writable-overlay flags are **exempt** — their
names and semantics may change in any release, including a patch:

`--with-kwok`, `--with-controller-manager`, `--schedule-pods`, `--writable`,
`--start-paused`, `--loop`, `--speed`

These drive the newest and least-exercised parts of the mock server (KWOK and
kube-controller-manager orchestration, the writable overlay, replay timing),
where the shape of the feature is still likely to move. Everything else in
`docs/cli-reference.md` is covered. Promoting one of these to covered is a
minor-version change (widening coverage); the reverse would not be, which is
why the exemption is being written down now rather than assumed.

### Exit codes

Every command follows the `diff(1)`-style contract documented in
[`cmd/root.go`](../cmd/root.go):

| Code | Meaning |
|------|---------|
| `0` | The command ran and didn't trip a findings/differences gate (`diagnose` without `--fail-on`, or with `--fail-on` but nothing met that severity; `diff` with no differences) — or the command has no such gate at all |
| `1` | The command ran and *did* trip a findings/differences gate (`diagnose --fail-on <severity>` matched, `diff` found differences) |
| `2` | The command failed to run (bad archive, invalid flags, I/O error, …) |

`diagnose` without `--fail-on` always exits `0` regardless of what it
found — printing findings and gating on them are separate; exit `1` is
opt-in via `--fail-on`. Only `diagnose` and `diff` currently ever exit `1`;
every other command exits `0` or `2`.

Giving another command an **opt-in** flag that can trigger exit `1` (the
`diagnose --fail-on` pattern) is a minor-version change — a script that
doesn't pass the new flag keeps seeing the command's existing exit codes.
Making an *existing* command start exiting `1` unconditionally, or
repurposing what `1` or `2` mean, is a major-version change — plenty of
scripts treat any non-zero exit as failure, so silently turning some of a
command's current successes into exit `1` would break them.

### Structured output (`-o json` / `-o yaml`)

`inspect`, `diagnose`, `diff`, `query`, and `transitions` all support `-o
json` (some also support `-o yaml`). **This output is a stable, scriptable
interface, not an implementation detail** — it follows the same evolution
rule as the archive format:

- Adding a new field is a minor-version change. Consumers must ignore
  fields they don't recognize.
- Removing a field, renaming a field, or changing a field's type/meaning is
  a major-version change.

Every one of the five emits a JSON **object** at the top level (never a bare
array) carrying its own `schema_version`, so the envelope can always gain
fields and a consumer can always tell what shape it's parsing. The versions
are per-command and independent: `diagnose`'s `schema_version` moving to 2
says nothing about `query`'s.

Collection fields are always arrays, never `null` — an empty result is `[]`,
so `jq '.findings[]'` and friends work without a guard.

#### Embedded Kubernetes objects are passthrough, not covered

`diff` and `transitions` embed whole captured Kubernetes objects under
`before` / `after`, and `query`'s `matches[].value` returns whatever the
JSONPath expression selected. **Those payloads are passthrough of what the
cluster returned; their field names and types belong to the Kubernetes API,
not to k8shark**, and they vary with the captured cluster's version. The
add-only rule above cannot sensibly apply to them and does not:

- **Covered**: the wrapper each one sits in — `changes[].{path, group,
  version, resource, namespace, before, after}` for `diff`,
  `transitions[].{time, event_type, api_path, group, version, resource,
  namespace, name, before, after}` for `transitions`, and
  `matches[].{path, group, version, resource, namespace, name, value}` for
  `query`.
- **Not covered**: anything *inside* `before`, `after`, or `value`.

`matches[].value` in particular is **polymorphic by design** — it is
whatever JSON type the selected field holds, so `{.spec.replicas}` yields a
number, `{.metadata.name}` a string, `{.metadata.labels}` an object, and
`{.spec.containers}` an array. Consumers must type-switch on it rather than
assume a type. This is not something a future release will "fix": the field
mirrors the queried document.

Table/text output (the human-facing default, no `-o` flag) is explicitly
**not** covered — column layout, spacing, and wording can change in any
release. Scripts must pass `-o json` (or `-o yaml`, where offered) to get a
stability guarantee.

### Config file schema

Covered by [docs/config.md](config.md). The schema carries its own
`version:` key (`internal/config.CurrentConfigVersion`) precisely so it can
evolve independently of the CLI's own version — see that page's
[Naming convention](config.md#naming-convention) section for the current
camelCase-vs-legacy-alias rules. A config file that validates against
schema version *N* keeps validating against every `1.x` build that
understands version *N* or higher.

### Generated kubeconfig

`kshrk open` / `kshrk replay` / `kshrk ui` write a kubeconfig usable by any
standard Kubernetes client (`kubectl`, `client-go`, `k9s`, …). What's
covered:

- **Default path**: `~/.kube/k8shark-<id>.yaml`, where `<id>` is the
  capture's ID, overridable with `--kubeconfig-out`.
- **Validity**: the file is always a well-formed kubeconfig (`apiVersion:
  v1`, `kind: Config`) pointing at the locally-running mock server, usable
  without further edits.

Internal details — the exact context/user/cluster names, the placeholder
bearer token value, and `insecure-skip-tls-verify` being set (the mock
server's self-signed cert has no meaningful trust boundary to protect) — are
**not** covered and may change at any time; don't parse or diff the file's
contents, just point a client at it.

## What is NOT covered

- **Anything under `internal/`.** Nothing there is an importable API;
  `internal/store`, `internal/server`, `internal/config`, etc. can be
  restructured, renamed, or have their exported (from Go's perspective)
  identifiers changed at any time. Only `internal/config` and
  `internal/archive/format`'s *on-disk/on-wire* output are covered, via the
  config-schema and archive-format policies above — not their Go APIs.
- **The web UI** (`kshrk ui`, `internal/ui`). Explicitly marked
  **experimental** in the [README](../README.md#web-ui-experimental); its
  HTTP API (`/v2/api/...`), DOM structure, and behavior can change in any
  release, including a patch.
- **Mock-server conformance gaps.** [docs/conformance.md](conformance.md)
  lists currently-accepted divergences from a real API server (e.g. 404
  body shape, `/version` field completeness). Closing one of those gaps —
  making the mock *more* faithful — is not a breaking change even though it
  changes response bytes, since nothing in this policy promises
  byte-for-byte fidelity to those specific divergences.
- **Human-facing table/text output** (see [Structured
  output](#structured-output--o-json---o-yaml) above).
- **Supported build/runtime environment specifics** beyond the Kubernetes
  compatibility window below — e.g. the exact Go toolchain version used to
  build releases (see [docs/development.md](development.md)) can change in
  any release.

## Deprecation policy

Deprecating part of a stable surface (a flag, a config key, a subcommand)
takes at least one full minor release cycle:

1. **Announce** — the deprecation is called out in that release's notes,
   naming the replacement.
2. **Coexist** — the deprecated spelling keeps working, unchanged in
   behavior, for at least one minor release after the announcement.
3. **Remove** — earliest possible removal is the *next* minor release after
   the one where the replacement became available (i.e. announce in `1.N`,
   remove no sooner than `1.(N+2)`), never a patch release.

This mirrors the precedent already set for the `snake_case` config-key
aliases (`auto_discover`, `ui.api_port`, …) documented in
[docs/config.md](config.md#naming-convention): accepted alongside the
canonical spelling for at least one minor release before their removal is
even considered.

## Supported Kubernetes / kubectl version window

- **`kubectl`**: any reasonably recent version. `kshrk`'s mock server speaks
  plain REST/JSON over HTTPS; it doesn't depend on a specific `kubectl`
  release.
- **Kubernetes server (for `capture`)**: tracked via `k8s.io/client-go` in
  `go.mod` (currently `v0.36.x`). k8shark captures via the generic
  REST/discovery surface rather than version-specific APIs, so support spans
  a wide range. Two distinct claims, which are easy to conflate:
  - **Continuously tested**: only the pinned minor
    (`kindest/node:v1.36.1`), on every change, via the
    [conformance workflow](../.github/workflows/conformance.yml).
  - **Verified**: **v1.30 through v1.36** as of `v1.0.0` — the full e2e
    suite (capture, redaction, encryption round-trip, replay, writable
    overlay, `kubectl` round-trip) plus the mock-vs-live conformance
    differential, run against v1.30.3 / v1.32.3 / v1.34.3 / v1.36.1. All
    four passed 113/113 e2e assertions with zero new conformance
    divergences. Reproducing the claim takes both harnesses — the e2e suite
    and the conformance differential are what the two halves of it rest on:

    ```sh
    make build
    NODE_IMAGE=kindest/node:v1.32.3 ./scripts/e2e.sh
    NODE_IMAGE=kindest/node:v1.32.3 ./scripts/conformance.sh
    ```

  Versions outside that verified range are expected to work but carry no
  evidence. v1.30 was the oldest version tested, not a discovered floor —
  it passed cleanly, so the real floor is older.
- **KWOK / kube-controller-manager** (`replay --with-kwok
  --with-controller-manager`): downloaded to match the *capture's* recorded
  Kubernetes version (see [docs/kwok.md](kwok.md)), not the version `kshrk`
  itself was built against.

## Backport / patch policy

`kshrk` maintains a single active line: **only the latest minor receives
patch releases.** There are no long-term-support branches. Upgrading to pick
up a fix means upgrading to the latest `1.x` minor.

## Changing this policy

Narrowing what's covered (removing something from [Stable
surfaces](#stable-surfaces)) is itself a breaking change and requires a
major-version bump and an entry in this document explaining what changed and
why. Widening coverage (documenting a previously-uncovered surface as
stable) can happen in a minor release.
