#!/usr/bin/env python3
"""Differential comparison of the k8shark mock server vs a live apiserver.

Part of issue #136 (phase 1). Driven by scripts/conformance.sh, which sets:
  LIVE_KUBECONFIG  kubeconfig for the real KinD apiserver
  MOCK_KUBECONFIG  kubeconfig for `kshrk open`'s mock server
  PROBE_NS         a namespace populated with test resources

For each in-scope endpoint (discovery, version, OpenAPI, resource reads,
health, error shapes) it fetches the response from both servers, normalizes
volatile fields, diffs, and prints a categorized compatibility report.

Both servers are reached through `kubectl proxy` so we get uniform plain-HTTP
access with the server's real status codes and raw (error) bodies.
"""
import json
import os
import re
import select
import subprocess
import sys
import time
import urllib.request

LIVE_KC = os.environ["LIVE_KUBECONFIG"]
MOCK_KC = os.environ["MOCK_KUBECONFIG"]
PROBE_NS = os.environ.get("PROBE_NS", "default")

# Documented baseline of accepted divergences. Each entry is "category::name".
# A divergence in the baseline is reported but does NOT fail the run — the gate
# fires only on *new* divergences. Regenerate with WRITE_BASELINE=1.
BASELINE_PATH = os.environ.get(
    "CONFORMANCE_BASELINE",
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "conformance-baseline.json"),
)
WRITE_BASELINE = os.environ.get("WRITE_BASELINE") == "1"


def load_baseline():
    try:
        with open(BASELINE_PATH) as f:
            return set(json.load(f).get("accepted", []))
    except FileNotFoundError:
        return set()


BASELINE = load_baseline()

# ── ANSI ──────────────────────────────────────────────────────────────────────
BOLD, DIM, RED, GRN, YEL, CYN, RST = (
    "\033[1m", "\033[2m", "\033[1;31m", "\033[1;32m", "\033[1;33m", "\033[1;36m", "\033[0m",
)

# Results: (category, name, verdict, detail).
# verdict in {PASS, EXPECTED, ACCEPTED, UNEXPECTED}. ACCEPTED is an UNEXPECTED
# divergence that matches the documented baseline, so it does not fail the run.
RESULTS = []


def record(category, name, verdict, detail=""):
    # Downgrade a divergence to ACCEPTED when it is in the documented baseline.
    if verdict == "UNEXPECTED" and f"{category}::{name}" in BASELINE:
        verdict = "ACCEPTED"
    RESULTS.append((category, name, verdict, detail))
    mark = {"PASS": f"{GRN}[MATCH]{RST}", "EXPECTED": f"{YEL}[EXPECTED DIFF]{RST}",
            "ACCEPTED": f"{YEL}[ACCEPTED DIFF]{RST}", "UNEXPECTED": f"{RED}[NEW DIVERGENCE]{RST}"}[verdict]
    print(f"  {mark} {name}")
    if detail:
        for line in detail.splitlines():
            print(f"           {DIM}{line}{RST}")


