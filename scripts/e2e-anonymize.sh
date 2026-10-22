#!/usr/bin/env bash
# k8shark anonymize end-to-end test script.
#
# Anonymizes every committed example capture and checks, the way a user would,
# that the result neither leaks nor breaks:
#
#   1. `anonymize --full-sweep --fail-on-residual` succeeds and reports
#      verified/0 residual (the tool's own leak check).
#   2. An INDEPENDENT probe — `kshrk query --text` for each original value in
#      the mapping — finds the value in the source capture (so the probe isn't
#      blind) and nowhere in the anonymized one.
#   3. The anonymized archive replays: real kubectl, default (Table) output
#      AND -o json, shows no original value and the same number of rows/items
#      as the source capture's replay.
#   4. Same salt => byte-identical output.
#   5. Negative controls: without --full-sweep the leak check must flag the
#      free-text leaks in an example known to have them, and
#      --fail-on-residual must then fail and remove the output. If the check
#      ever stops detecting anything, (1) would pass vacuously; this is what
#      keeps it honest.
#
# No cluster needed. Called automatically by:  make e2e-anonymize
# Can also be run directly: ./scripts/e2e-anonymize.sh
#
# Prerequisites: kubectl, jq (must be in PATH)
set -euo pipefail

PROJ_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY="${BINARY:-${PROJ_ROOT}/kshrk}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/k8shark-e2e-anon.XXXXXX")"
SALT_FILE="${WORK}/salt.txt"
CATEGORIES=(--categories namespace --categories node --categories pod --categories workload --categories ip --categories url --categories image)
# Resources whose default `kubectl get` output carries names (Table format).
RESOURCES=(ns nodes pods deploy rs sts ds jobs cronjobs services)
# Substring probes shorter than this match unrelated text ("api" in "kube-api-
# access-*"); short names are covered by the Go unit/e2e tests instead.
MIN_PROBE_LEN=4
PASS=0
FAIL=0
SERVER_PID=""

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
info() { printf '    %s\n' "$*"; }
pass() { printf '  \033[1;32m[OK]   %s\033[0m\n' "$*"; PASS=$((PASS + 1)); }
fail() { printf '  \033[1;31m[FAIL] %s\033[0m\n' "$*"; FAIL=$((FAIL + 1)); }
die()  { printf '\n\033[1;31mFATAL: %s\033[0m\n' "$*" >&2; exit 1; }

stop_server() {
  if [[ -n "${SERVER_PID}" ]]; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
    SERVER_PID=""
  fi
}
cleanup() { stop_server; rm -rf "${WORK}"; }
trap cleanup EXIT

command -v kubectl >/dev/null || die "kubectl not in PATH"
command -v jq >/dev/null || die "jq not in PATH"
[[ -x "${BINARY}" ]] || die "binary not found at ${BINARY} (run: make build)"

# A fixed salt: aliases are reproducible, and no run writes a generated salt
# to the log.
printf 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n' > "${SALT_FILE}"

# open_capture <archive> <kubeconfig-out>: start the mock server, wait until
# kubectl can talk to it.
open_capture() {
  local archive="$1" kc="$2"
  stop_server
  "${BINARY}" open "${archive}" --kubeconfig-out "${kc}" >"${WORK}/server.log" 2>&1 &
  SERVER_PID=$!
  local i
  for i in $(seq 1 50); do
    if [[ -s "${kc}" ]] && kubectl --kubeconfig "${kc}" --request-timeout=3s get --raw /version >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  cat "${WORK}/server.log" >&2 || true
  die "mock server for ${archive} did not become ready"
}

# replay_dump <archive> <outdir>: default-format `kubectl get` for every
# resource (what a user types first), plus -o json including managedFields.
replay_dump() {
  local archive="$1" out="$2" kc="${WORK}/replay.kubeconfig" r
  mkdir -p "${out}"
  rm -f "${kc}"
  open_capture "${archive}" "${kc}"
  for r in "${RESOURCES[@]}"; do
    kubectl --kubeconfig "${kc}" --request-timeout=10s get "${r}" -A >"${out}/${r}.txt" 2>&1 || true
    # --show-managed-fields: kubectl hides them by default, and their keys
    # (k:{"name":...}, k:{"ip":...}) are exactly where originals once hid.
    kubectl --kubeconfig "${kc}" --request-timeout=10s get "${r}" -A -o json --show-managed-fields >"${out}/${r}.json" 2>&1 || true
  done
  # Discovery documents: /api carries the API server's own host:port.
  kubectl --kubeconfig "${kc}" --request-timeout=10s get --raw /api >"${out}/raw-api.json" 2>&1 || true
  kubectl --kubeconfig "${kc}" --request-timeout=10s get --raw /apis >"${out}/raw-apis.json" 2>&1 || true
  stop_server
}

