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
import urllib.error
import urllib.request

LIVE_KC = os.environ["LIVE_KUBECONFIG"]
MOCK_KC = os.environ["MOCK_KUBECONFIG"]
PROBE_NS = os.environ.get("PROBE_NS", "default")
# Optional: write a Markdown report here (for the CI job summary + PR comment).
REPORT_MD = os.environ.get("CONFORMANCE_MD")
# Optional: the tested Kubernetes version, shown in the report.
K8S_VERSION = os.environ.get("K8S_VERSION", "")

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
            data = json.load(f)
    except FileNotFoundError:
        return set()  # no baseline yet — every divergence is "new"
    except (json.JSONDecodeError, OSError) as e:
        # A corrupt baseline (merge conflict, partial write) must fail loudly
        # rather than silently accepting nothing or crashing with a stack trace.
        sys.exit(f"conformance: baseline {BASELINE_PATH} is unreadable/malformed: {e}")
    if not isinstance(data, dict) or not isinstance(data.get("accepted", []), list):
        sys.exit(f"conformance: baseline {BASELINE_PATH} must be an object with an 'accepted' list")
    return set(data.get("accepted", []))


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
    "managedFields", "selfLink",
}


def key_paths(obj, prefix=""):
    """Set of dotted key paths in a JSON object (arrays collapsed to [])."""
    paths = set()
    if isinstance(obj, dict):
        for k, v in obj.items():
            p = f"{prefix}.{k}" if prefix else k
            paths.add(p)
            paths |= key_paths(v, p)
    elif isinstance(obj, list):
        # Union over every element: later items (e.g. extra containers, env
        # entries) can carry keys the first item lacks.
        for item in obj:
            paths |= key_paths(item, prefix + "[]")
    return paths


def strip_volatile_item(item):
    """Drop fields that legitimately drift between capture time and 'now'."""
    if not isinstance(item, dict):
        return item
    meta = item.get("metadata")
    if not isinstance(meta, dict):
        return item
    for k in list(meta):
        if k in VOLATILE_META:
            meta.pop(k, None)
    # Compare annotations at map-presence level only: keep the `annotations`
    # key so a mock that drops the whole map is caught, but don't descend into
    # individual annotation keys, whose set legitimately drifts between capture
    # time and the live 'now' (controllers add revision/last-applied/etc.).
    if isinstance(meta.get("annotations"), dict):
        meta["annotations"] = {}
    # status drifts constantly for live workloads; compare spec/metadata shape only.
    item.pop("status", None)
    return item


# ═══════════════════════════════════════════════════════════════════════════════
def main():
    live = Proxy(LIVE_KC)
    mock = Proxy(MOCK_KC)
    try:
        # Inside the try so a failure in either start() still tears down the
        # other proxy in the finally block.
        live.start()
        mock.start()
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
def parse_group_versions(index):
    """Map group name -> set of groupVersion strings from an APIGroupList,
    ignoring malformed groups/versions so an unexpected shape can't crash the
    harness (it degrades to fewer groups rather than raising)."""
    out = {}
    if not isinstance(index, dict):
        return out
    for g in index.get("groups") or []:
        if not isinstance(g, dict) or not g.get("name"):
            continue
        gvs = {v["groupVersion"] for v in g.get("versions", []) or []
               if isinstance(v, dict) and v.get("groupVersion")}
        out[g["name"]] = gvs
    return out


