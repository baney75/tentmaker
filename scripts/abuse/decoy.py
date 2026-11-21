"""Decoy listeners for the abuse test (.github/workflows/abuse.yml).

Each listener stands in for something a CI job must never reach: another
tailnet device, MagicDNS, a LAN device, the mesh API on port 7433, a
loopback service, tailscaled's local API, a multicast group on the LAN. It
answers with "tentmaker-decoy <where>", so a job that reaches one can prove
it.

  python3 decoy.py SPEC [SPEC ...]

  ADDR:PORT          HTTP over TCP; IPv6 as [ADDR]:PORT (IPv6 only, so
                     [::]:PORT and 0.0.0.0:PORT can both listen)
  udp:ADDR:PORT      answers every UDP datagram (a DNS server stand-in)
  mcast:GROUP:PORT   joins an IPv4 multicast group and answers each
                     datagram to its sender
  unix:PATH          HTTP over a Unix socket, mode 0666 like tailscaled's
"""

import http.server
import os
import socket
import socketserver
import struct
import sys
import threading


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = ("tentmaker-decoy %s\n" % self.server.where).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def address_string(self):
        return str(self.client_address[0]) if self.client_address else "unix"

    def log_message(self, fmt, *args):
        sys.stderr.write("decoy %s: %s\n" % (self.server.where, fmt % args))


class Server4(http.server.ThreadingHTTPServer):
    allow_reuse_address = True


class Server6(Server4):
    address_family = socket.AF_INET6

    def server_bind(self):
        self.socket.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
        super().server_bind()


class ServerUnix(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True


def udp(host, port, group=None):
    fam = socket.AF_INET6 if ":" in host else socket.AF_INET
    s = socket.socket(fam, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    if group:
        s.bind(("", port))
        s.setsockopt(socket.IPPROTO_IP, socket.IP_ADD_MEMBERSHIP, struct.pack("4s4s", socket.inet_aton(group), socket.inet_aton("0.0.0.0")))
    else:
        s.bind((host, port))
    where = "udp %s:%d" % (group or host, port)

    def loop():
        while True:
            data, peer = s.recvfrom(4096)
            sys.stderr.write("decoy %s: %d bytes from %s\n" % (where, len(data), peer[0]))
            # A DNS query gets its id back, so a resolver library sees an answer.
            s.sendto(data[:2] + b"tentmaker-decoy " + where.encode(), peer)

    threading.Thread(target=loop, daemon=True).start()


def serve(spec):
    kind, _, rest = spec.partition(":")
    if kind == "unix":
        try:
            os.unlink(rest)
        except FileNotFoundError:
            pass
        srv = ServerUnix(rest, Handler)
        os.chmod(rest, 0o666)
    elif kind in ("udp", "mcast"):
        host, _, port = rest.rpartition(":")
        host = host.strip("[]")
        udp(host, int(port), group=host if kind == "mcast" else None)
        print("decoy listening on " + spec, flush=True)
        return
    else:
        host, _, port = spec.rpartition(":")
        cls = Server4
        if host.startswith("["):
            host, cls = host[1:-1], Server6
        srv = cls((host, int(port)), Handler)
    srv.where = spec
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    print("decoy listening on " + spec, flush=True)


def main():
    if len(sys.argv) < 2:
        sys.exit("usage: decoy.py SPEC [SPEC ...]")
    for spec in sys.argv[1:]:
        try:
            serve(spec)
        except OSError as e:
            # The harness checks every decoy answers; one that can't listen
            # must not take the others down.
            print("decoy failed on %s: %s" % (spec, e), flush=True)
    threading.Event().wait()


if __name__ == "__main__":
    main()
