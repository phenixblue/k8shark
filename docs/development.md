# Development

## Prerequisites

| Tool | Minimum version | Notes |
|------|----------------|-------|
| Go | 1.22 | Check `go.mod` for the exact minimum |
| `kind` | any recent | Required for `make e2e` and `make kind-up` |
| `kubectl` | any recent | Required for E2E and manual testing |
| `goreleaser` | v2 | Required for `make release-snapshot` / `make release-local` only |

## Building

```sh
make build          # compiles ./kshrk
make build VERSION=v0.1.2-dev  # embed a specific version string
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
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
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

Prerequisites: `kind` and `kubectl` must be in your `PATH`, and the binary must already be built (`make build`).

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
  release-snapshot  Build a local release snapshot without publishing (no signing)
  release-local   Dry-run release with SBOM + Homebrew output but no publish
  clean           Remove build artifacts
  help            Print available targets
```

## Package structure

```
.
├── main.go                    # entry point — calls cmd.Execute()
├── cmd/                       # cobra CLI commands
│   ├── root.go                # kshrk root command + config init
│   ├── capture.go             # kshrk capture
│   ├── open.go                # kshrk open
│   └── version.go             # kshrk version
└── internal/
    ├── archive/               # tar.gz read/write
    ├── capture/               # capture engine + record types
    │   ├── engine.go          # polling loop, doFetch, buildAPIPath
    │   └── record.go          # Record, Index, CaptureMetadata types
    ├── config/                # config file loading + validation
    └── server/                # mock API server
        ├── server.go          # TLS server lifecycle, archive extraction
        ├── handler.go         # HTTP routing + serveResource
        ├── store.go           # CaptureStore, Latest, Aggregate*, parseAPIPath
        ├── selector.go        # label + field selector filtering
        ├── tls.go             # self-signed cert generation
        └── kubeconfig.go      # kubeconfig writer
```