def check_discovery(live, mock):
    print(f"\n{BOLD}{CYN}== A. Discovery =={RST}")

    # /api (APIVersions) — static envelope; validate the mock's shape.
    _, cm = mock.get_json("/api")
    if isinstance(cm, dict) and cm.get("kind") == "APIVersions" and "v1" in cm.get("versions", []):
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
    live_groups = parse_group_versions(la)
    mock_groups = parse_group_versions(ma)
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
    if not isinstance(mr, dict) or mr.get("kind") != "APIResourceList":
        record("discovery", f"{gv_path} APIResourceList", "UNEXPECTED", f"mock returned: {mr}")
        return
    if not isinstance(lr, dict):
        # Live is the reference; an unreadable live response is one clear
        # divergence, not a cascade of "not present on live" per-resource diffs.
        record("discovery", f"{gv_path} APIResourceList", "UNEXPECTED",
               f"live {gv_path} unreadable ({type(lr).__name__}); cannot compare")
        return
    live_by_name = {r["name"]: r for r in (lr.get("resources") or [])
                    if isinstance(r, dict) and r.get("name")}
    field_diffs, verb_diffs, missing_sub, missing_meta = [], [], [], []
    verbs_reduced = False  # mock replaced live's verbs with a usable read-only subset
    mock_names = set()
    for r in mr.get("resources") or []:
        if not isinstance(r, dict) or not r.get("name"):
            field_diffs.append(f"malformed resource entry: {r!r}")
            continue
        name = r["name"]
        mock_names.add(name)
        lref = live_by_name.get(name)
        if not lref:
            field_diffs.append(f"{name}: not present on live server")
            continue
        # verbs: the mock synthesizes read-only {get,list,watch}; live has the
        # full write set. Observe rather than assume, so this stays honest if
        # the mock ever serves real verbs from a captured discovery document.
        lverbs, mverbs = set(lref.get("verbs", [])), set(r.get("verbs", []))
        if mverbs != lverbs:
            # A legitimate read-only reduction must still be usable: a subset of
            # {get,list,watch} that at least keeps get+list. An empty set or one
            # missing get/list is a real regression, not an "expected" reduction.
            if mverbs <= {"get", "list", "watch"} and {"get", "list"} <= mverbs:
                verbs_reduced = True
            else:
                verb_diffs.append(f"{name}.verbs {sorted(mverbs)} != live {sorted(lverbs)}")
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
    mock_parents = {n.split("/")[0] for n in mock_names}
    for name in live_by_name:
        if "/" in name and name.split("/")[0] in mock_parents and name not in mock_names:
            missing_sub.append(name)

    if field_diffs:
        record("discovery", f"{gv_path} resource fields", "UNEXPECTED", "\n".join(field_diffs))
    else:
        record("discovery", f"{gv_path} resource fields (kind/namespaced/singular/shortNames)", "PASS")
    if missing_sub:
        # Subresources are in-scope: a mock that drops them is a real regression
        # that should gate. Baseline it explicitly if a specific omission is
        # ever intentional, rather than treating all omissions as expected.
        record("discovery", f"{gv_path} subresources", "UNEXPECTED",
               "mock omits subresource entries live advertises: " + ", ".join(sorted(missing_sub)))
    else:
        record("discovery", f"{gv_path} subresources", "PASS")
    if missing_meta:
        record("discovery", f"{gv_path} resource metadata", "EXPECTED",
               "mock omits live-only fields: " + ", ".join(sorted(missing_meta)))
    else:
        record("discovery", f"{gv_path} resource metadata", "PASS")
    # verbs — always exactly one record so the check count stays stable.
    if verb_diffs:
        record("discovery", f"{gv_path} verbs", "UNEXPECTED", "\n".join(verb_diffs))
    elif verbs_reduced:
        record("discovery", f"{gv_path} verbs", "EXPECTED",
               "mock advertises a read-only verb subset ({get,list,watch}); live advertises full write verbs")
    else:
        record("discovery", f"{gv_path} verbs", "PASS")


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
    else:
        record("version", "/version major/minor", "PASS")


