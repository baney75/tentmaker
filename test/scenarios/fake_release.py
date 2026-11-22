#!/usr/bin/env python3
"""A fake GitHub release for the scenario harness, on 127.0.0.1.

    fake_release.py ROOT PORT

ROOT holds install.sh, install.ps1, rel/<version>/ (one build's archives and
SHA256SUMS) and a file named current with the newest version. It serves:

  /install.sh, /install.ps1           the installers
  /latest/download/<asset>            the current build (stable installs)
  /download/dev/<asset>               the current build (dev installs)
  /rel/<version>/<asset>              any build
  /repos/<o>/<r>/releases/tags/dev    the release API for the dev channel,
                                      which `tentmaker update` reads when
                                      TENTMAKER_UPDATE_API points here
  /repos/<o>/<r>/releases             the same release in a list
  /healthz                            200 once the server is up
"""

import json
import os
import sys
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

ROOT = os.path.abspath(sys.argv[1])
PORT = int(sys.argv[2])
BASE = "http://127.0.0.1:%d" % PORT


def current():
    try:
        with open(os.path.join(ROOT, "current")) as f:
            return f.read().strip()
    except OSError:
        return ""


def release(version):
    d = os.path.join(ROOT, "rel", version)
    assets = []
    for name in sorted(os.listdir(d)) if os.path.isdir(d) else []:
        assets.append({
            "name": name,
            "size": os.path.getsize(os.path.join(d, name)),
            "browser_download_url": "%s/rel/%s/%s" % (BASE, version, name),
        })
    return {
        "tag_name": "dev",
        "name": "tentmaker %s" % version,
        "prerelease": True,
        "draft": False,
        "assets": assets,
    }


class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *a, **kw):
        super().__init__(*a, directory=ROOT, **kw)

    def log_message(self, fmt, *args):
        sys.stderr.write("%s\n" % (fmt % args))

    def send_json(self, v):
        body = json.dumps(v).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/healthz":
            return self.send_json({"ok": True})
        if path.startswith("/repos/"):
            v = current()
            if not v:
                self.send_error(404)
                return
            if path.endswith("/releases/tags/dev"):
                return self.send_json(release(v))
            if path.endswith("/releases"):
                return self.send_json([release(v)])
            self.send_error(404)
            return
        for prefix in ("/latest/download/", "/download/dev/"):
            if path.startswith(prefix):
                self.path = "/rel/%s/%s" % (current(), path[len(prefix):])
                break
        return super().do_GET()


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