# originals <mapping.json>: every original value >= MIN_PROBE_LEN chars.
originals() {
  jq -r '[.[] | keys[]] | unique[] | select(length >= '"${MIN_PROBE_LEN}"')' "$1"
}

# query_hits <archive> <value>: number of objects/logs containing value.
query_hits() {
  "${BINARY}" query "$1" "$2" --text -o json 2>/dev/null | jq '.matches | length'
}

examples=()
for f in "${PROJ_ROOT}"/examples/*/capture.kshrk; do
  [[ -f "$f" ]] && examples+=("$f")
done
[[ ${#examples[@]} -gt 0 ]] || die "no example captures found"

for src in "${examples[@]}"; do
  name="$(basename "$(dirname "${src}")")"
  d="${WORK}/${name}"
  mkdir -p "${d}"
  log "example: ${name}"

  # ── 1. anonymize, strictly ──────────────────────────────────────────────────
  if "${BINARY}" anonymize "${src}" "${CATEGORIES[@]}" --full-sweep --fail-on-residual \
        --out "${d}/anon.kshrk" --anonymize-salt-file "${SALT_FILE}" \
        --emit-mapping --emit-mapping-plaintext --mapping-path "${d}/map.json" \
        -o json >"${d}/result.json" 2>"${d}/anon.err"; then
    pass "${name}: anonymize --full-sweep --fail-on-residual succeeded"
  else
    fail "${name}: anonymize --full-sweep --fail-on-residual failed"
    info "$(head -5 "${d}/anon.err")"
    jq -c '.residual_locations[:5]' "${d}/result.json" 2>/dev/null | sed 's/^/    residual: /' || true
    continue
  fi
  if [[ "$(jq -r '.verified' "${d}/result.json")" == "true" && "$(jq -r '.residual_occurrences' "${d}/result.json")" == "0" ]]; then
    pass "${name}: the built-in leak check ran and found 0 residual occurrences"
  else
    fail "${name}: verified/residual_occurrences = $(jq -c '[.verified,.residual_occurrences]' "${d}/result.json")"
  fi

  # ── 2. independent probe ────────────────────────────────────────────────────
  probed=0 seen_in_src=0 leaked=()
  while IFS= read -r orig; do
    probed=$((probed + 1))
    if [[ "$(query_hits "${src}" "${orig}")" -gt 0 ]]; then seen_in_src=$((seen_in_src + 1)); fi
    if [[ "$(query_hits "${d}/anon.kshrk" "${orig}")" -gt 0 ]]; then leaked+=("${orig}"); fi
  done < <(originals "${d}/map.json")
  if [[ ${probed} -eq 0 || ${seen_in_src} -eq 0 ]]; then
    fail "${name}: the probe found none of ${probed} originals in the SOURCE capture — it would pass vacuously"
  elif [[ ${#leaked[@]} -eq 0 ]]; then
    pass "${name}: independent probe: ${seen_in_src}/${probed} originals present in the source, 0 in the anonymized archive"
  else
    fail "${name}: independent probe found ${#leaked[@]} original(s) still in the anonymized archive"
    info "locations (values withheld): $("${BINARY}" query "${d}/anon.kshrk" "${leaked[0]}" --text -o json | jq -c '[.matches[:3][] | {resource, field}]')"
  fi

  # ── 3. replay with real kubectl ─────────────────────────────────────────────
  replay_dump "${src}" "${d}/replay-src"
  replay_dump "${d}/anon.kshrk" "${d}/replay-anon"
  src_names=0 replay_leaks=()
  while IFS= read -r orig; do
    if grep -rqF -- "${orig}" "${d}/replay-src"; then src_names=$((src_names + 1)); fi
    if grep -rqF -- "${orig}" "${d}/replay-anon"; then replay_leaks+=("${orig}"); fi
  done < <(originals "${d}/map.json")
  if [[ ${src_names} -eq 0 ]]; then
    fail "${name}: kubectl output of the SOURCE replay contains no original — the replay probe would pass vacuously"
  elif [[ ${#replay_leaks[@]} -eq 0 ]]; then
    pass "${name}: kubectl replay (Table + -o json): ${src_names} originals visible in the source replay, 0 in the anonymized one"
  else
    fail "${name}: kubectl output of the anonymized replay still shows ${#replay_leaks[@]} original(s)"
    info "first one appears in: $(grep -rlF -- "${replay_leaks[0]}" "${d}/replay-anon" | xargs -n1 basename | tr '\n' ' ')"
  fi

  # Same shape: anonymizing must not drop or add rows/items.
  shape_ok=1
  for r in "${RESOURCES[@]}"; do
    a="$(wc -l <"${d}/replay-src/${r}.txt" | tr -d ' ')"
    b="$(wc -l <"${d}/replay-anon/${r}.txt" | tr -d ' ')"
    ia="$(jq '.items | length' "${d}/replay-src/${r}.json" 2>/dev/null || echo err)"
    ib="$(jq '.items | length' "${d}/replay-anon/${r}.json" 2>/dev/null || echo err)"
    if [[ "${a}" != "${b}" || "${ia}" != "${ib}" ]]; then
      shape_ok=0
      info "kubectl get ${r}: source ${a} lines/${ia} items vs anonymized ${b} lines/${ib} items"
    fi
  done
  if [[ ${shape_ok} -eq 1 ]]; then
    pass "${name}: replay has the same rows/items as the source for every resource"
  else
    fail "${name}: replay shape differs from the source"
  fi

  # ── 4. determinism ──────────────────────────────────────────────────────────
  "${BINARY}" anonymize "${src}" "${CATEGORIES[@]}" --full-sweep --fail-on-residual \
      --out "${d}/anon2.kshrk" --anonymize-salt-file "${SALT_FILE}" >/dev/null 2>&1 || true
  if [[ -f "${d}/anon2.kshrk" ]] && cmp -s "${d}/anon.kshrk" "${d}/anon2.kshrk"; then
    pass "${name}: same salt => byte-identical archive"
  else
    fail "${name}: re-running with the same salt produced a different archive"
  fi
done

# ── 5. negative controls ──────────────────────────────────────────────────────
# multi-namespace carries kubectl.kubernetes.io/last-applied-configuration
# annotations and app labels — free-text occurrences only --full-sweep reaches.
log "negative controls (multi-namespace, WITHOUT --full-sweep)"
nsrc="${PROJ_ROOT}/examples/multi-namespace/capture.kshrk"
if [[ -f "${nsrc}" ]]; then
  d="${WORK}/neg"; mkdir -p "${d}"
  "${BINARY}" anonymize "${nsrc}" "${CATEGORIES[@]}" --out "${d}/o.kshrk" \
      --anonymize-salt-file "${SALT_FILE}" -o json >"${d}/result.json" 2>/dev/null || true
  if [[ "$(jq -r '.verified' "${d}/result.json")" == "true" && "$(jq -r '.residual_occurrences' "${d}/result.json")" -gt 0 ]]; then
    pass "without --full-sweep the leak check reports residual occurrences ($(jq -r '.residual_occurrences' "${d}/result.json"))"
  else
    fail "without --full-sweep the leak check reported nothing — it would not catch a regression"
  fi
  # The report must say where, never what: none of this example's real names
  # (committed in the capture) may appear anywhere in it.
  if jq -r '.. | strings' "${d}/result.json" | grep -qF -e "team-backend" -e "team-data" -e "team-frontend" -e "orders-api" -e "storefront" -e "redis"; then
    fail "the residual report contains an original value"
  else
    pass "the residual report names locations, never original values"
  fi
  rm -f "${d}/o2.kshrk"
  if "${BINARY}" anonymize "${nsrc}" "${CATEGORIES[@]}" --fail-on-residual --out "${d}/o2.kshrk" \
        --anonymize-salt-file "${SALT_FILE}" >/dev/null 2>&1; then
    fail "--fail-on-residual succeeded on an archive that still leaks"
  elif [[ -e "${d}/o2.kshrk" ]]; then
    fail "--fail-on-residual failed but left the leaking archive behind"
  else
    pass "--fail-on-residual fails and removes the leaking output"
  fi
else
  fail "examples/multi-namespace/capture.kshrk missing; cannot run negative controls"
fi

log "Results: ${PASS} passed, ${FAIL} failed"
[[ ${FAIL} -eq 0 ]] || exit 1
