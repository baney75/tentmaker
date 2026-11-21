#!/usr/bin/env python3
"""Markdown table of every metrics/<scenario>-<os>.json, for the job summary.

    summary.py METRICS_DIR [EXPECTED...]

EXPECTED names "<scenario>-<os>" pairs that must have a file; a missing one
is a row marked "no metrics" (the job died before writing them).
Exits 1 when any row did not pass.
"""

import glob
import json
import os
import sys


def short(v):
    if isinstance(v, float):
        return "%g" % v
    if isinstance(v, (list, dict)):
        s = json.dumps(v, separators=(",", ":"))
    else:
        s = str(v)
    s = s.replace("|", "\\|").replace("\n", " ")
    return s if len(s) <= 120 else s[:117] + "..."


def main():
    d = sys.argv[1]
    rows = {}
    for f in sorted(glob.glob(os.path.join(d, "**", "*.json"), recursive=True)):
        try:
            with open(f) as fh:
                m = json.load(fh)
            rows["%s-%s" % (m["scenario"], m["os"])] = m
        except (OSError, ValueError, KeyError) as e:
            print("skipped %s: %s" % (f, e), file=sys.stderr)
    for want in sys.argv[2:]:
        if want not in rows:
            scn, _, os_ = want.rpartition("-")
            rows[want] = {"scenario": scn, "os": os_, "pass": False, "measurements": {}, "failures": ["no metrics"]}
    ok = True
    out = ["## Tentmaker scenarios", "", "| scenario | os | result | measurements | failures |", "| --- | --- | --- | --- | --- |"]
    for k in sorted(rows):
        m = rows[k]
        ok = ok and bool(m.get("pass"))
        meas = "<br>".join("%s: %s" % (a, short(b)) for a, b in m.get("measurements", {}).items())
        fails = "<br>".join(short(x) for x in m.get("failures", []))
        out.append("| %s | %s | %s | %s | %s |" % (m["scenario"], m["os"], "pass" if m.get("pass") else "FAIL", meas, fails))
    print("\n".join(out))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
