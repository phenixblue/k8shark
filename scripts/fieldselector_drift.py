#!/usr/bin/env python3
"""Detect drift between k8shark's field-label tables and upstream Kubernetes.

internal/store/fieldselector.go hand-transcribes two upstream lists per kind:

  1. the labels a fieldSelector may use — the per-kind funcs registered with
     AddFieldLabelConversionFunc in pkg/apis/<group>/<version>/conversion.go;
  2. the labels that actually resolve to a value — the registry strategy's
     ToSelectableFields in pkg/registry/.../strategy.go.

They are hand-maintained because upstream registers them in
k8s.io/kubernetes's *internal* API packages, which are not reachable from
k8s.io/api or client-go — there is nothing to reflect over at build time.

The conformance differential (scripts/conformance_diff.py, section G) is the
primary drift detector, since it compares behavior against a live apiserver. Its
blind spot is any kind or key the capture does not contain: a KinD capture
usually has no CertificateSigningRequest, so spec.signerName goes unexercised.
This script covers that gap by parsing the upstream source at the Kubernetes
minor version pinned in go.mod and diffing the extracted labels against a
checked-in snapshot.

It is text extraction, so it is brittle to upstream refactors *by design*: a
parse that stops finding a known list is reported as drift to investigate, not
silently ignored.

Usage:
    ./scripts/fieldselector_drift.py            # compare against the snapshot
    ./scripts/fieldselector_drift.py --update   # rewrite the snapshot
    ./scripts/fieldselector_drift.py --version 1.36   # override the k8s minor

Exit codes: 0 no drift, 1 drift found, 2 could not complete the comparison.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.request

PROJ_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SNAPSHOT = os.path.join(PROJ_ROOT, "scripts", "fieldselector-snapshot.json")
RAW = "https://raw.githubusercontent.com/kubernetes/kubernetes/release-{ver}/{path}"

# Files to scrape, and what to pull out of each.
#
# `conversion` entries yield the accepted labels: every string literal appearing
# as a `case "..."` inside an AddFieldLabelConversionFunc body.
# `strategy` entries yield the selectable labels: every quoted key in the
# fields.Set literal built by ToSelectableFields/SelectableFields.
CONVERSION_FILES = [
    "pkg/apis/core/v1/conversion.go",
    "pkg/apis/batch/v1/conversion.go",
    "pkg/apis/certificates/v1/conversion.go",
    "pkg/apis/events/v1/conversion.go",
    "pkg/apis/apps/v1/conversion.go",
]

STRATEGY_FILES = [
    "pkg/registry/core/pod/strategy.go",
    "pkg/registry/core/node/strategy.go",
    "pkg/registry/core/namespace/strategy.go",
    "pkg/registry/core/secret/strategy.go",
    "pkg/registry/core/service/strategy.go",
    "pkg/registry/core/replicationcontroller/strategy.go",
    "pkg/registry/core/event/strategy.go",
    "pkg/registry/batch/job/strategy.go",
    "pkg/registry/certificates/certificates/strategy.go",
]


def k8s_minor_from_gomod() -> str:
    """Read the k8s.io/api version from go.mod and map it to a release branch.

    k8s.io/api v0.36.3 tracks kubernetes release-1.36.
    """
    gomod = os.path.join(PROJ_ROOT, "go.mod")
    try:
        with open(gomod, encoding="utf-8") as f:
            text = f.read()
    except OSError as e:
        raise SystemExit(f"cannot read {gomod}: {e}")
    m = re.search(r"^\s*k8s\.io/api\s+v0\.(\d+)\.\d+", text, re.MULTILINE)
    if not m:
        raise SystemExit("could not find a k8s.io/api v0.<minor>.<patch> require in go.mod")
    return f"1.{m.group(1)}"


def fetch(ver: str, path: str) -> str:
    url = RAW.format(ver=ver, path=path)
    req = urllib.request.Request(url, headers={"User-Agent": "k8shark-fieldselector-drift"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.read().decode("utf-8")
    except urllib.error.HTTPError as e:
        raise SystemExit(f"fetching {url}: HTTP {e.code}")
    except Exception as e:  # noqa: BLE001
        raise SystemExit(f"fetching {url}: {e}")


# AddFieldLabelConversionFunc(SchemeGroupVersion.WithKind("Pod"), func(...) { ... })
#
# Both patterns avoid nesting one quantifier inside another. A grouped repetition
# such as (?:\w+\.)* or (?:"[^"]+"\s*,?\s*)+ backtracks exponentially on input
# that almost-but-never matches, which CodeQL flags as py/redos — and this script
# runs over source fetched from the network.
CONV_START = re.compile(
    r'AddFieldLabelConversionFunc\(\s*(?:\w+\.)?SchemeGroupVersion\.WithKind\(\s*"(\w+)"\s*\)',
)
# The label list of a case clause, up to its colon. Quoted labels are pulled out
# of the captured span separately. [^:]* is greedy but cannot cross a colon, so
# it lands on the first one deterministically — and it spans newlines, which
# matters because upstream wraps long lists:
#
#     case "metadata.name",
#         "spec.signerName":
#
# Stopping at the first ":" is safe because no upstream field label contains one.
CASE_LABELS = re.compile(r"\bcase\b([^:]*):")
# fields.Set{ "key": expr, ... } — capture the quoted keys.
SET_KEY = re.compile(r'"([A-Za-z0-9_.]+)"\s*:')
# Pods build their set imperatively — make(fields.Set, 10) followed by
# podSpecificFieldsSet["spec.nodeName"] = ... — so index assignment counts too.
SET_ASSIGN = re.compile(r'\w+\[\s*"([A-Za-z0-9_.]+)"\s*\]\s*=[^=]')


def brace_block(text: str, start: int) -> str:
    """Return the text from start through the matching close of the first '{'."""
    open_at = text.find("{", start)
    if open_at < 0:
        return ""
    depth = 0
    for i in range(open_at, len(text)):
        if text[i] == "{":
            depth += 1
        elif text[i] == "}":
            depth -= 1
            if depth == 0:
                return text[open_at:i + 1]
    return ""


def extract_accepted(src: str) -> dict[str, list[str]]:
    """Kind -> sorted accepted field labels, from the conversion funcs.

    Two upstream shapes are in use: a `switch label { case "...": }` (core/v1,
    batch/v1, certificates/v1) and a `map[string]string` lookup whose keys are
    the accepted labels (events/v1). The map is declared *before* the
    AddFieldLabelConversionFunc call, so fall back to scanning the enclosing
    function when the call's own body yields no case labels.
    """
    out: dict[str, list[str]] = {}
    for m in CONV_START.finditer(src):
        kind = m.group(1)
        body = brace_block(src, m.end())
        labels: set[str] = set()
        for cm in CASE_LABELS.finditer(body):
            labels.update(re.findall(r'"([^"]+)"', cm.group(1)))
        if not labels:
            labels.update(mapping_keys_in_enclosing_func(src, m.start()))
        if labels:
            out[kind] = sorted(labels)
    return out


def mapping_keys_in_enclosing_func(src: str, call_at: int) -> set[str]:
    """Keys of any map[string]string literal in the func enclosing call_at."""
    start = src.rfind("\nfunc ", 0, call_at)
    if start < 0:
        return set()
    body = brace_block(src, start)
    keys: set[str] = set()
    for mm in re.finditer(r"map\[string\]string\{", body):
        keys.update(SET_KEY.findall(brace_block(body, mm.end() - 1)))
    return keys


def extract_selectable(src: str) -> dict[str, list[str]]:
    """Function name -> sorted selectable field labels, from fields.Set literals.

    Keyed by the enclosing function so a file with several (e.g. a strategy plus
    a helper) stays distinguishable.
    """
    out: dict[str, list[str]] = {}
    for m in re.finditer(r"func\s+(\w*(?:SelectableFields|ToSelectableFields))\s*\(", src):
        name = m.group(1)
        body = brace_block(src, m.end())
        keys = set()
        for sm in re.finditer(r"fields\.Set\{", body):
            keys.update(SET_KEY.findall(brace_block(body, sm.end() - 1)))
        keys.update(SET_ASSIGN.findall(body))
        if keys:
            out[name] = sorted(keys)
    return out


def collect(ver: str) -> dict:
    accepted: dict[str, dict[str, list[str]]] = {}
    for path in CONVERSION_FILES:
        found = extract_accepted(fetch(ver, path))
        # apps/v1 legitimately registers none; record the empty result so the
        # snapshot notices if upstream *adds* one.
        accepted[path] = found
    selectable: dict[str, dict[str, list[str]]] = {}
    for path in STRATEGY_FILES:
        found = extract_selectable(fetch(ver, path))
        if not found:
            # A strategy that suddenly yields nothing means the extraction broke
            # or upstream restructured — either way a human should look.
            print(f"warning: no selectable fields extracted from {path}", file=sys.stderr)
        selectable[path] = found
    return {"kubernetesVersion": ver, "accepted": accepted, "selectable": selectable}


def diff(old: dict, new: dict) -> list[str]:
    problems = []
    for section in ("accepted", "selectable"):
        o, n = old.get(section, {}), new.get(section, {})
        for path in sorted(set(o) | set(n)):
            ov, nv = o.get(path, {}), n.get(path, {})
            for key in sorted(set(ov) | set(nv)):
                before, after = ov.get(key, []), nv.get(key, [])
                if before == after:
                    continue
                added = [x for x in after if x not in before]
                removed = [x for x in before if x not in after]
                bits = []
                if added:
                    bits.append(f"added {added}")
                if removed:
                    bits.append(f"removed {removed}")
                problems.append(f"{section}: {path}:{key}: " + ", ".join(bits))
    return problems


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--update", action="store_true", help="rewrite the snapshot from upstream")
    ap.add_argument("--version", help="Kubernetes minor to scrape (default: from go.mod)")
    args = ap.parse_args()

    ver = args.version or k8s_minor_from_gomod()
    print(f"scraping upstream field-label tables at release-{ver}")
    new = collect(ver)

    if args.update:
        with open(SNAPSHOT, "w", encoding="utf-8") as f:
            json.dump(new, f, indent=2, sort_keys=True)
            f.write("\n")
        print(f"wrote {SNAPSHOT}")
        return 0

    try:
        with open(SNAPSHOT, encoding="utf-8") as f:
            old = json.load(f)
    except FileNotFoundError:
        print(f"no snapshot at {SNAPSHOT}; run with --update to create it", file=sys.stderr)
        return 2

    if old.get("kubernetesVersion") != ver:
        print(f"note: snapshot was taken at release-{old.get('kubernetesVersion')}, "
              f"comparing against release-{ver}")

    problems = diff(old, new)
    if not problems:
        print("no drift: upstream field-label tables match the snapshot")
        return 0

    print("\nDRIFT DETECTED — upstream field-label tables changed:\n", file=sys.stderr)
    for p in problems:
        print(f"  - {p}", file=sys.stderr)
    print("\nUpdate internal/store/fieldselector.go to match, then re-run with "
          "--update to refresh the snapshot.", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
