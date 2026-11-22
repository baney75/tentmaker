#!/usr/bin/env python3
"""Blind head-to-head eval: can a cheap model set up Tentmaker from the docs alone?

Responders (Haiku and Sonnet) see only README.md and skills/tentmaker/SKILL.md
and answer each request in cases.yaml with the commands they would run and the
questions they would ask. A blind Opus judge grades the shuffled, unlabeled
replies for a case against its answer key, with docs/CLI.md and the installer
help as ground truth.

usage:
  python3 evals/agent-setup/run.py verify            parse-check every answer key (writes verify.json)
  python3 evals/agent-setup/run.py run [options]     respond, judge and summarize one run
      --stamp S        run directory name under runs/ (default: UTC time); rerun to resume
      --phase P        respond, judge or all (default all)
      --docs DIR       replay with pinned docs from an earlier run's DIR (default: the repo's)
      --samples N      replies per responder per case (default 2)
      --only id,id     only these cases
      --jobs N         parallel claude calls (default 8)
  python3 evals/agent-setup/run.py rejudge RUN_DIR --stamp S
                                                     grade RUN_DIR's replies again with today's cases.yaml
  python3 evals/agent-setup/run.py report RUN_DIR    print the per-case table again

Every model call is `env -u ANTHROPIC_API_KEY claude -p` with no tools, no
settings, no MCP servers and no saved session, so it runs on the signed-in
Claude plan and never on API billing. A quota, rate limit or sign-in error
stops the run; nothing switches billing. Needs PyYAML and Go (for verify).
"""
import argparse
import concurrent.futures as cf
import datetime
import json
import os
import random
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

try:
    import yaml
except ImportError:
    sys.exit("run.py needs PyYAML: python3 -m pip install pyyaml")

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent.parent
RUNS = HERE / "runs"
RESPONDERS = ("haiku", "sonnet")
JUDGE_MODEL = "opus"
PENALTY = 0.25  # per forbidden item a reply commits
BASE = ["env", "-u", "ANTHROPIC_API_KEY", "claude", "-p", "--tools", "", "--setting-sources", "",
        "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}', "--no-session-persistence",
        "--output-format", "json"]
RESPONDER_ROLE = ("You are an agent helping a person; reply with the exact commands you'd run "
                  "and anything you'd ask them first.")
# Text in a reply or an error that means the plan is out of quota or signed out.
STOP_RE = re.compile(r"usage limit|rate limit|quota|credit balance|/login|not logged in|sign in again|"
                     r"invalid api key|api billing|out of extra usage", re.I)


class Stop(Exception):
    """A quota or sign-in problem: stop the run and report it."""


def load_cases():
    return yaml.safe_load((HERE / "cases.yaml").read_text())


# ---------------------------------------------------------------- verify

def build_binary(dest):
    subprocess.run(["go", "build", "-o", str(dest), "./cmd/tentmaker"], cwd=ROOT, check=True)


