#!/usr/bin/env sh
# Smoke-test an *already-extracted* kshrk binary from a published release.
#
# This is the one check that runs the bytes users actually download, on the
# platform they download them for. The CI test matrix builds from source and
# runs `go test`; it never executes a release artifact, so a packaging or
# cross-compilation defect that only shows up in the shipped binary would not
# be caught there. Wired up in .github/workflows/artifact-smoke.yml.
#
# POSIX sh on purpose — it has to run on Alpine (busybox ash) as well as on
# glibc distros and Windows Git Bash, so: no arrays, no [[ ]], no local.
#
# Usage:   ./scripts/artifact-smoke.sh <binary> <expected-version> [repo-root]
# Env:     EXPECT_CONTRACT=0  skip the -o json shape assertions, for binaries
#                             released before the v1.0 contract landed (#320).
set -u

BIN="${1:?usage: artifact-smoke.sh <binary> <expected-version> [repo-root]}"
WANT_VERSION="${2:?expected version, e.g. 1.0.0}"
ROOT="${3:-.}"
EXPECT_CONTRACT="${EXPECT_CONTRACT:-1}"

BASIC="$ROOT/examples/basic-workloads/capture.kshrk"
ROLLING="$ROOT/examples/rolling-update/capture.kshrk"

PASS=0
FAIL=0
ok()   { printf '  [OK]   %s\n' "$1"; PASS=$((PASS + 1)); }
bad()  { printf '  [FAIL] %s\n' "$1"; FAIL=$((FAIL + 1)); }

printf -- '--- %s/%s  kshrk=%s ---\n' "$(uname -s)" "$(uname -m)" "$BIN"

# ── Portability: does the shipped binary run at all, everywhere? ──────────────
# Catches the class of defect this script exists for: a dynamically-linked
# binary that can't resolve libc on musl, a wrong-arch build, a corrupt
# archive, or a stripped-away entry point.
if out=$("$BIN" version 2>&1); then
  ok "version runs: $out"
else
  bad "version failed: $out"
  printf '  => cannot execute the binary; abandoning the rest\n'
  exit 1
fi

case "$out" in
  *"$WANT_VERSION"*) ok "version reports $WANT_VERSION" ;;
  *) bad "version is '$out', want it to contain '$WANT_VERSION'" ;;
esac

if "$BIN" --help >/dev/null 2>&1; then ok "--help exits 0"; else bad "--help exited nonzero"; fi

# ── Real work: read a zip+zstd archive off disk ───────────────────────────────
# `version`/`--help` would pass on a binary whose archive layer was broken, so
# actually decode a capture.
if json=$("$BIN" inspect "$BASIC" -o json 2>&1); then
  ok "inspect -o json read a real archive"
  case "$json" in
    *'"record_count"'*) ok "inspect output has record_count" ;;
    *) bad "inspect output missing record_count" ;;
  esac
else
  bad "inspect failed: $(printf '%s' "$json" | head -n 2)"
fi

# ── Exit-code contract (docs/stability-policy.md) ─────────────────────────────
"$BIN" inspect "$ROOT/no-such-archive.kshrk" >/dev/null 2>&1
rc=$?
if [ "$rc" -eq 2 ]; then ok "missing archive exits 2"; else bad "missing archive exited $rc, want 2"; fi

# ── Frozen -o json shapes (#320) ──────────────────────────────────────────────
if [ "$EXPECT_CONTRACT" = "1" ]; then
  case "$json" in
    *'"schema_version"'*) ok "inspect has schema_version" ;;
    *) bad "inspect missing schema_version" ;;
  esac
  case "$json" in
    *'"archive_format_version"'*) ok "inspect has archive_format_version" ;;
    *) bad "inspect missing archive_format_version (renamed from format_version in #320)" ;;
  esac

  # findings must be [] not null, so `jq '.findings[]'` works on a clean capture.
  if d=$("$BIN" diagnose "$BASIC" -o json 2>&1); then
    case "$d" in
      *'"findings": null'*|*'"findings":null'*) bad "diagnose findings is null, want []" ;;
      *'"findings"'*) ok "diagnose findings is an array" ;;
      *) bad "diagnose output has no findings key" ;;
    esac
  else
    bad "diagnose failed: $(printf '%s' "$d" | head -n 2)"
  fi

  # transitions must be an object envelope, not a bare array.
  if t=$("$BIN" transitions "$ROLLING" -o json 2>&1); then
    case "$t" in
      \{*) ok "transitions emits an object envelope" ;;
      \[*) bad "transitions emits a bare array; #320 wrapped it in an object" ;;
      *) bad "transitions output is neither object nor array" ;;
    esac
  else
    bad "transitions failed: $(printf '%s' "$t" | head -n 2)"
  fi
else
  printf '  [skip] -o json shape checks (EXPECT_CONTRACT=0)\n'
fi

printf '  => passed=%s failed=%s\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
