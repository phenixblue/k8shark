# Releases

## How releases are created

Releases are cut by pushing a version tag to `main`. The tag triggers the [release workflow](.github/workflows/release.yml) which runs [GoReleaser](https://goreleaser.com) to build and publish everything.

```sh
git tag v1.2.3
git push origin v1.2.3
```

Use [semantic versioning](https://semver.org): `vMAJOR.MINOR.PATCH`. Tags that contain a pre-release identifier (e.g. `v1.2.3-rc.1`) are automatically marked as pre-release on GitHub.

## What the release workflow does

1. **Tests** — runs `go test ./...` against the tagged commit. The release is blocked if tests fail.
2. **GoReleaser** — builds cross-platform binaries, packages archives, generates a checksum file, and publishes the GitHub Release.
3. **SBOM** — [Syft](https://github.com/anchore/syft) generates a Software Bill of Materials for each archive artifact.
4. **Signing** — the `checksums.txt` file is signed with [cosign](https://github.com/sigstore/cosign) using keyless OIDC signing (no long-lived keys). The signature and certificate are attached to the release.
5. **Attestation** — GitHub's `attest-build-provenance` action attaches a build provenance attestation to the checksum file.
6. **Homebrew tap** — GoReleaser pushes an updated formula to `phenixblue/homebrew-tap`, so `brew upgrade k8shark` picks up the new version automatically.

## Build matrix

Each release produces binaries for:

| OS | Architectures |
|----|---------------|
| Linux | amd64, arm64 |
| macOS | amd64 (Intel), arm64 (Apple Silicon) |
| Windows | amd64, arm64 |

Archives are named `k8shark_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows).

## Required repository secrets

| Secret | Description |
|--------|-------------|
| `GITHUB_TOKEN` | Provided automatically by GitHub Actions; used to create the GitHub Release. |
| `HOMEBREW_TAP_GITHUB_TOKEN` | A GitHub PAT with `repo` scope on `phenixblue/homebrew-tap`, used to push the Homebrew formula. |

The cosign signing uses GitHub's OIDC token — no additional secret is needed.

## Verifying a release

### Verify the checksum signature

```sh
# Download the release artifacts
gh release download v1.2.3 --repo phenixblue/k8shark

# Verify the cosign signature
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/phenixblue/k8shark/.github/workflows/release.yml' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  checksums.txt
```

### Verify a binary checksum

```sh
sha256sum --check --ignore-missing checksums.txt
```

### Verify the build attestation

```sh
gh attestation verify kshrk_linux_amd64.tar.gz \
  --repo phenixblue/k8shark
```

## Local release builds

You can build a release locally without publishing using `make`:

```sh
# Snapshot build (no signing, no publish)
make release-snapshot

# Dry-run with SBOM + Homebrew output but no signing or publishing
make release-local
```

Both commands place output in `./dist/`. Requires `goreleaser` in your PATH (`go install github.com/goreleaser/goreleaser/v2@latest`).

## CI pipeline (non-release)

Every push to `main` and every pull request runs the [CI workflow](.github/workflows/ci.yml):

```
contract ──┬── test ── build
           └── lint
```

| Job | What it checks |
|-----|----------------|
| **contract** | `go mod tidy` (no drift), `gofmt` (no unformatted files), `go vet` |
| **test** | `go test -race ./...` |
| **build** | `make build` (binary compiles) |
| **lint** | `golangci-lint run` (errcheck, govet, ineffassign, staticcheck, unused) |