def verify(_args):
    """Run every verify entry and record what it proves."""
    if hasattr(os, "geteuid") and os.geteuid() == 0:
        sys.exit("run verify as a normal user: the checks rely on the root check stopping each command")
    data = load_cases()
    tmp = Path(tempfile.mkdtemp(prefix="tm-verify-"))
    bin_path = tmp / "tentmaker"
    build_binary(bin_path)
    cfg = tmp / "config.toml"
    cfg.write_text('device = "eval"\n\n[[repos]]\nname = "acme/webapp"\n')
    repo = tmp / "repo"
    (repo / ".github" / "workflows").mkdir(parents=True)
    (repo / ".github" / "workflows" / "ci.yml").write_text(
        "on: [push, pull_request]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n"
        "      - uses: actions/checkout@v4\n      - run: make test\n")
    env = dict(os.environ, TENTMAKER_CONFIG=str(cfg))
    env.pop("ANTHROPIC_API_KEY", None)
    results, bad = [], 0
    for case in data["cases"]:
        for i in case.get("required", []):
            if i.get("regex"):
                hint(i["regex"])  # a bad hint fails here, not mid-run
        for v in case.get("verify", []):
            rec = {"case": case["id"]}
            if "grep" in v:
                text = (ROOT / v["grep"]).read_text()
                ok = re.search(v["match"], text) is not None
                rec.update(check="grep " + v["grep"], match=v["match"], ok=ok)
            else:
                if "script" in v:
                    cmd = ["sh"] + [str(ROOT / v["script"][0])] + v["script"][1:]
                    shown = "sh " + " ".join(v["script"])
                else:
                    args = [a.replace("{repo}", str(repo)) for a in v["args"]]
                    cmd = [str(bin_path)] + args
                    shown = "tentmaker " + " ".join(v["args"])
                p = subprocess.run(cmd, input=v.get("stdin", ""), capture_output=True, text=True,
                                   timeout=60, env=env, cwd=tmp)
                out = p.stdout + p.stderr
                want = v.get("expect_any", [v.get("expect", 0)])
                ok = p.returncode in want and (not v.get("match") or re.search(v["match"], out) is not None)
                rec.update(check=shown, exit=p.returncode, expect=want, match=v.get("match"), ok=ok,
                           output=out.strip().replace(str(tmp), "$TMP")[:300])
            bad += not rec["ok"]
            results.append(rec)
            print(("ok   " if rec["ok"] else "FAIL ") + rec["case"] + ": " + rec["check"]
                  + (f" -> exit {rec['exit']}" if "exit" in rec else ""))
    shutil.rmtree(tmp, ignore_errors=True)
    commit = git("rev-parse", "HEAD")
    (HERE / "verify.json").write_text(json.dumps({"commit": commit, "checks": results}, indent=1) + "\n")
    print(f"\n{len(results) - bad}/{len(results)} checks pass; wrote verify.json")
    return 1 if bad else 0


# ---------------------------------------------------------------- claude

def claude(model, system_file, prompt, cwd):
    cmd = BASE + ["--model", model, "--system-prompt-file", str(system_file)]
    try:
        p = subprocess.run(cmd, input=prompt, capture_output=True, text=True, timeout=900, cwd=cwd)
    except subprocess.TimeoutExpired:
        raise RuntimeError(f"{model}: claude -p timed out")
    try:
        d = json.loads(p.stdout)
    except json.JSONDecodeError:
        msg = (p.stdout + p.stderr).strip()[:500]
        if STOP_RE.search(msg):
            raise Stop(f"{model}: {msg}")
        raise RuntimeError(f"{model}: exit {p.returncode}: {msg}")
    text = d.get("result") or ""
    if d.get("is_error") or p.returncode != 0:
        if STOP_RE.search(text + p.stderr):
            raise Stop(f"{model}: {text or p.stderr}"[:500])
        raise RuntimeError(f"{model}: {text or p.stderr}"[:500])
    return text, sorted(d.get("modelUsage", {}).keys())


def git(*args):
    return subprocess.run(["git", *args], cwd=ROOT, capture_output=True, text=True).stdout.strip()


# ---------------------------------------------------------------- run

def pin_docs(run_dir, src):
    """Copy the doc texts this run uses, so it can be replayed exactly."""
    d = run_dir / "docs"
    if d.exists():
        return d
    d.mkdir(parents=True)
    if src:
        for f in Path(src).iterdir():
            shutil.copy(f, d / f.name)
        return d
    shutil.copy(ROOT / "README.md", d / "README.md")
    shutil.copy(ROOT / "skills/tentmaker/SKILL.md", d / "SKILL.md")
    shutil.copy(ROOT / "docs/CLI.md", d / "CLI.md")
    shutil.copy(HERE / "cases.yaml", d / "cases.yaml")
    sh = subprocess.run(["sh", str(ROOT / "scripts/install.sh"), "--help"], capture_output=True, text=True).stdout
    ps = (ROOT / "scripts/install.ps1").read_text()
    ps_help = ps[: ps.find("#>") + 2] if "#>" in ps else ps[:3000]
    (d / "installers.txt").write_text("install.sh --help:\n\n" + sh + "\n\ninstall.ps1 help:\n\n" + ps_help + "\n")
    return d


