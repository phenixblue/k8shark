#!/usr/bin/env bash
# Shared helper: verify a KinD cluster actually reached the Kubernetes version
# that NODE_IMAGE asked for.
#
# Sourced by scripts/e2e.sh and scripts/conformance.sh. docs/development.md
# cites both together as the way to reproduce the verified Kubernetes version
# range, so both have to enforce the same thing — otherwise the docs promise a
# guarantee only one of them keeps. A logged-but-unchecked version would let a
# typo'd or unavailable tag quietly fall back to kind's default and make a
# version matrix look broader than it was.
#
# The sourcing script must define `info` and `die`.

# assert_node_image_version <node_image> <server_version>
#
# No-op when node_image is empty (no version was requested, so there's nothing
# to hold the cluster to). Fatal when a version was requested but can't be
# determined, or doesn't match.
assert_node_image_version() {
  local node_image="$1" server_version="$2" want_tag

  [[ -n "$node_image" ]] || return 0

  if [[ -z "$server_version" ]]; then
    die "Could not determine the server version, but NODE_IMAGE=$node_image requested one. Refusing to report a version-matrix result that can't be verified."
  fi

  # kindest/node:v1.32.3, optionally carrying an @sha256:… digest.
  want_tag="${node_image%%@*}"
  want_tag="${want_tag##*:}"

  if [[ ! "$want_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    info "NODE_IMAGE tag '$want_tag' isn't a plain vX.Y.Z; skipping the version-match check"
    return 0
  fi

  # Prefix match, so a vendor-suffixed gitVersion (v1.32.3+something) still
  # counts as reaching v1.32.3.
  if [[ "$server_version" != "$want_tag"* ]]; then
    die "NODE_IMAGE=$node_image requested $want_tag but the cluster is running $server_version."
  fi
  info "requested Kubernetes version reached ($want_tag)"
}
