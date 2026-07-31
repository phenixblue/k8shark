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

## Release-artifact smoke test

`make e2e`, `make test`, and the CI matrix all build from source. Nothing among
them executes a **published release artifact**, so a packaging or
cross-compilation defect present only in the shipped binary would go unnoticed
until a user hit it.

[`scripts/artifact-smoke.sh`](../scripts/artifact-smoke.sh) closes that: point it
at an extracted binary and it checks the binary runs, reports the expected
version, reads a real `.kshrk` archive, honors the exit-code contract, and emits
the frozen `-o json` shapes.

```sh
# Against a published release, by hand:
gh release download v1.0.0 --repo phenixblue/k8shark -p 'k8shark_*_linux_amd64.tar.gz'
mkdir -p /tmp/ks && tar xzf k8shark_1.0.0_linux_amd64.tar.gz -C /tmp/ks
./scripts/artifact-smoke.sh /tmp/ks/kshrk 1.0.0 .
```

Set `EXPECT_CONTRACT=0` to skip the `-o json` shape assertions when testing a
release older than `v1.0.0` (the shapes were settled in #320, after
`v1.0.0-rc.3`).

The script is POSIX `sh`, not bash, so the same file runs on Alpine's busybox
`ash` and in Windows Git Bash. Testing a linux binary locally on macOS:

```sh
docker run --rm --platform linux/arm64 \
  -v "$PWD/kshrk:/kshrk:ro" -v "$PWD:/work:ro" alpine:3 \
  /bin/sh /work/scripts/artifact-smoke.sh /kshrk 1.0.0 /work
```

Running it under both `debian:stable-slim` and `alpine:3` is worth the extra
minute: the binaries are built `CGO_ENABLED=0`, and musl is where a stray libc
dependency would surface.

[artifact-smoke.yml](../.github/workflows/artifact-smoke.yml) runs it
automatically on every published release across all six shipped platforms
(linux/darwin/windows × amd64/arm64), verifying each archive's checksum before
executing it. It can also be dispatched manually against any existing tag.
## Verifying backward compatibility against a real old release

[docs/archive-format.md](archive-format.md#format-version--compatibility)
promises the `1.x` line reads every version-1 archive for the life of the
series, and [docs/stability-policy.md](stability-policy.md#config-file-schema)
makes a similar promise for config files. The golden fixtures in
`internal/archive/testdata/` pin the *format*; this is how to check the promise
against what an old release actually wrote:

```sh
# 1. Get the old binary. (Swap darwin_arm64 for your platform.)
gh release download v0.5.1 --repo phenixblue/k8shark -p 'k8shark_*_darwin_arm64.tar.gz'
mkdir -p /tmp/old && tar xzf k8shark_0.5.1_darwin_arm64.tar.gz -C /tmp/old

# 2. Write a short capture config. The old release's own examples/k8shark.yaml
#    runs for 10m over 11 resources; this is the same schema, just quicker.
cat > /tmp/old-capture.yaml <<'YAML'
duration: 20s
output: /tmp/old-capture.kshrk
resources:
  - group: ""
    version: v1
    resource: pods
    namespaces: ["*"]
    interval: 5s
  - group: ""
    version: v1
    resource: nodes
    interval: 10s
YAML

# 3. Capture with the OLD binary against a live cluster.
make kind-up
KUBECONFIG=~/.kube/k8shark-dev.yaml /tmp/old/kshrk capture --config /tmp/old-capture.yaml

# 4. Read that archive with the CURRENT build — every command, plus the mock
#    server. A high API port keeps this from colliding with a real run.
make build
./kshrk inspect      /tmp/old-capture.kshrk -o json
./kshrk diagnose     /tmp/old-capture.kshrk -o json
./kshrk transitions  /tmp/old-capture.kshrk -o json
./kshrk query        /tmp/old-capture.kshrk '{.metadata.name}' -o json
./kshrk open         /tmp/old-capture.kshrk --api-port 18081 --kubeconfig-out /tmp/mock.yaml &
mock_pid=$!
until [ -s /tmp/mock.yaml ]; do sleep 1; done
kubectl --kubeconfig /tmp/mock.yaml get pods -A
kubectl --kubeconfig /tmp/mock.yaml get nodes
kill "$mock_pid"                       # don't leave a server bound to 18081

# 5. Separately, run the old release's *shipped* config through the current
#    validator — that's the config-schema half of the promise.
git show v0.5.1:examples/k8shark.yaml > /tmp/v051-shipped.yaml
./kshrk validate --config /tmp/v051-shipped.yaml
```

Expect `inspect -o json` to report `"archive_format_version": 1`, and every
command in step 4 to exit 0.

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