def responder_system(docs):
    return (RESPONDER_ROLE + "\n\n<file path=\"README.md\">\n" + (docs / "README.md").read_text()
            + "\n</file>\n\n<file path=\"skills/tentmaker/SKILL.md\">\n" + (docs / "SKILL.md").read_text()
            + "\n</file>\n")


def responder_prompt(case):
    return (case["request"].strip() + "\n\n[What you can see on the machine: " + case["context"].strip() + "]")


JUDGE_SYSTEM = "You grade agent replies blind against an answer key. You output one JSON object and nothing else."

JUDGE = """Grade each reply below against the answer key. The replies come from different AI agents in random
order; judge only the text. Each agent had no tools: its reply is the plan it would carry out, so a command it
says it would run counts as run, and a question it says it would ask first counts as asked.

Ground truth for what exists: the CLI contract and the installer help below. Any tentmaker command,
subcommand, flag or flag value, or installer flag or environment variable, that is not in them is made up.

<cli-contract>
{cli}
</cli-contract>

<installers>
{installers}
</installers>

The person's request:
<<<
{request}
>>>
What the agent could see: {context}

Answer key. A reply MEETS an item when its plan does what the item says (alternatives the item names count).
{items}

FORBIDDEN items. A reply COMMITS one when it does or tells the person to do it; mentioning it only to warn
against it does not count. Global:
{global_forbidden}
For this case:
{forbidden}

Replies:
{replies}

Return only this JSON, one entry per reply label:
{{"R1": {{"met": ["item id", ...], "forbidden": [{{"id": "forbidden id", "evidence": "short quote"}}],
"reason": "under 40 words"}}, ...}}"""


def fmt_items(items):
    return "\n".join(f"- {i['id']}: {i['what'].strip()}" for i in items) or "- (none)"


def judge_case(case, data, docs, replies, cwd, run_dir):
    """replies: list of (key, text). Returns {key: verdict}."""
    order = replies[:]
    random.shuffle(order)
    labels = {f"R{n + 1}": key for n, (key, _) in enumerate(order)}
    body = "\n\n".join(f"Reply R{n + 1}:\n<<<\n{text.strip()}\n>>>" for n, (_, text) in enumerate(order))
    items = case.get("required", []) + case.get("questions", [])
    prompt = JUDGE.format(
        cli=(docs / "CLI.md").read_text(), installers=(docs / "installers.txt").read_text(),
        request=case["request"].strip(), context=case["context"].strip(),
        items="Required:\n" + fmt_items(case.get("required", [])) + "\nMust ask the person first:\n"
        + fmt_items(case.get("questions", [])),
        global_forbidden=fmt_items(data["global_forbidden"]), forbidden=fmt_items(case.get("forbidden", [])),
        replies=body)
    sysf = run_dir / "judge-system.txt"
    sysf.write_text(JUDGE_SYSTEM)
    last = None
    for _ in range(2):
        text, models = claude(JUDGE_MODEL, sysf, prompt, cwd)
        try:
            raw = json.loads(text[text.find("{"): text.rfind("}") + 1])
            break
        except json.JSONDecodeError as e:
            last = e
    else:
        raise RuntimeError(f"judge output for {case['id']} is not JSON: {last}")
    item_ids = {i["id"] for i in items}
    forb_ids = {i["id"] for i in data["global_forbidden"]} | {i["id"] for i in case.get("forbidden", [])}
    out = {}
    for label, key in labels.items():
        v = raw.get(label, {})
        met = sorted(set(v.get("met", [])) & item_ids)
        forb = [f for f in v.get("forbidden", []) if isinstance(f, dict) and f.get("id") in forb_ids]
        uniq = {f["id"] for f in forb}
        score = max(0.0, min(1.0, (len(met) / len(item_ids) if item_ids else 1.0) - PENALTY * len(uniq)))
        out[key] = {"label": label, "met": met, "missed": sorted(item_ids - set(met)), "forbidden": forb,
                    "score": round(score, 3), "reason": v.get("reason", "")}
    return out, models


