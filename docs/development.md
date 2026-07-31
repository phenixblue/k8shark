# Development

## Prerequisites

| Tool | Minimum version | Notes |
|------|----------------|-------|
| Go | 1.26 | Check `go.mod` for the exact minimum |
| `kind` | any recent | Required for `make e2e` and `make kind-up` |
| `kubectl` | any recent | Required for E2E and manual testing |
| `goreleaser` | v2 | Required for `make release-snapshot` / `make release-local` only |

## Building

```sh
make build          # compiles ./kshrk
make build VERSION=v0.2.0-rc.1  # embed a specific version string
```

The binary embeds the version via `-ldflags -X github.com/phenixblue/k8shark/cmd.Version=...`. The default when building with `make` is `dev`.

## Testing

```sh
make test           # go test ./...
make test-race      # go test -race ./...
make test-cover     # generates coverage.html
```

All tests are in `internal/`. There are no external dependencies required; the capture and server tests use in-process fakes.

## Formatting and linting

```sh
make fmt            # gofmt -w . (run after any Go changes)
make lint           # go vet ./...
```

The CI `contract` job enforces that `gofmt -w . && git diff --exit-code` is clean. Always run `make fmt` before committing Go changes.

For golangci-lint (run in CI):

```sh
go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8
golangci-lint run
```

Enabled linters are defined in [`.golangci.yml`](../.golangci.yml): `errcheck`, `govet`, `ineffassign`, `staticcheck`, `unused`.

## KinD dev cluster

`make kind-up` creates a local KinD cluster named `k8shark-dev` and deploys a variety of test resources (namespaces, pods, deployments, configmaps, services, PVCs, jobs) for manual capture testing.

```sh
make kind-up                # create cluster + deploy test resources
make kind-up ARGS=--reset   # delete existing cluster first, then recreate

export KUBECONFIG=~/.kube/k8shark-dev.yaml

# Run a capture against it
kshrk capture --config k8shark.yaml

make kind-down              # delete the cluster
```

The kubeconfig is written to `~/.kube/k8shark-dev.yaml`.

## End-to-end tests

```sh
make e2e
```

This runs `scripts/e2e.sh`, which:

1. Creates a temporary KinD cluster
2. Deploys test resources (pods, deployments, services, configmaps, nodes, etc.)
3. Runs `kshrk capture` with a short duration config
4. Runs `kshrk open` in the background
5. Asserts that `kubectl get pods`, `kubectl get nodes`, and other commands return expected data from the capture
6. Cleans up the cluster on exit (even on failure)

Prerequisites: `kind`, `kubectl`, and `jq` must be in your `PATH`, and the binary must already be built (`make build`). The script checks all of these in its first phase and exits immediately if one is missing.

### Testing against a specific Kubernetes version

`NODE_IMAGE` pins the KinD node image, so the whole suite can be pointed at any
Kubernetes minor (`scripts/conformance.sh` takes the same variable):

```sh
NODE_IMAGE=kindest/node:v1.32.3 ./scripts/e2e.sh
NODE_IMAGE=kindest/node:v1.32.3 ./scripts/conformance.sh
```