# ── kubectl proxy management ────────────────────────────────────────────────────
class Proxy:
    def __init__(self, kubeconfig):
        self.kubeconfig = kubeconfig
        self.proc = None
        self.base = None

    def start(self):
        self.proc = subprocess.Popen(
            ["kubectl", "--kubeconfig", self.kubeconfig, "proxy", "--port=0"],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        )
        # kubectl prints: "Starting to serve on 127.0.0.1:PORT". Use select so a
        # silent/blocked proxy can't hang readline() past the deadline.
        deadline = time.time() + 15
        while time.time() < deadline:
            if self.proc.poll() is not None:
                break  # proxy exited before serving
            ready, _, _ = select.select([self.proc.stdout], [], [], deadline - time.time())
            if not ready:
                continue
            line = self.proc.stdout.readline()
            if not line:
                break
            m = re.search(r"Starting to serve on (\S+)", line)
            if m:
                self.base = "http://" + m.group(1)
                return
        raise RuntimeError(f"kubectl proxy for {self.kubeconfig} did not start")

    def stop(self):
        if self.proc:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.proc.kill()

    def get(self, path):
        """Return (status_code, body_bytes). status 0 on transport error."""
        req = urllib.request.Request(self.base + path, headers={"Accept": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=15) as r:
                return r.status, r.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()
        except Exception as e:  # noqa: BLE001
            return 0, str(e).encode()

    def get_json(self, path):
        code, body = self.get(path)
        try:
            return code, json.loads(body)
        except Exception:  # noqa: BLE001
            return code, None


# ── normalization ───────────────────────────────────────────────────────────────
VOLATILE_META = {
    "resourceVersion", "creationTimestamp", "uid", "generation",
    "managedFields", "selfLink", "annotations",
}


def key_paths(obj, prefix=""):
    """Set of dotted key paths in a JSON object (arrays collapsed to [])."""
    paths = set()
    if isinstance(obj, dict):
        for k, v in obj.items():
            p = f"{prefix}.{k}" if prefix else k
            paths.add(p)
            paths |= key_paths(v, p)
    elif isinstance(obj, list) and obj:
        paths |= key_paths(obj[0], prefix + "[]")
    return paths


def strip_volatile_item(item):
    """Drop fields that legitimately drift between capture time and 'now'."""
    if not isinstance(item, dict):
        return item
    meta = item.get("metadata", {})
    for k in list(meta):
        if k in VOLATILE_META:
            meta.pop(k, None)
    # status drifts constantly for live workloads; compare spec/metadata shape only.
    item.pop("status", None)
    return item


# ═══════════════════════════════════════════════════════════════════════════════
def main():
    live = Proxy(LIVE_KC)
    mock = Proxy(MOCK_KC)
    live.start()
    mock.start()
    try:
        check_discovery(live, mock)
        check_version(live, mock)
        check_openapi(live, mock)
        check_resources(live, mock)
        check_health(live, mock)
        check_errors(live, mock)
    finally:
        live.stop()
        mock.stop()
    summarize()


# ── A. Discovery ────────────────────────────────────────────────────────────────
def check_discovery(live, mock):
    print(f"\n{BOLD}{CYN}== A. Discovery =={RST}")

    # /api (APIVersions)
    _, lm = live.get_json("/api")
    cm_code, cm = mock.get_json("/api")
    if cm and cm.get("kind") == "APIVersions" and "v1" in cm.get("versions", []):
        record("discovery", "/api APIVersions envelope", "PASS")
    else:
        record("discovery", "/api APIVersions envelope", "UNEXPECTED", f"mock returned: {cm}")

    # /apis (APIGroupList) — mock is a subset of live; verify each mock group exists in live.
    _, la = live.get_json("/apis")
    _, ma = mock.get_json("/apis")
    if not isinstance(la, dict) or not isinstance(ma, dict):
        record("discovery", "/apis APIGroupList", "UNEXPECTED",
               f"could not read /apis as JSON (live={type(la).__name__}, mock={type(ma).__name__}); "
               "skipping per-group comparison")
        return
    live_groups = {g["name"]: {v["groupVersion"] for v in g["versions"]} for g in la.get("groups", [])}
    mock_groups = {g["name"]: {v["groupVersion"] for v in g.get("versions", [])} for g in ma.get("groups", [])}
    missing = [g for g in mock_groups if g not in live_groups]
    if missing:
        record("discovery", "/apis groups subset of live", "UNEXPECTED",
               "mock advertises groups the live server does not: " + ", ".join(missing))
    else:
        record("discovery", "/apis groups subset of live", "PASS",
               f"mock advertises {len(mock_groups)}/{len(live_groups)} live groups "
               f"(subset by design): {', '.join(sorted(mock_groups)) or '(none)'}")

    # Per groupVersion APIResourceList field comparison.
    gvs = ["/api/v1"] + sorted(
        f"/apis/{gv}" for versions in mock_groups.values() for gv in versions
    )
    for gv_path in gvs:
        compare_resource_list(live, mock, gv_path)


def compare_resource_list(live, mock, gv_path):
    _, lr = live.get_json(gv_path)
    _, mr = mock.get_json(gv_path)
    if not mr or mr.get("kind") != "APIResourceList":
        record("discovery", f"{gv_path} APIResourceList", "UNEXPECTED", f"mock returned: {mr}")
        return
    live_by_name = {r["name"]: r for r in ((lr or {}).get("resources") or [])}
    field_diffs, missing_sub, missing_meta = [], [], []
    verbs_reduced = False  # mock replaced live's verbs with a read-only subset
    verbs_diverged = False  # mock verbs differ in some other (unexpected) way
    for r in mr.get("resources", []):
        name = r["name"]
        lref = live_by_name.get(name)
        if not lref:
            field_diffs.append(f"{name}: not present on live server")
            continue
        # verbs: the mock synthesizes read-only {get,list,watch}; live has the
        # full write set. Observe rather than assume, so this stays honest if
        # the mock ever serves real verbs from a captured discovery document.
        lverbs, mverbs = set(lref.get("verbs", [])), set(r.get("verbs", []))
        if mverbs != lverbs:
            if mverbs <= {"get", "list", "watch"}:
                verbs_reduced = True
            else:
                verbs_diverged = True
                field_diffs.append(f"{name}.verbs {sorted(mverbs)} != live {sorted(lverbs)}")
        for f in ("kind", "namespaced", "singularName"):
            if r.get(f) != lref.get(f):
                field_diffs.append(f"{name}.{f}={r.get(f)!r} != live {lref.get(f)!r}")
        if sorted(r.get("shortNames", [])) != sorted(lref.get("shortNames", [])):
            field_diffs.append(
                f"{name}.shortNames {sorted(r.get('shortNames', []))} != live {sorted(lref.get('shortNames', []))}")
        # live-only fields the mock omits
        for f in ("categories", "storageVersionHash"):
            if f in lref and f not in r:
                missing_meta.append(f"{name}.{f}")
    # subresources live lists but the mock does not (e.g. pods/status, .../scale).
    # Only count those whose parent the mock serves but the subresource itself
    # is genuinely absent from the mock's list.
    mock_names = {r["name"] for r in mr.get("resources", [])}
    mock_parents = {n.split("/")[0] for n in mock_names}
    for name in live_by_name:
        if "/" in name and name.split("/")[0] in mock_parents and name not in mock_names:
            missing_sub.append(name)

    if field_diffs:
        record("discovery", f"{gv_path} resource fields", "UNEXPECTED", "\n".join(field_diffs))
    else:
        record("discovery", f"{gv_path} resource fields (kind/namespaced/singular/shortNames)", "PASS")
    if missing_sub:
        record("discovery", f"{gv_path} subresources", "EXPECTED",
               "mock omits subresource entries live advertises: " + ", ".join(sorted(missing_sub)))
    if missing_meta:
        record("discovery", f"{gv_path} resource metadata", "EXPECTED",
               "mock omits live-only fields: " + ", ".join(sorted(missing_meta)))
    if verbs_reduced and not verbs_diverged:
        record("discovery", f"{gv_path} verbs (read-only)", "EXPECTED",
               "mock advertises a read-only verb subset (e.g. {get,list,watch}); "
               "live advertises the full write set")
    elif not verbs_reduced and not verbs_diverged:
        record("discovery", f"{gv_path} verbs match live", "PASS")


# ── B. Version ───────────────────────────────────────────────────────────────────
def check_version(live, mock):
    print(f"\n{BOLD}{CYN}== B. Version =={RST}")
    _, lv = live.get_json("/version")
    _, mv = mock.get_json("/version")
    if not isinstance(mv, dict) or not isinstance(lv, dict):
        record("version", "/version payload", "UNEXPECTED",
               f"could not read /version as JSON (live={type(lv).__name__}, mock={type(mv).__name__})")
        return
    missing_keys = set(lv) - set(mv)
    if missing_keys:
        record("version", "/version keys", "UNEXPECTED", "mock missing keys: " + ", ".join(sorted(missing_keys)))
    else:
        record("version", "/version keys present", "PASS")
    # gitVersion should reflect the captured cluster version.
    if mv.get("gitVersion") and mv["gitVersion"].startswith("v1."):
        record("version", "/version gitVersion reflects captured cluster", "PASS",
               f"mock gitVersion={mv['gitVersion']} live={lv.get('gitVersion')}")
    else:
        record("version", "/version gitVersion", "UNEXPECTED", f"mock gitVersion={mv.get('gitVersion')!r}")
    # major/minor are hard-coded 1/0 in the mock.
    if (mv.get("major"), mv.get("minor")) != (lv.get("major"), lv.get("minor")):
        record("version", "/version major/minor", "EXPECTED",
               f"mock major/minor={mv.get('major')}/{mv.get('minor')} != live "
               f"{lv.get('major')}/{lv.get('minor')} (hard-coded stub)")


# ── C. OpenAPI ───────────────────────────────────────────────────────────────────
def check_openapi(live, mock):
    print(f"\n{BOLD}{CYN}== C. OpenAPI =={RST}")
    lc, lb = live.get_json("/openapi/v2")
    mc, mb = mock.get_json("/openapi/v2")
    if mc == 200 and isinstance(mb, dict) and "swagger" in mb:
        real = bool(mb.get("definitions"))
        record("openapi", "/openapi/v2 served", "PASS" if real else "EXPECTED",
               "" if real else "mock serves a minimal stub (no definitions) when the capture lacks OpenAPI")
    else:
        record("openapi", "/openapi/v2 served", "UNEXPECTED", f"mock status={mc}")
    mc3, _ = mock.get("/openapi/v3")
    lc3, _ = live.get("/openapi/v3")
    if mc3 == 200:
        record("openapi", "/openapi/v3 served", "PASS")
    else:
        record("openapi", "/openapi/v3 served", "EXPECTED",
               f"mock returns {mc3} (live={lc3}); only served when captured")


# ── D. Resource reads ────────────────────────────────────────────────────────────
def check_resources(live, mock):
    print(f"\n{BOLD}{CYN}== D. Resource reads =={RST}")
    targets = [
        ("/api/v1/namespaces", "NamespaceList"),
        ("/api/v1/nodes", "NodeList"),
        (f"/api/v1/namespaces/{PROBE_NS}/pods", "PodList"),
        (f"/api/v1/namespaces/{PROBE_NS}/services", "ServiceList"),
        (f"/api/v1/namespaces/{PROBE_NS}/configmaps", "ConfigMapList"),
        (f"/apis/apps/v1/namespaces/{PROBE_NS}/deployments", "DeploymentList"),
    ]
    for path, want_kind in targets:
        _, lb = live.get_json(path)
        code, mb = mock.get_json(path)
        if not mb or mb.get("kind") != want_kind:
            record("reads", f"{path} list envelope", "UNEXPECTED",
                   f"mock status={code} kind={mb.get('kind') if mb else None} want {want_kind}")
            continue
        env_ok = mb.get("apiVersion") == (lb or {}).get("apiVersion") and \
            "resourceVersion" in mb.get("metadata", {}) and isinstance(mb.get("items"), list)
        record("reads", f"{path} list envelope", "PASS" if env_ok else "UNEXPECTED",
               "" if env_ok else f"apiVersion/metadata.resourceVersion/items mismatch: keys={list(mb.keys())}")
        # per-item structural comparison for an object present in both
        compare_item_structure(path, lb, mb)


def compare_item_structure(path, lb, mb):
    live_items = {i["metadata"]["name"]: i for i in (lb or {}).get("items", []) if i.get("metadata")}
    for mi in mb.get("items", []):
        name = mi.get("metadata", {}).get("name")
        li = live_items.get(name)
        if not li:
            continue
        lp = key_paths(strip_volatile_item(json.loads(json.dumps(li))))
        mp = key_paths(strip_volatile_item(json.loads(json.dumps(mi))))
        missing = sorted(lp - mp)
        extra = sorted(mp - lp)
        detail = []
        if missing:
            detail.append("spec/metadata fields on live but absent in mock: " + ", ".join(missing[:12]))
        if extra:
            detail.append("fields in mock but not live: " + ", ".join(extra[:12]))
        if not missing and not extra:
            record("reads", f"{path} item structure ({name})", "PASS")
        else:
            # Mock replays the captured object, so structural loss here is a real concern.
            record("reads", f"{path} item structure ({name})", "UNEXPECTED", "\n".join(detail))
        return  # one representative item per list is enough


# ── E. Health ────────────────────────────────────────────────────────────────────
def check_health(live, mock):
    print(f"\n{BOLD}{CYN}== E. Health =={RST}")
    for p in ("/healthz", "/readyz", "/livez"):
        lc, _ = live.get(p)
        mc, mb = mock.get(p)
        if mc == 200:
            record("health", f"{p} 200", "PASS", f"live={lc} mock={mc} body={mb.decode()[:16]!r}")
        else:
            record("health", f"{p} 200", "UNEXPECTED", f"mock status={mc}")


# ── F. Error shapes ──────────────────────────────────────────────────────────────
def check_errors(live, mock):
    print(f"\n{BOLD}{CYN}== F. Error shapes =={RST}")
    # not-found for a real, captured resource type
    nf = f"/api/v1/namespaces/{PROBE_NS}/pods/does-not-exist-xyz"
    lc, lb = live.get_json(nf)
    mc, mb = mock.get_json(nf)
    ok = (mc == 404 and isinstance(mb, dict) and mb.get("kind") == "Status"
          and mb.get("status") == "Failure" and mb.get("reason") == "NotFound"
          and mb.get("code") == 404)
    if ok:
        record("errors", "404 not-found Status object", "PASS",
               f"live code={lc} reason={lb.get('reason') if isinstance(lb, dict) else None}")
    else:
        record("errors", "404 not-found Status object", "UNEXPECTED",
               f"mock status={mc} body={mb}")
    # unknown group/version
    ug = "/apis/nonexistent.example.com/v1"
    lc, _ = live.get(ug)
    mc, mb = mock.get_json(ug)
    if mc == 404:
        record("errors", "404 for unknown group/version", "PASS", f"live={lc} mock={mc}")
    else:
        record("errors", "404 for unknown group/version", "UNEXPECTED",
               f"mock status={mc} (live={lc}) body={mb}")


# ── summary ──────────────────────────────────────────────────────────────────────
def summarize():
    if WRITE_BASELINE:
        keys = sorted(f"{cat}::{name}" for cat, name, v, _ in RESULTS if v in ("UNEXPECTED", "ACCEPTED"))
        doc = {
            "_comment": ("Accepted mock<->upstream divergences (issue #136). Each key is "
                         "'category::name' from conformance_diff.py. Entries here are reported "
                         "but do NOT fail the run; the gate fires only on NEW divergences. See "
                         "docs/conformance.md for the rationale. Regenerate with WRITE_BASELINE=1."),
            "accepted": keys,
        }
        with open(BASELINE_PATH, "w") as f:
            json.dump(doc, f, indent=2)
            f.write("\n")
        print(f"\n{YEL}WROTE baseline ({len(keys)} accepted divergences) -> {BASELINE_PATH}{RST}")

    total = len(RESULTS)
    passed = sum(1 for r in RESULTS if r[2] == "PASS")
    expected = sum(1 for r in RESULTS if r[2] == "EXPECTED")
    accepted = sum(1 for r in RESULTS if r[2] == "ACCEPTED")
    unexpected = sum(1 for r in RESULTS if r[2] == "UNEXPECTED")
    print(f"\n{BOLD}{CYN}== Summary =={RST}")
    cats = {}
    for cat, _, verdict, _ in RESULTS:
        d = cats.setdefault(cat, {"PASS": 0, "EXPECTED": 0, "ACCEPTED": 0, "UNEXPECTED": 0})
        d[verdict] += 1
    for cat, d in cats.items():
        print(f"  {cat:10s}  match={d['PASS']:2d}  expected={d['EXPECTED']:2d}  "
              f"accepted={d['ACCEPTED']:2d}  new-divergence={d['UNEXPECTED']:2d}")
    print(f"\n  {BOLD}checks: {total}   "
          f"{GRN}match: {passed}{RST}   "
          f"{YEL}expected: {expected}   accepted (baseline): {accepted}{RST}   "
          f"{RED}new divergences: {unexpected}{RST}")
    # Compatibility score treats expected + accepted diffs as compatible.
    score = (passed + expected + accepted) / total * 100 if total else 0
    print(f"  {BOLD}contract compatibility (match + accepted): {score:.0f}%{RST}")
    if unexpected:
        print(f"\n{RED}{BOLD}NEW divergences (not in baseline — failing the gate):{RST}")
        for cat, name, verdict, _ in RESULTS:
            if verdict == "UNEXPECTED":
                print(f"  - {cat}::{name}")
        print(f"{DIM}  If a divergence is intentional, add its key to "
              f"{os.path.basename(BASELINE_PATH)} (or run WRITE_BASELINE=1).{RST}")
    sys.exit(1 if unexpected else 0)


if __name__ == "__main__":
    main()
