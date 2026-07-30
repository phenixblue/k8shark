# Contributing

Thanks for considering a contribution to k8shark. This covers the essentials
to get a change through CI on the first try; see
[docs/development.md](docs/development.md) for the full dev setup (building,
the KinD dev cluster, E2E tests, package layout).

## Before you start

For anything beyond a small, obvious fix, please open an issue first to
discuss the approach — it saves rework on both sides. Check
[existing issues](https://github.com/phenixblue/k8shark/issues) for
duplicates.

## Building, testing, linting

```sh
make build           # compiles ./kshrk
make test            # go test ./...
make test-race        # go test -race ./...
make fmt              # gofmt -w . — run before every push
make lint             # go vet ./...
make lint-ci-install   # one-time: install golangci-lint at the pinned version
make lint-ci           # golangci-lint run, exactly as CI runs it
```

## The format/tidy contract

CI's `contract` job runs `gofmt -w . && git diff --exit-code` and
`go mod tidy` with a diff check. **This is the most common cause of red CI.**
Always run `make fmt` and make sure `go.mod`/`go.sum` are tidy before
pushing:

```sh
make fmt
go mod tidy
git diff --exit-code go.mod go.sum
```

## Conventions

- **American English** everywhere — docs, code comments, identifiers,
  log/error strings, and commit messages (`color`, `behavior`, `canceled`,
  not `colour`, `behaviour`, `cancelled`).
- Prefer editing existing files and following the patterns already in the
  surrounding code over introducing a new style.
- Comments explain *why*, not *what* — skip a comment if the code already
  says what it does.
- Keep pull requests scoped to one logical change; unrelated cleanup makes
  review harder and the diff riskier to land.

## CI tool versions

Tools installed via `go install` in CI (`golangci-lint`, `govulncheck`) and
every GitHub Action referenced in `.github/workflows/` are pinned to an
explicit version (a commit SHA for Actions), never `@latest` or a floating
major tag — see [CLAUDE.md](CLAUDE.md#ci-tool-versions) for why. Pin any new
CI tool or Action the same way.

## Commit messages and pull requests

- Write commit messages that explain *why*, not just *what changed* — the
  diff already shows what changed.
- Squash-worthy fixup commits are fine locally; keep the final PR history
  reasonably clean.
- Make sure `go test ./...` and `make lint-ci` pass locally before opening a
  PR — CI will run both, but catching it locally is faster.
- Reference the issue a PR closes with `Closes #123` in the description so it
  auto-closes on merge.

## Reporting bugs and requesting features

Use the issue templates when opening a new issue — they prompt for the
information that's actually needed to act on a report (version, repro steps,
expected vs. actual behavior).

**Security vulnerabilities**: see [SECURITY.md](SECURITY.md) instead of
opening a public issue.

## License

By contributing, you agree that your contributions will be licensed under
the project's [Apache License 2.0](LICENSE).
