package mesh

import (
	"net/http"
	"net/netip"
	"testing"
)

// A request that reaches the peer API from this device's own address is a
// local process (a CI job where the fence leaks, or Windows, which does
// not filter loopback), not a peer. WhoIs would call it this device, a
// fully trusted peer, so it is refused on every route (review r1 fence
// verification).
func TestPeerAPIRefusesThisDevice(t *testing.T) {
	ts := newFakeTS()
	ts.self.IPs = []netip.Addr{netip.MustParseAddr("100.64.0.1"), netip.MustParseAddr("fd7a:115c:a1e0::1")}
	self := Identity{Node: "alice-mac.example-tailnet.ts.net", HostName: "alice-mac", UserID: 1001, LoginName: "alice@example.com",
		IPs: ts.self.IPs}
	ts.ids["100.64.0.1"] = self
	ts.ids["fd7a:115c:a1e0::1"] = self
	ts.ids["::1"] = alice
	// A WhoIs answer naming this device for an address it doesn't list.
	ts.ids["100.64.0.9"] = self
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts, Broker: &fakeBroker{secret: []byte("s3cr3t")}})
	knowPeer(n, "n2", "100.64.0.2", "w")
	for _, remote := range []string{"100.64.0.1:40000", "[fd7a:115c:a1e0::1]:40000", "127.0.0.1:40000", "[::1]:40000",
		"[::ffff:100.64.0.1]:40000", "100.64.0.9:40000"} {
		for _, path := range []string{"/v1/hello", "/v1/status"} {
			if c := do(t, n.handler(), "GET", path, remote, nil, "home").Code; c != http.StatusForbidden {
				t.Errorf("GET %s from %s: %d, want 403", path, remote, c)
			}
		}
		if c := do(t, n.handler(), "POST", "/v1/claim", remote, nil, "home").Code; c != http.StatusForbidden {
			t.Errorf("POST /v1/claim from %s: %d, want 403", remote, c)
		}
	}
	if c := do(t, n.handler(), "GET", "/v1/hello", "100.64.0.2:40000", nil, "").Code; c != http.StatusOK {
		t.Errorf("a peer was refused: %d", c)
	}
}
