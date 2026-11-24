package mesh

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func doHost(n *Node, method, path, host, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr, req.Host = "100.64.0.2:4000", host
	req.Header.Set(poolHeader, "home")
	rec := httptest.NewRecorder()
	n.handler().ServeHTTP(rec, req)
	return rec
}

var apiRoutes = []struct{ method, path string }{
	{"GET", "/v1/hello"}, {"GET", "/v1/status"}, {"POST", "/v1/join"}, {"POST", "/v1/work"},
	{"POST", "/v1/claim"}, {"POST", "/v1/release"}, {"GET", "/v1/runs"}, {"GET", "/v1/runs/log"},
	{"GET", "/v1/runs/tests"}, {"POST", "/v1/rerun"}, {"POST", "/v1/wake"}, {"GET", "/v1/guests"},
	{"POST", "/v1/guests"}, {"POST", "/v1/leave"}, {"GET", "/v1/archive/" + strings.Repeat("a", 40)},
	{"GET", "/"}, {"GET", "/status"}, {"GET", "/metrics"}, {"GET", "/nowhere"},
}

const guestBody = `{"guests":[{"node":"nEVIL","login":"evil@example.com","user_id":6666,"trust":"trusted","device":"evil-pc","joined":"2026-11-24T00:00:00Z","updated":"2026-11-24T00:00:00Z"}]}`

// A page that rebinds its own name to this device's tailnet address reaches
// it from an admitted device, with its own name as Host. Every route refuses
// it, and the guest table stays as it was.
func TestEveryRouteRefusesAForeignHost(t *testing.T) {
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS()})
	for _, host := range []string{"evil.example", "evil.example:7433", "alice-mac.evil.example", "localhost:7433", "127.0.0.1:7433", "10.0.0.5:7433", "100.63.255.255:7433", "fd7a:115c:a1e1::1", ""} {
		for _, r := range apiRoutes {
			rec := doHost(n, r.method, r.path, host, guestBody)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "tailnet address or name") {
				t.Errorf("%s %s with Host %q: %d %s", r.method, r.path, host, rec.Code, rec.Body)
			}
		}
	}
	if g := n.guestList(true); len(g) != 0 {
		t.Fatalf("a foreign Host wrote guests: %+v", g)
	}
}

// Every name and address of this device is its own Host, and so is a
// masquerade address a peer in another tailnet dials.
func TestEveryOwnHostReachesTheAPI(t *testing.T) {
	ts := newFakeTS()
	ts.self.DNSName = "alice-mac.example-tailnet.ts.net."
	ts.self.IPs = append(ts.self.IPs, netip.MustParseAddr("fd7a:115c:a1e0::1"))
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts})
	for _, host := range []string{"100.64.0.1", "100.64.0.1:7433", "[fd7a:115c:a1e0::1]:7433", "alice-mac", "alice-mac:7433",
		"alice-mac.example-tailnet.ts.net:7433", "ALICE-MAC.example-tailnet.ts.net.",
		"100.127.0.9:7433", "[fd7a:115c:a1e0:ffff::9]:7433"} {
		for _, r := range apiRoutes {
			if rec := doHost(n, r.method, r.path, host, "{}"); strings.Contains(rec.Body.String(), "tailnet address or name") {
				t.Errorf("%s %s with own Host %q refused: %d %s", r.method, r.path, host, rec.Code, rec.Body)
			}
		}
		if rec := doHost(n, "GET", "/v1/hello", host, ""); rec.Code != http.StatusOK {
			t.Errorf("hello with own Host %q: %d", host, rec.Code)
		}
	}
}

// The addresses peers and lent devices dial by default pass the Host check.
func TestDialedHostsAreOwnHosts(t *testing.T) {
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS()})
	for _, ip := range []string{"100.64.0.2", "fd7a:115c:a1e0::2"} {
		p := Peer{IPs: []netip.Addr{netip.MustParseAddr(ip)}}
		for _, raw := range []string{n.peerURL(p), n.joinURL(netip.AddrPortFrom(p.IPs[0], DefaultPort))} {
			u, err := url.Parse(raw)
			if err != nil || !hostIsSelf(p, u.Host) {
				t.Errorf("%s is not its own Host (%v)", raw, err)
			}
		}
	}
}
