"""Send one UDP datagram and wait for a decoy's answer (scripts/abuse/decoy.py).

  python3 udpask.py ADDR PORT     exits 0 when an answer came back

The datagram is a DNS query for github.com, so a real DNS server answers
too. Multicast groups work: the decoy answers from its own address.
"""

import socket
import sys


def main():
    host, port = sys.argv[1], int(sys.argv[2])
    fam = socket.AF_INET6 if ":" in host else socket.AF_INET
    s = socket.socket(fam, socket.SOCK_DGRAM)
    s.settimeout(4)
    if fam == socket.AF_INET:
        s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_TTL, 1)
    q = b"\x7a\x11\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x06github\x03com\x00\x00\x01\x00\x01"
    try:
        s.sendto(q, (host, port))
        data, peer = s.recvfrom(4096)
    except OSError as e:
        print("no answer: %s" % e)
        return 1
    print("answer from %s: %r" % (peer[0], data[:60]))
    return 0


if __name__ == "__main__":
    sys.exit(main())