# ── C. OpenAPI ───────────────────────────────────────────────────────────────────
def check_openapi(live, mock):
    print(f"\n{BOLD}{CYN}== C. OpenAPI =={RST}")
    # /openapi/v2 — the live apiserver is the reference; validate it first so an
    # unreadable live response is a divergence rather than a silent mock PASS.
    lc, lb = live.get_json("/openapi/v2")
    mc, mb = mock.get_json("/openapi/v2")
    if lc != 200 or not isinstance(lb, dict) or "swagger" not in lb:
        record("openapi", "/openapi/v2 served", "UNEXPECTED",
               f"live /openapi/v2 unreadable (status={lc}); cannot compare")
    elif mc == 200 and isinstance(mb, dict) and "swagger" in mb:
        real = bool(mb.get("definitions"))
        record("openapi", "/openapi/v2 served", "PASS" if real else "EXPECTED",
               f"live status={lc}, mock served full spec" if real else
               "mock serves a minimal stub (no definitions) when the capture lacks OpenAPI")
    else:
        record("openapi", "/openapi/v2 served", "UNEXPECTED", f"mock status={mc} (live=200)")

    # /openapi/v3 — compare the index symmetrically, validating the body shape
    # (a valid index is a JSON object with a "paths" map), not just the status.
    lc3, l3 = live.get_json("/openapi/v3")
    mc3, m3 = mock.get_json("/openapi/v3")
    if lc3 != 200 or not isinstance(l3, dict) or "paths" not in l3:
        record("openapi", "/openapi/v3 served", "UNEXPECTED",
               f"live /openapi/v3 unreadable/invalid (status={lc3}); cannot compare")
    elif mc3 == 200 and isinstance(m3, dict) and "paths" in m3:
        record("openapi", "/openapi/v3 served", "PASS", "live=mock=200 (valid index)")
    elif mc3 == 200:
        record("openapi", "/openapi/v3 served", "UNEXPECTED",
               f"mock returned 200 but not a valid v3 index (type={type(m3).__name__})")
    else:
        record("openapi", "/openapi/v3 served", "EXPECTED",
               f"mock returns {mc3} (live=200); only served when captured")

    # /openapi/v3/<gv> — exercise a representative per-GV document, not just the
    # index. Use each server's own index URL (the ?hash= differs per server).
    lpaths = l3.get("paths") if isinstance(l3, dict) else {}
    mpaths = m3.get("paths") if isinstance(m3, dict) else {}
    if isinstance(lpaths, dict) and isinstance(mpaths, dict) and lpaths and mpaths:
        common = set(lpaths) & set(mpaths)
        sample = next((gv for gv in ("api/v1", "apis/apps/v1") if gv in common),
                      next(iter(sorted(common)), None))
        if not sample:
            record("openapi", "/openapi/v3 per-GV document", "EXPECTED",
                   "no group-version common to both /openapi/v3 indexes to sample")
        else:
            lurl = (lpaths[sample] or {}).get("serverRelativeURL", f"/openapi/v3/{sample}")
            murl = (mpaths[sample] or {}).get("serverRelativeURL", f"/openapi/v3/{sample}")
            lcd, ld = live.get_json(lurl)
            mcd, md = mock.get_json(murl)
            if lcd != 200 or not isinstance(ld, dict) or "openapi" not in ld:
                record("openapi", "/openapi/v3 per-GV document", "UNEXPECTED",
                       f"live {lurl} unreadable (status={lcd}); cannot compare")
            elif mcd == 200 and isinstance(md, dict) and ("openapi" in md or "components" in md):
                record("openapi", "/openapi/v3 per-GV document", "PASS", f"sampled {sample}: live=mock=200")
            elif mcd != 200:
                record("openapi", "/openapi/v3 per-GV document", "EXPECTED",
                       f"mock returns {mcd} for {sample} (live=200); per-GV doc only served when captured")
            else:
                shape = list(md.keys())[:8] if isinstance(md, dict) else repr(md)[:80]
                record("openapi", "/openapi/v3 per-GV document", "UNEXPECTED",
                       f"mock {sample} doc shape unexpected (status={mcd}): {shape}")


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
        lc, lb = live.get_json(path)
        code, mb = mock.get_json(path)
        # Live is the reference: if it can't be read, flag rather than compare.
        if lc != 200 or not isinstance(lb, dict) or lb.get("kind") != want_kind:
            record("reads", f"{path} list envelope", "UNEXPECTED",
                   f"live {path} unreadable (status={lc}, kind={lb.get('kind') if isinstance(lb, dict) else None}); cannot compare")
            continue
        if not isinstance(mb, dict) or mb.get("kind") != want_kind:
            record("reads", f"{path} list envelope", "UNEXPECTED",
                   f"mock status={code} kind={mb.get('kind') if isinstance(mb, dict) else None} want {want_kind}")
            continue
        env_ok = mb.get("apiVersion") == lb.get("apiVersion") and \
            isinstance(mb.get("metadata"), dict) and "resourceVersion" in mb["metadata"] and \
            isinstance(mb.get("items"), list)
        record("reads", f"{path} list envelope", "PASS" if env_ok else "UNEXPECTED",
               "" if env_ok else f"apiVersion/metadata.resourceVersion/items mismatch: keys={list(mb.keys())}")
        # per-item structural comparison for an object present in both
        compare_item_structure(path, lb, mb)

    # Single-object GET (not just LIST) for a representative resource.
    list_path = f"/api/v1/namespaces/{PROBE_NS}/pods"
    _, lb = live.get_json(list_path)
    _, mb = mock.get_json(list_path)
    name = first_common_name(lb, mb)
    if not name:
        record("reads", "single-object GET envelope", "EXPECTED", "no common object to GET")
    else:
        obj_path = f"{list_path}/{name}"
        lc, lo = live.get_json(obj_path)
        mc, mo = mock.get_json(obj_path)
        if lc != 200 or not isinstance(lo, dict):
            record("reads", "single-object GET envelope", "UNEXPECTED",
                   f"live GET {obj_path} unreadable (status={lc}); cannot compare")
        elif (mc == 200 and isinstance(mo, dict) and mo.get("kind") == lo.get("kind")
              and mo.get("apiVersion") == lo.get("apiVersion")
              and isinstance(mo.get("metadata"), dict) and mo["metadata"].get("name") == name):
            record("reads", "single-object GET envelope", "PASS",
                   f"GET {obj_path}: kind={mo.get('kind')} apiVersion={mo.get('apiVersion')}")
        else:
            record("reads", "single-object GET envelope", "UNEXPECTED",
                   f"mock GET status={mc} kind={mo.get('kind') if isinstance(mo, dict) else None} "
                   f"apiVersion={mo.get('apiVersion') if isinstance(mo, dict) else None} "
                   f"want kind={lo.get('kind')} apiVersion={lo.get('apiVersion')}")


