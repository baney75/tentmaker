"""Judge one abuse test run (.github/workflows/abuse.yml).

Reads the check logs the broker kept (one file per job, named <job>.log in
--logs), the jobs' commit statuses (--statuses, a JSON object: context ->
{state, description}) and the harness's own ATTACK lines (--harness), then
writes --out (one entry per attack: blocked true or false) and exits 1 when
any attack succeeded, any expected attack line is missing, or a control
failed.

  python3 judge.py --os linux|windows --logs DIR --statuses FILE --harness FILE --out FILE
"""

import argparse
import json
import os
import re
import sys

MAIN = [
    "read-secrets", "read-control-token", "pause-no-token", "pause-guessed-token",
    "decoy-tailnet-v4", "decoy-tailnet-v6", "decoy-lan-v4", "cloud-metadata",
    "mesh-port-host-ip", "mesh-port-tailnet", "mesh-port-loopback", "loopback-other-port",
    "PRIV", "write-other-home", "write-state-dir", "signal-daemon", "read-other-job",
]
HARNESS = ["background-process", "daemon-healthy"]
# Linux only: the same network attacks from a rootless podman container.
PODMAN = ["podman-decoy-tailnet-v4", "podman-decoy-lan-v4", "podman-mesh-port-host-ip", "podman-loopback-other-port"]
LINE = re.compile(r"^ATTACK (\S+) (BLOCKED|SUCCEEDED)\b ?(.*)$")


def attacks(text):
    out = {}
    for line in text.splitlines():
        m = LINE.match(line.strip())
        if m:
            name, res, detail = m.groups()
            # The first SUCCEEDED wins over any BLOCKED for the same name.
            if name not in out or res == "SUCCEEDED":
                out[name] = (res == "BLOCKED", detail.strip())
    return out


def read(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return ""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--os", required=True, choices=["linux", "windows"])
    ap.add_argument("--logs", required=True)
    ap.add_argument("--statuses", required=True)
    ap.add_argument("--harness", required=True)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    sfx = "-windows" if a.os == "windows" else ""
    job = {k: "abuse" + k + sfx for k in ("", "-sibling", "-oom", "-pids")}
    logs = {k: read(os.path.join(a.logs, v + ".log")) for k, v in job.items()}
    try:
        with open(a.statuses, encoding="utf-8") as f:
            statuses = json.load(f)
    except (OSError, ValueError):
        statuses = {}
    harness = attacks(read(a.harness))
    problems = []

    def status(k):
        return statuses.get("tentmaker/" + job[k]) or {}

    for k in ("", "-sibling", "-pids"):
        st = status(k)
        if st.get("state") != "success":
            problems.append("tentmaker/%s is %s: %s" % (job[k], st.get("state", "missing"), st.get("description", "")))

    main_log = logs[""]
    for c in ("internet", "dns") + (("podman",) if a.os == "linux" else ()):
        if "CONTROL %s OK" % c not in main_log:
            problems.append("control %s failed: the job could not reach what it needs, so blocked attacks prove nothing" % c)

    got = attacks(main_log)
    got.update({k: v for k, v in attacks(logs["-pids"]).items() if k == "fork-bomb"})

    # read-other-job only counts when the sibling ran while abuse looked.
    m = re.search(r"^READ-OTHER-JOB window (\d+) (\d+)", main_log, re.M)
    s = re.search(r"^SIBLING start (\d+)", logs["-sibling"], re.M)
    e = re.search(r"^SIBLING end (\d+)", logs["-sibling"], re.M)
    if "read-other-job" in got and got["read-other-job"][0]:
        if not (m and s and e and int(s.group(1)) <= int(m.group(2)) and int(e.group(1)) >= int(m.group(1))):
            del got["read-other-job"]
            problems.append("read-other-job is inconclusive: the sibling job did not run while abuse looked for it")

    # memory-limit: the job must end (killed or failed allocating) and say so.
    oom = attacks(logs["-oom"]).get("memory-limit")
    st = status("-oom")
    if oom and not oom[0]:
        got["memory-limit"] = oom
    elif st.get("state") in ("failure", "error"):
        got["memory-limit"] = (True, st.get("description", ""))
        if not re.search(r"ran out of memory", st.get("description", ""), re.I):
            problems.append("tentmaker/%s does not say it ran out of memory: %r" % (job["-oom"], st.get("description", "")))
    elif st:
        got["memory-limit"] = (False, "the job ended %s: %s" % (st.get("state"), st.get("description", "")))

    got.update(harness)

    want = [n.replace("PRIV", "admin" if a.os == "windows" else "sudo") for n in MAIN]
    want += (PODMAN if a.os == "linux" else []) + ["fork-bomb", "memory-limit"] + HARNESS
    entries = []
    for n in want:
        if n not in got:
            problems.append("no ATTACK %s line" % n)
            entries.append({"name": n, "blocked": False, "detail": "no ATTACK line: the attack's result is unknown"})
            continue
        blocked, detail = got[n]
        entries.append({"name": n, "blocked": blocked, "detail": detail})
        if not blocked:
            problems.append("ATTACK %s SUCCEEDED %s" % (n, detail))

    result = {"os": a.os, "commit": os.environ.get("GITHUB_SHA", ""), "run": os.environ.get("GITHUB_RUN_ID", ""),
              "ok": not problems, "attacks": entries, "problems": problems}
    os.makedirs(os.path.dirname(os.path.abspath(a.out)), exist_ok=True)
    with open(a.out, "w", encoding="utf-8") as f:
        json.dump(result, f, indent=2)
        f.write("\n")
    for x in entries:
        print("%-22s %s  %s" % (x["name"], "blocked" if x["blocked"] else "SUCCEEDED", x["detail"]))
    for p in problems:
        print("::error::" + p)
    return 0 if not problems else 1


if __name__ == "__main__":
    sys.exit(main())
