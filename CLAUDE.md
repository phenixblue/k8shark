# CLAUDE.md

Guidance for Claude Code when working in this repository. For full dev setup,
the KinD cluster, and the Make target reference, see `docs/development.md`.

## Conventions

- **American English** everywhere — docs, code comments, identifiers, log/error
  strings, and commit messages. Use `color`, `behavior`, `summarize`,
  `canceled`, `recognize`, `serialization`, etc. (not the `-our`/`-ise`/`-lled`
  British forms). The repo was normalized to American English in #105.

## Building & verifying

Use the Makefile rather than raw `go` commands:

- `make build` — build the `kshrk` binary
- `make test` / `make test-race` — unit tests (race detector)
- `make fmt` — `gofmt -w .` (**run before every push**)
- `make lint` — `go vet ./...`
- `make lint-ci-install` + `make lint-ci` — run golangci-lint exactly as CI does

**The most common cause of red CI is formatting/tidy drift.** The `contract` job
runs `gofmt -w . && git diff --exit-code` and `go mod tidy` (with a diff check),
so always run `make fmt` and keep `go.mod`/`go.sum` tidy before pushing.

## CI tool versions

Tools installed via `go install` in CI (golangci-lint, govulncheck) are pinned to
explicit versions, **not `@latest`**, for reproducible runs. Dependabot's
`github-actions` ecosystem only bumps `uses:` action refs, so these pins need
manual bumps. Pin any new CI tool the same way.

Every `uses:` line in `.github/workflows/` is pinned to a full 40-char commit
SHA with a trailing `# vX.Y.Z` comment (not a floating major tag like `@v4`) —
`release.yml` holds `contents: write`, `id-token: write`, `attestations: write`,
and the `HOMEBREW_TAP_GITHUB_TOKEN` secret, so a compromised tag on any action
it uses would get all of it, including the Sigstore signing identity (#229).
Dependabot's `github-actions` ecosystem (already configured in
`.github/dependabot.yml`) opens PRs to bump these SHAs and keeps the version
comment in sync — no manual maintenance needed once pinned. Pin any new action
the same way when adding one.

## Archive lifecycle (gotcha)

`archive.Open` returns a `*zip.ReadCloser` that holds a real OS file handle, and
`store.LoadStore` needs the archive to stay open for the store's lifetime.
Therefore:

- **`internal/server`** (the mock API server) is the only long-lived holder
  that owns an archive. It MUST close it on `Shutdown`/`Wait` — otherwise the
  file descriptor leaks for the life of the process (this leak was fixed in
  #91). `internal/ui` does **not** open its own archive: it reuses the mock
  server's `CaptureStore` via `MockServer.Store()`, so its `Shutdown`/`Wait`
  only stop its own HTTP server and must NOT close the archive (double-close).
  `kshrk replay --ui` and `kshrk ui` therefore always start a mock server
  first and tear the UI down *before* it (see cmd/ui.go, cmd/replay.go) — the
  UI reads through the same store, so shutting the mock server down first
  would leave the UI serving from an archive it doesn't own.
- **`LoadStore` starts a background discovery-enrichment goroutine** that
  keeps reading from the archive after `LoadStore` returns (the store is
  documented as usable before it completes). Every holder of a `CaptureStore`
  MUST call `store.Close()` (which waits for that goroutine) before closing
  the underlying archive, or a fast command can close the zip out from under
  it (#232). `defer store.Close()` immediately after a successful
  `LoadStore`, ordered so it fires before `defer ar.Close()`.
- **`internal/server`'s watch endpoints hold the archive open indefinitely**
  (they block on the request context until the client disconnects or
  `?timeoutSeconds` fires). Its `Server.Shutdown`/`Wait` cancel a shared
  `BaseContext` before calling `httpServer.Shutdown` (so those handlers wake
  up and return promptly) and then wait on a request-tracking `WaitGroup` as a
  hard guarantee that none is still reading before the archive is closed
  (#230). Don't reintroduce a bare `httpServer.Shutdown()` + immediate
  `ar.Close()` without both of those.
- **One-shot CLIs** (`inspect`, `redact`, `transitions`, `diff`) use
  `defer ar.Close()` immediately after opening; the ones that go through
  `server.LoadStore` (`query`, `diagnose`, `diff`) also need `defer
  store.Close()` per above.

## Orientation

- **Web UI:** only the v2 dashboard exists (`internal/ui/v2`, mounted at `/v2/`,
  with `/` redirecting there). The legacy v1 UI was removed in #91 — don't
  reference `/v1/` or `/api/ui/*`.
- **Archive format version:** see `CheckFormatVersion` in
  `internal/archive/format/format.go` — the .kshrk schema types and version
  check live in this stdlib-only leaf package so `internal/archive` can
  reference them without an import cycle back to `internal/capture` (#233).
  `internal/capture`'s `Record`/`CaptureMetadata`/`Index`/etc. are type
  aliases to it, so existing `capture.X` call sites are unaffected.
  `archive.Open`/`OpenWithIdentities` enforce the version check centrally,
  so callers no longer need their own `CheckFormatVersion` call after
  `ReadMetadata`. See also the "Format version & compatibility" section of
  `docs/archive-format.md`. Semantics: `0` = pre-versioning (treated as v1),
  negative = corrupt (rejected), greater than `CurrentFormatVersion` = rejected
  with an "upgrade kshrk" error. Bump `CurrentFormatVersion` only on a breaking,
  structurally-incompatible change.
- **`internal/store`** holds `CaptureStore`/`LoadStore` (archive-backed
  reads, selectors, response content negotiation) — extracted from
  `internal/server` (#234) so `diagnose`/`query`/`diff` read an archive
  without importing the HTTP mock apiserver package. `internal/server`
  imports it back (aliased `kstore`, since many of its functions/methods take
  or hold a local/field variable literally named `store`) for the mock
  apiserver's own reads. `internal/ui/v2`'s `Handler.Overlay` is typed as an
  `OverlayReader` interface (defined in `v2`, satisfied by `*server.Server`)
  rather than the concrete server type — but unlike `*server.Server`'s
  nil-safe methods, a nil `OverlayReader` interface value panics on any method
  call, so every `h.Overlay.OverlayScopes()`/`MergeOverlayList` call site
  still nil-checks `h.Overlay` first.

## Code review notes

- **Copilot's "map literal won't compile" warnings are false positives.** Copilot
  repeatedly flags `capture.Index` map literals such as

  ```go
  capture.Index{
      "/api/v1/...": {APIPath: "/api/v1/...", Seqs: []int{0}},
  }
  ```

  claiming they need `&capture.IndexEntry{...}` because `Index` is
  `map[string]*IndexEntry` (see `internal/archive/format/format.go`, aliased
  as `capture.Index`/`capture.IndexEntry` in `internal/capture/record.go`).
  This is valid Go:
  the spec lets you elide the `&T` for map/array/slice values whose element type
  is a pointer to a composite literal ("elements or keys that are addresses of
  composite literals may elide the `&T` when the element or key type is `*T`").
  CI compiles and runs these tests. Dismiss this comment rather than "fixing" it.