def named_items(container):
    """Yield (name, item) for well-formed list items with a metadata.name,
    ignoring malformed entries so upstream format drift can't crash the harness."""
    if not isinstance(container, dict):
        return
    for i in container.get("items") or []:
        if isinstance(i, dict) and isinstance(i.get("metadata"), dict):
            name = i["metadata"].get("name")
            if name:
                yield name, i


def first_common_name(lb, mb):
    """Name of the first object present in both the live and mock list items."""
    live_names = {name for name, _ in named_items(lb)}
    for name, _ in named_items(mb):
        if name in live_names:
            return name
    return None


def compare_item_structure(path, lb, mb):
    live_items = dict(named_items(lb))
    for name, mi in named_items(mb):
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
    # No object present in both lists: record explicitly rather than silently
    # skipping, so a PASS on the list envelope can't imply a structural check ran.
    if not live_items:
        record("reads", f"{path} item structure", "EXPECTED",
               "live list is empty; no object to compare structurally")
    else:
        record("reads", f"{path} item structure", "UNEXPECTED",
               "live has items but none are present in the mock list")


# ── E. Health ────────────────────────────────────────────────────────────────────
def check_health(live, mock):
    print(f"\n{BOLD}{CYN}== E. Health =={RST}")
    for p in ("/healthz", "/readyz", "/livez"):
        lc, _ = live.get(p)
        mc, mb = mock.get(p)
        # Symmetric: the live apiserver is the reference. A non-200 from live is
        # an environmental problem, not a mock match.
        if lc != 200:
            record("health", f"{p} 200", "UNEXPECTED", f"live {p} status={lc} (mock={mc}); cannot compare")
        elif mc == 200:
            record("health", f"{p} 200", "PASS", f"live=mock=200 body={mb.decode()[:16]!r}")
        else:
            record("health", f"{p} 200", "UNEXPECTED", f"mock status={mc} (live=200)")


