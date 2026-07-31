# Releases

## How releases are created

Releases are cut by pushing a version tag to `main`. The tag triggers the [release workflow](../.github/workflows/release.yml) which runs [GoReleaser](https://goreleaser.com) to build and publish everything.

```sh
git tag v1.0.0-rc.1
git push origin v1.0.0-rc.1
```

Use [semantic versioning](https://semver.org): `vMAJOR.MINOR.PATCH`. Tags that contain a pre-release identifier (e.g. `v1.0.0-rc.1`) are automatically marked as pre-release on GitHub.

### Versioning policy

**Pre-`v1.0.0`** (every release up to and including the current `v0.x` line):
backward-incompatible changes are released by bumping the **minor** version —
there is no stability promise on any CLI-facing surface yet. The one
exception is the `.kshrk` archive format, which already carries its own
version-gated stability promise independent of the CLI's own version — see
[docs/archive-format.md](archive-format.md#format-version--compatibility).

**`v1.0.0` and later**: what a MAJOR/MINOR/PATCH bump means, and exactly what
is and isn't allowed to change without one, is defined in
[docs/stability-policy.md](stability-policy.md) — see that page rather than
this one for the compatibility contract itself. In short:

- **PATCH** (`v1.0.1`) — bug fixes only, no *intentional* behavior change to
  a documented surface (correcting a bug in one is what a patch is for).
- **MINOR** (`v1.1.0`) — new, backward-compatible features (flags, config
  keys, subcommands).
- **MAJOR** (`v2.0.0`) — a breaking change to anything listed as a stable
  surface in the stability policy.

Only the latest minor receives patch releases; see the stability policy's
[backport section](stability-policy.md#backport--patch-policy).

## What the release workflow does

1. **Tests** — runs `go test ./...` against the tagged commit. The release is blocked if tests fail.
2. **GoReleaser** — builds cross-platform binaries, packages archives, generates a checksum file, and publishes the GitHub Release.
3. **SBOM** — [Syft](https://github.com/anchore/syft) generates a Software Bill of Materials for each archive artifact.
4. **Signing** — the `checksums.txt` file is signed with [cosign](https://github.com/sigstore/cosign) using keyless OIDC signing (no long-lived keys). The signature and certificate are attached to the release together, as a single `checksums.txt.bundle`.
5. **Attestation** — GitHub's `attest-build-provenance` action attaches a build provenance attestation to each release archive (`.tar.gz`/`.zip`).
6. **Homebrew tap** — GoReleaser pushes an updated cask to `phenixblue/homebrew-tap`, so `brew upgrade --cask k8shark` picks up the new version automatically.

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
| `HOMEBREW_TAP_GITHUB_TOKEN` | A GitHub PAT with `repo` scope on `phenixblue/homebrew-tap`, used to push the Homebrew cask. |

The cosign signing uses GitHub's OIDC token — no additional secret is needed.

## Verifying a release

### Verify the checksum signature

```sh
# Download the release artifacts
gh release download v1.0.0-rc.3 --repo phenixblue/k8shark

# Verify the cosign signature
cosign verify-blob \
  --bundle checksums.txt.bundle \
  --certificate-identity-regexp '^https://github\.com/phenixblue/k8shark/\.github/workflows/release\.yml@refs/tags/v.+$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  checksums.txt
```

**The identity regexp is anchored on purpose — don't loosen it.**
`--certificate-identity-regexp` is an unanchored search, so a bare
`https://github.com/phenixblue/k8shark/.github/workflows/release.yml` would
also match any identity that merely *contains* that string (say
`.../evil/x/.github/workflows/release.yml@refs/tags/v9?u=<the real one>`),
which defeats the point of checking it. `^`/`$` plus escaped dots plus the
required `@refs/tags/v…` suffix pins it to this repo's release workflow run
from a version tag. The signing identity looks like
`https://github.com/phenixblue/k8shark/.github/workflows/release.yml@refs/tags/v1.0.0-rc.2`
(read off that release's own certificate).

The signature and certificate live together in a single `checksums.txt.bundle`
(cosign's newer bundle format), which is what `--bundle` above reads. Releases
are signed by the cosign version pinned in `release.yml` — v3.x, via
`cosign-installer` v4.x — so verifying with a v3.x cosign is the tested
combination.

Releases before `v1.0.0-rc.3` instead carry separate `checksums.txt.sig` and
`checksums.txt.pem`. Verify those by swapping `--bundle` for `--signature
checksums.txt.sig --certificate checksums.txt.pem`. Those two flags are
deprecated in cosign v3 but still functional (it prints a warning), so a
single v3 cosign verifies both the old and new layouts — you don't need an
older cosign for older releases.

### Verify a binary checksum

```sh
sha256sum --check --ignore-missing checksums.txt
```

### Verify the build attestation

```sh
gh attestation verify k8shark_<version>_linux_amd64.tar.gz \
  --repo phenixblue/k8shark
```

## Local release builds

You can build a release locally without publishing using `make`:

```sh
# Full pipeline including signing, but no publish.
# Signing is keyless, so this needs an OIDC identity: expect the cosign step
# to prompt for a browser login, or to fail if you're headless.
make release-snapshot

# Dry-run with SBOM + Homebrew output, skipping BOTH signing and publishing.
# This is the one to use for a quick local check.
make release-local
```

Both commands place output in `./dist/`. Requires `goreleaser` in your PATH (`go install github.com/goreleaser/goreleaser/v2@latest`).

Signing is also covered in CI:
[release-check.yml](../.github/workflows/release-check.yml) runs the full
snapshot with `--skip=publish` only — it signs for real (using the same pinned
cosign as the release job) and verifies the resulting
`checksums.txt.bundle` round-trips — on every change to `.goreleaser.yaml` or
either release workflow. It didn't always: it used to skip signing, and that
gap is exactly how a cosign v3 incompatibility survived until a `v1.0.0-rc.3`
tag was about to be cut.

Because that job only triggers on release-config paths, a change elsewhere
that somehow affects packaging still won't be signed-tested until the tag.
`make release-snapshot` reproduces the full signing path locally if you want
belt-and-braces before a release; `make release-local` does not.

## CI pipeline (non-release)

Every push to `main` and every pull request runs the [CI workflow](../.github/workflows/ci.yml):

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
