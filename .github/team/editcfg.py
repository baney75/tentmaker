"""Edit a Tentmaker config.toml for the team test.

usage: python editcfg.py CONFIG

Sets mesh.members = ["tagged-devices"] (the CI nodes are tagged, and tagged
devices are never "the same user") and update.auto = false (the daemon must
not swap the build under test for a published release mid-run). setup has no
flag for either, so this edits the file setup wrote, then the caller
restarts the service.
"""

import re
import sys


def main(path):
    with open(path, encoding="utf-8") as f:
        lines = f.read().splitlines()
    out, section, did_members, did_auto = [], "", False, False
    for line in lines:
        m = re.match(r"\s*\[([^\]]+)\]\s*$", line)
        if m:
            section = m.group(1).strip()
            out.append(line)
            if section == "mesh":
                out.append('  members = ["tagged-devices"]')
                did_members = True
            continue
        key = line.split("=", 1)[0].strip()
        if section == "mesh" and key == "members":
            continue
        if section == "update" and key == "auto":
            out.append("  auto = false")
            did_auto = True
            continue
        out.append(line)
    if not did_members:
        out += ["", "[mesh]", '  members = ["tagged-devices"]']
    if not did_auto:
        out += ["", "[update]", "  auto = false"]
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        f.write("\n".join(out) + "\n")
    print(f"{path}: mesh.members = [\"tagged-devices\"], update.auto = false")


if __name__ == "__main__":
    main(sys.argv[1])