def hint(pattern):
    """A key's regex hint, case-insensitive wherever it says (?i)."""
    return re.compile(pattern.replace("(?i)", ""), re.I)


def regex_hits(case, text):
    return {i["id"]: bool(hint(i["regex"]).search(text)) for i in case.get("required", []) if i.get("regex")}


def run(args):
    data = load_cases()
    stamp = args.stamp or datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    run_dir = RUNS / stamp
    (run_dir / "replies").mkdir(parents=True, exist_ok=True)
    (run_dir / "judge").mkdir(exist_ok=True)
    docs = pin_docs(run_dir, args.docs)
    # The run's own pinned cases are the key, so a replay grades the same way.
    data = yaml.safe_load((docs / "cases.yaml").read_text())
    cases = [c for c in data["cases"] if not args.only or c["id"] in args.only.split(",")]
    meta = run_dir / "meta.json"
    if not meta.exists():
        meta.write_text(json.dumps({
            "stamp": stamp, "commit": git("rev-parse", "HEAD"), "dirty": bool(git("status", "--porcelain")),
            "claude": subprocess.run(["claude", "--version"], capture_output=True, text=True).stdout.strip(),
            "responders": RESPONDERS, "judge": JUDGE_MODEL, "samples": args.samples, "penalty": PENALTY,
            "docs_from": args.docs or "repo"}, indent=1) + "\n")
    sysf = run_dir / "responder-system.txt"
    sysf.write_text(responder_system(docs))
    cwd = tempfile.mkdtemp(prefix="tm-eval-")  # empty: no CLAUDE.md or settings to pick up

    if args.phase in ("respond", "all"):
        todo = [(c, m, k) for c in cases for m in RESPONDERS for k in range(args.samples)
                if not (run_dir / "replies" / f"{c['id']}.{m}.{k}.json").exists()]
        print(f"responding: {len(todo)} calls", flush=True)

        def one(job):
            c, m, k = job
            text, models = claude(m, sysf, responder_prompt(c), cwd)
            (run_dir / "replies" / f"{c['id']}.{m}.{k}.json").write_text(json.dumps(
                {"case": c["id"], "responder": m, "sample": k, "models": models, "text": text}, indent=1) + "\n")
            return f"{c['id']}.{m}.{k}"
        with cf.ThreadPoolExecutor(args.jobs) as ex:
            for f in cf.as_completed([ex.submit(one, j) for j in todo]):
                print("  replied", f.result(), flush=True)

    if args.phase in ("judge", "all"):
        todo = [c for c in cases if not (run_dir / "judge" / f"{c['id']}.json").exists()]
        print(f"judging: {len(todo)} cases", flush=True)

        def judge_one(c):
            replies = []
            for m in RESPONDERS:
                for k in range(args.samples):
                    p = run_dir / "replies" / f"{c['id']}.{m}.{k}.json"
                    replies.append((f"{m}.{k}", json.loads(p.read_text())["text"]))
            verdicts, models = judge_case(c, data, docs, replies, cwd, run_dir)
            for key, text in replies:
                verdicts[key]["regex"] = regex_hits(c, text)
            (run_dir / "judge" / f"{c['id']}.json").write_text(json.dumps(
                {"case": c["id"], "judge_models": models, "verdicts": verdicts}, indent=1) + "\n")
            return c["id"]
        with cf.ThreadPoolExecutor(args.jobs) as ex:
            for f in cf.as_completed([ex.submit(judge_one, c) for c in todo]):
                print("  judged", f.result(), flush=True)
        summarize(run_dir, data)
    shutil.rmtree(cwd, ignore_errors=True)
    return 0


