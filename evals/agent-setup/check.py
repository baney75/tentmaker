#!/usr/bin/env python3
"""Ratchet for the agent setup eval: fail when a run scores below the baseline.

usage: python3 evals/agent-setup/check.py RUN_DIR [--baseline evals/agent-setup/baseline.json]

A run passes when, for each responder in the baseline, its mean is at least
the baseline mean minus the tolerance, and the bar in baseline.json still
holds (minimum means, and the most forbidden items a responder may commit).
Per-case changes print for reading; only the means and the bar decide.
"""
import argparse
import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run_dir")
    ap.add_argument("--baseline", default=str(HERE / "baseline.json"))
    args = ap.parse_args()
    base = json.loads(Path(args.baseline).read_text())
    path = Path(args.run_dir) / "summary.json"
    if not path.exists():
        sys.exit(f"{path} is missing: finish the run first (run.py run --stamp ... --phase judge)")
    run = json.loads(path.read_text())
    tol = base["tolerance"]
    problems = []
    for m, b in base["responders"].items():
        r = run["responders"].get(m)
        if r is None:
            problems.append(f"{m}: not in this run")
            continue
        floor = round(b["mean"] - tol, 3)
        print(f"{m}: mean {r['mean']:.3f} (baseline {b['mean']:.3f}, floor {floor:.3f}), "
              f"forbidden {r['forbidden']}")
        if r["mean"] < floor:
            problems.append(f"{m}: mean {r['mean']:.3f} is below the floor {floor:.3f}")
        bar = base["bar"].get(m, {})
        if "min_mean" in bar and r["mean"] < bar["min_mean"]:
            problems.append(f"{m}: mean {r['mean']:.3f} is below the bar {bar['min_mean']}")
        if "max_forbidden" in bar and r["forbidden"] > bar["max_forbidden"]:
            problems.append(f"{m}: {r['forbidden']} forbidden item(s), the bar allows {bar['max_forbidden']}")
        for cid, bc in b["cases"].items():
            rc = r["cases"].get(cid)
            if rc and rc["mean"] < bc["mean"]:
                print(f"  {cid}: {rc['mean']:.2f} < {bc['mean']:.2f}"
                      + (f" missed {','.join(rc['missed'])}" if rc["missed"] else "")
                      + (f" forbidden {','.join(rc['forbidden'])}" if rc["forbidden"] else ""))
    if problems:
        print("\nFAIL\n" + "\n".join("- " + p for p in problems))
        print(f"Read {Path(args.run_dir) / 'judge'} for the judge's evidence, then fix the docs.")
        return 1
    print("\nPASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
