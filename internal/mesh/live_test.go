//go:build live

package mesh

import (
	"context"
	"net/netip"
	"testing"
)

// TestLiveTailscaleCLI reads this device's real tailnet state, read-only.
// It logs counts, never names, IPs or logins.
func TestLiveTailscaleCLI(t *testing.T) {
	ts := NewTailscaleCLI()
	ctx := context.Background()
	self, err := ts.Self(ctx)
	if err != nil {
		t.Skipf("tailscale not up here: %v", err)
	}
	if self.ID == "" || self.UserID == 0 || self.LoginName == "" || len(self.IPs) == 0 {
		t.Fatalf("self is missing fields (id %t, user %t, login %t, ips %d)", self.ID != "", self.UserID != 0, self.LoginName != "", len(self.IPs))
	}
	peers, err := ts.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	online := 0
	for _, p := range peers {
		if p.Online {
			online++
		}
	}
	t.Logf("%d peers, %d online", len(peers), online)
	id, err := ts.WhoIs(ctx, netip.AddrPortFrom(self.IPs[0], 7433).String())
	if err != nil {
		t.Fatal(err)
	}
	if id.UserID != self.UserID || id.LoginName != self.LoginName {
		t.Fatal("whois on this device's own IP names a different user than status")
	}
}
