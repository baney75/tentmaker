"""Decoy listeners for the abuse test (.github/workflows/abuse.yml).

Each listener stands in for something a CI job must never reach: another
tailnet device, a LAN device, the mesh API on port 7433, a loopback service.
It answers any GET with 200 and "tentmaker-decoy <addr>:<port>", so a job
that reaches one can prove it.

  python3 decoy.py ADDR:PORT [ADDR:PORT ...]     IPv6 as [ADDR]:PORT
"""

import http.server
import socket
import sys
import threading


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        host, port = self.server.server_address[:2]
        body = ("tentmaker-decoy %s:%d\n" % (host, port)).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        sys.stderr.write("decoy %s: %s\n" % (self.server.server_address[0], fmt % args))


class Server4(http.server.ThreadingHTTPServer):
    allow_reuse_address = True


class Server6(Server4):
    address_family = socket.AF_INET6


def serve(spec):
    host, _, port = spec.rpartition(":")
    cls = Server4
    if host.startswith("["):
        host, cls = host[1:-1], Server6
    srv = cls((host, int(port)), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    print("decoy listening on " + spec, flush=True)


def main():
    if len(sys.argv) < 2:
        sys.exit("usage: decoy.py ADDR:PORT [ADDR:PORT ...]")
    for spec in sys.argv[1:]:
        serve(spec)
    threading.Event().wait()


if __name__ == "__main__":
    main()
