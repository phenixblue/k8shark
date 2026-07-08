# Copilot instructions for k8shark

Guidance for GitHub Copilot when reviewing pull requests and assisting in this
repository. (For the human/CLI-agent conventions, see `CLAUDE.md` and
`.github/instructions/`.)

## Code-review approach

- **Be comprehensive on the first review.** Review the entire changed surface in
  a single pass and report all substantive findings together, grouped by file
  or theme. Don't trickle findings across many rounds — surface everything you
  can in the initial review.
- **Scope re-reviews to what changed.** On subsequent pushes, review only the
  new diff since your previous review. Do not re-raise points that were already
  addressed or resolved, and do not repeat findings on unchanged lines.
- **Prioritize substance over nits.** Focus on correctness bugs, security
  issues, data loss, resource leaks, race conditions, incorrect error handling,
  and public API / contract mistakes. Skip purely stylistic remarks.
- **Group repeated findings.** If the same issue pattern appears in several
  places, raise it once with the list of locations rather than one comment per
  occurrence.
- **Don't flag what tooling already enforces.** Formatting is handled by
  `gofmt`; linting by `golangci-lint` and `go vet`; vulnerabilities by
  `govulncheck`. Don't raise issues these tools already cover in CI.
- **Respect established patterns.** If a construct is used consistently across
  the codebase or documented in `CLAUDE.md`, don't flag a single instance in
  isolation.

## Repository conventions

- **American English** everywhere — docs, comments, identifiers, log/error
  strings, and commit messages (`color`, `behavior`, `canceled`, `normalize`,
  `recognize`, etc.), not the British forms.
- Build and verify through the **Makefile** (`make build`, `make test`,
  `make lint`, `make fmt`) rather than raw `go` commands.
- CI tools (`golangci-lint`, `govulncheck`) are pinned to explicit versions,
  not `@latest`; the `go` directive in `go.mod` is pinned to a specific patch
  release.

## Known false positives — do not flag

- `capture.Index` map literals that elide `&IndexEntry{...}` (e.g.
  `capture.Index{"/api/...": {APIPath: "...", Seqs: []int{0}}}`) are valid Go:
  the spec allows eliding `&T` for composite-literal values whose element type
  is `*T`. These compile and are correct.