Each run logs the server `gitVersion` it actually reached and, when
`NODE_IMAGE` is set, **fails** if that version can't be determined or doesn't
match the requested tag — so a typo'd or unavailable tag can't quietly fall
back to kind's default and make a version matrix look broader than it was.
This is how the v1.30–v1.36 range in the
[version window](stability-policy.md#supported-kubernetes--kubectl-version-window)
was established; `make e2e` with no `NODE_IMAGE` uses kind's default.

## Verifying backward compatibility against a real old release

[docs/archive-format.md](archive-format.md#format-version--compatibility)
promises the `1.x` line reads every version-1 archive for the life of the
series, and [docs/stability-policy.md](stability-policy.md#config-file-schema)
makes a similar promise for config files. The golden fixtures in
`internal/archive/testdata/` pin the *format*; this is how to check the promise
against what an old release actually wrote:

```sh
# 1. Get the old binary.
gh release download v0.5.1 --repo phenixblue/k8shark -p 'k8shark_*_darwin_arm64.tar.gz'
mkdir -p /tmp/old && tar xzf k8shark_0.5.1_darwin_arm64.tar.gz -C /tmp/old

# 2. Capture with it against a live cluster.
make kind-up
KUBECONFIG=~/.kube/k8shark-dev.yaml /tmp/old/kshrk capture --config old.yaml

# 3. Read it with the current build — every command, plus the mock server.
make build
./kshrk inspect old.kshrk -o json
./kshrk diagnose old.kshrk -o json
./kshrk transitions old.kshrk -o json
./kshrk open old.kshrk --api-port 18081 --kubeconfig-out /tmp/mock.yaml &
kubectl --kubeconfig /tmp/mock.yaml get pods -A

# 4. And run the old release's shipped config through the current validator.
git show v0.5.1:examples/k8shark.yaml > /tmp/v051.yaml
./kshrk validate --config /tmp/v051.yaml
```

Use a high API port (`18081`) rather than the default so a test run can't
collide with a real one.

Worth knowing: a v0.5.1 capture of a *single* resource still lands at ~1 MB
because discovery and OpenAPI paths are captured alongside it, so a real old
archive is too heavy to check in as a fixture. `TestReadMetadata_RealV051Shape`
in `internal/archive/format_test.go` instead encodes the metadata key set and
value shapes observed from a real v0.5.1 capture, which is the part the golden
fixture leaves uncovered.

## Make targets reference

Run `make help` to print all targets:

```
  build           Build the kshrk binary
  test            Run unit tests
  test-race       Run unit tests with the race detector
  test-cover      Run unit tests and generate an HTML coverage report
  fmt             Format Go source files
  lint            Run go vet
  e2e             Build binary and run end-to-end tests (requires kind + kubectl)
  kind-up         Create a dev KinD cluster with test resources (use --reset to recreate)
  kind-down       Delete the dev KinD cluster (k8shark-dev)
  release-snapshot  Build a local release snapshot without publishing (DOES sign: needs OIDC)
  release-local   Dry-run release with SBOM + Homebrew output but no publish
  clean           Remove build artifacts
  help            Print available targets
```

## Package structure

```
.
├── main.go                    # entry point — calls cmd.Execute()
├── cmd/                       # cobra CLI commands
│   ├── root.go                # root command + config discovery
│   ├── capture.go             # kshrk capture
│   ├── open.go                # kshrk open
│   ├── ui.go                  # kshrk ui
│   ├── inspect.go             # kshrk inspect
│   ├── diff.go                # kshrk diff
│   ├── redact.go              # kshrk redact
│   ├── transitions.go         # kshrk transitions
│   ├── validate.go            # kshrk validate
│   └── version.go             # kshrk version
└── internal/
    ├── archive/               # .kshrk (ZIP+Zstd) read/write
    ├── capture/               # capture engine + record types
    │   ├── engine.go          # polling loop, doFetch, buildAPIPath
    │   └── record.go          # Record, Index, CaptureMetadata types
    ├── config/                # config file loading + validation
    ├── server/                # mock API server
    │   ├── server.go          # TLS server lifecycle, archive loading
    │   ├── handler.go         # HTTP routing + serveResource
    │   ├── store.go           # CaptureStore, Latest, Aggregate*, parseAPIPath
    │   ├── selector.go        # label + field selector filtering
    │   ├── tls.go             # self-signed cert generation
    │   └── kubeconfig.go      # kubeconfig writer
    ├── ui/                    # web UI server (hosts the v2 dashboard)
    │   └── v2/                # dashboard handlers + embedded static assets
    ├── inspect/               # archive summary (kshrk inspect)
    ├── diff/                  # before/after archive diff
    ├── redact/                # secret + field redaction
    └── transitions/           # watch-event state-change timeline
```