def rejudge(args):
    """Copy a run's replies and pinned docs, swap in the current answer key, and judge."""
    src = Path(args.run_dir)
    dst = RUNS / args.stamp
    if dst.exists():
        sys.exit(f"{dst} exists; pick another --stamp")
    shutil.copytree(src / "docs", dst / "docs")
    shutil.copy(HERE / "cases.yaml", dst / "docs" / "cases.yaml")
    shutil.copytree(src / "replies", dst / "replies")
    meta = json.loads((src / "meta.json").read_text())
    meta.update(stamp=args.stamp, rejudged_from=src.name, key_commit=git("rev-parse", "HEAD"))
    (dst / "meta.json").write_text(json.dumps(meta, indent=1) + "\n")
    ns = argparse.Namespace(stamp=args.stamp, phase="judge", docs=None, samples=meta["samples"],
                            only=None, jobs=args.jobs)
    return run(ns)


def summarize(run_dir, data=None):
    data = data or yaml.safe_load((run_dir / "docs" / "cases.yaml").read_text())
    summary = {"run": run_dir.name, "responders": {}}
    for m in RESPONDERS:
        cases, scores, forb = {}, [], 0
        for c in data["cases"]:
            p = run_dir / "judge" / f"{c['id']}.json"
            if not p.exists():
                continue
            vs = [v for k, v in json.loads(p.read_text())["verdicts"].items() if k.startswith(m + ".")]
            s = [v["score"] for v in vs]
            cases[c["id"]] = {"mean": round(sum(s) / len(s), 3), "scores": s,
                              "missed": sorted({x for v in vs for x in v["missed"]}),
                              "forbidden": sorted({f["id"] for v in vs for f in v["forbidden"]})}
            scores += s
            forb += sum(len(v["forbidden"]) for v in vs)
        summary["responders"][m] = {"mean": round(sum(scores) / len(scores), 3) if scores else 0.0,
                                    "forbidden": forb, "replies": len(scores), "cases": cases}
    (run_dir / "summary.json").write_text(json.dumps(summary, indent=1) + "\n")
    print_table(summary)
    return summary


def print_table(summary):
    rs = summary["responders"]
    ids = list(next(iter(rs.values()))["cases"].keys())
    print(f"\nrun {summary['run']}")
    print(f"{'case':<20} " + " ".join(f"{m:>7}" for m in rs) + "  missed / forbidden")
    for i in ids:
        notes = "; ".join(f"{m}: " + ",".join(rs[m]["cases"][i]["missed"] + ["!" + f for f in rs[m]["cases"][i]["forbidden"]])
                          for m in rs if rs[m]["cases"][i]["missed"] or rs[m]["cases"][i]["forbidden"])
        print(f"{i:<20} " + " ".join(f"{rs[m]['cases'][i]['mean']:>7.2f}" for m in rs) + "  " + notes)
    print(f"{'MEAN':<20} " + " ".join(f"{rs[m]['mean']:>7.3f}" for m in rs))
    print(f"{'forbidden items':<20} " + " ".join(f"{rs[m]['forbidden']:>7}" for m in rs))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("verify")
    r = sub.add_parser("run")
    r.add_argument("--stamp")
    r.add_argument("--phase", choices=("respond", "judge", "all"), default="all")
    r.add_argument("--docs")
    r.add_argument("--samples", type=int, default=2)
    r.add_argument("--only")
    r.add_argument("--jobs", type=int, default=8)
    rj = sub.add_parser("rejudge")
    rj.add_argument("run_dir")
    rj.add_argument("--stamp", required=True)
    rj.add_argument("--jobs", type=int, default=7)
    rep = sub.add_parser("report")
    rep.add_argument("run_dir")
    args = ap.parse_args()
    try:
        if args.cmd == "verify":
            return verify(args)
        if args.cmd == "rejudge":
            return rejudge(args)
        if args.cmd == "report":
            summarize(Path(args.run_dir))
            return 0
        return run(args)
    except Stop as e:
        print(f"\nSTOPPED: {e}\nA quota or sign-in problem stopped the run. Nothing switched billing; "
              "rerun the same command with --stamp to resume once it clears.", file=sys.stderr)
        return 3


if __name__ == "__main__":
    sys.exit(main())