# ── F. Error shapes ──────────────────────────────────────────────────────────────
def check_errors(live, mock):
    print(f"\n{BOLD}{CYN}== F. Error shapes =={RST}")
    # not-found for a real, captured resource type. Confirm live returns the
    # canonical 404 Status shape first, then require the mock to match it.
    nf = f"/api/v1/namespaces/{PROBE_NS}/pods/does-not-exist-xyz"
    lc, lb = live.get_json(nf)
    mc, mb = mock.get_json(nf)
    live_ok = (lc == 404 and isinstance(lb, dict) and lb.get("kind") == "Status"
               and lb.get("reason") == "NotFound")
    if not live_ok:
        record("errors", "404 not-found Status object", "UNEXPECTED",
               f"live not-found shape unexpected: status={lc} body={lb}; cannot compare")
    else:
        mock_ok = (mc == 404 and isinstance(mb, dict) and mb.get("kind") == "Status"
                   and mb.get("status") == "Failure" and mb.get("reason") == "NotFound"
                   and mb.get("code") == 404)
        record("errors", "404 not-found Status object", "PASS" if mock_ok else "UNEXPECTED",
               f"live reason={lb.get('reason')}" if mock_ok else f"mock status={mc} body={mb}")
    # unknown group/version — live must 404; the mock should match.
    ug = "/apis/nonexistent.example.com/v1"
    lc, _ = live.get(ug)
    mc, mb = mock.get_json(ug)
    if lc != 404:
        record("errors", "404 for unknown group/version", "UNEXPECTED",
               f"live returned status={lc} for unknown group (expected 404); cannot compare")
    elif mc == 404:
        record("errors", "404 for unknown group/version", "PASS", "live=mock=404")
    else:
        record("errors", "404 for unknown group/version", "UNEXPECTED",
               f"mock status={mc} (live=404) body={mb}")


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
    print(f"  {BOLD}contract compatibility (match + expected + accepted): {score:.0f}%{RST}")
    if unexpected:
        print(f"\n{RED}{BOLD}NEW divergences (not in baseline — failing the gate):{RST}")
        for cat, name, verdict, _ in RESULTS:
            if verdict == "UNEXPECTED":
                print(f"  - {cat}::{name}")
        print(f"{DIM}  If a divergence is intentional, add its key to "
              f"{os.path.basename(BASELINE_PATH)} (or run WRITE_BASELINE=1).{RST}")

    if REPORT_MD:
        write_markdown(REPORT_MD, cats, total, passed, expected, accepted, unexpected, score)
        print(f"\n{DIM}wrote markdown report -> {REPORT_MD}{RST}")
    # When refreshing the baseline, the observed divergences are exactly what we
    # just wrote into it, so exit success to keep refresh workflows simple.
    sys.exit(0 if WRITE_BASELINE else (1 if unexpected else 0))


def write_markdown(path, cats, total, passed, expected, accepted, unexpected, score):
    """Emit a Markdown report for the CI job summary and the sticky PR comment.

    The leading HTML marker lets the workflow find and update its own comment
    instead of posting a new one each run.
    """
    status = "❌ new divergences" if unexpected else "✅ no new divergences"
    lines = [
        "<!-- conformance-report -->",
        "## Mock ↔ upstream conformance",
        "",
        f"Differential comparison of the `kshrk open` mock server against a live "
        f"apiserver{f' (**{K8S_VERSION}**)' if K8S_VERSION else ''}.",
        "",
        f"**Contract compatibility: {score:.0f}%** &nbsp;·&nbsp; {status}",
        "",
        "| Category | Match | Expected diff | Accepted (baseline) | New divergence |",
        "|----------|------:|--------------:|--------------------:|---------------:|",
    ]
    for cat, d in cats.items():
        lines.append(f"| {cat} | {d['PASS']} | {d['EXPECTED']} | {d['ACCEPTED']} | {d['UNEXPECTED']} |")
    lines.append(f"| **total** | **{passed}** | **{expected}** | **{accepted}** | **{unexpected}** |")
    lines.append("")

    new_divs = [f"{cat}::{name}" for cat, name, v, _ in RESULTS if v == "UNEXPECTED"]
    if new_divs:
        lines += [
            "### ❌ New divergences (failing the gate)",
            "These are not in `scripts/conformance-baseline.json`. If intentional, "
            "add the key to the baseline (or run `WRITE_BASELINE=1`).",
            "",
        ]
        lines += [f"- `{d}`" for d in new_divs]
        lines.append("")

    accepted_divs = [f"{cat}::{name}" for cat, name, v, _ in RESULTS if v == "ACCEPTED"]
    if accepted_divs:
        lines += ["<details><summary>Accepted divergences (documented baseline)</summary>", ""]
        lines += [f"- `{d}`" for d in accepted_divs]
        lines += ["", "</details>", ""]

    lines.append(f"<sub>{total} checks · generated by `scripts/conformance.sh`</sub>")
    with open(path, "w") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    main()
