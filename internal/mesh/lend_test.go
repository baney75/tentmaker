package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// lendHost is alice-mac, a broker in pool "home" with a state dir, and its
// clock.
func lendHost(t *testing.T, members ...string) (*Node, *fakeBroker, *clock, *syncBuf) {
	t.Helper()
	log, buf := debugLog()
	fb := &fakeBroker{secret: []byte("JIT-secret-0123456789")}
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: newFakeTS(), Broker: fb, StateDir: t.TempDir(), Log: log,
		Members: members, Wake: func(context.Context) WakeInfo { return *wakeInfo("a4:5e:60:00:00:01", homeSubnet, homeBcast) }})
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	n.now = clk.Now
	return n, fb, clk, buf
}

func mkInvite(t *testing.T, n *Node, trust string, repos []string, ttl time.Duration) (code, secret string) {
	t.Helper()
	ic, err := n.Invite(context.Background(), trust, repos, ttl)
	if err != nil {
		t.Fatal(err)
	}
	sec, host, err := parseCode(ic.Code)
	if err != nil {
		t.Fatalf("parse %q: %v", ic.Code, err)
	}
	if host.Addr() != netip.MustParseAddr("100.64.0.1") {
		t.Fatalf("code names host %v, want this device's tailnet address", host)
	}
	return ic.Code, sec
}

func joinBody(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

const bobAddr = "100.64.0.5:40000"

func TestInviteCodeStrength(t *testing.T) {
	n, _, _, _ := lendHost(t)
	seen := map[string]bool{}
	for range 20 {
		code, sec := mkInvite(t, n, core.TrustTrusted, nil, time.Hour)
		if seen[sec] {
			t.Fatal("two invites share a secret")
		}
		seen[sec] = true
		if !strings.HasPrefix(code, codePrefix) || len(sec)*5 < 128 {
			t.Fatalf("code %q: secret has %d bits, want at least 128", code, len(sec)*5)
		}
	}
}

func TestJoinRecordsWhoIsIdentityAndInviteTrust(t *testing.T) {
	n, _, _, logs := lendHost(t)
	_, sec := mkInvite(t, n, core.TrustTrusted, []string{"alice/app"}, time.Hour)
	// The guest claims to be alice, the owner, with owner trust and every
	// repo. None of it counts.
	body := joinBody(t, map[string]any{"code": sec, "device": "bob-pc", "trust": "owner", "repos": []string{},
		"node": "nSELF", "login": "alice@example.com", "user_id": 1001})
	rec := do(t, n.handler(), "POST", "/v1/join", bobAddr, body, "")
	if rec.Code != 200 {
		t.Fatalf("join: %d %s", rec.Code, rec.Body)
	}
	var resp joinResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Pool != "home" || resp.Trust != core.TrustTrusted || len(resp.Repos) != 1 || resp.Repos[0] != "alice/app" {
		t.Fatalf("answer %+v", resp)
	}
	gs := n.guestList(true)
	if len(gs) != 1 {
		t.Fatalf("guests %+v", gs)
	}
	g := gs[0]
	if g.Node != bob.ID || g.UserID != bob.UserID || g.Login != bob.LoginName || g.Trust != core.TrustTrusted ||
		len(g.Repos) != 1 || g.Repos[0] != "alice/app" {
		t.Fatalf("guest %+v: identity must be WhoIs's and trust the invite's", g)
	}
	// Only the hash is kept, and the secret is in no log or answer.
	raw, err := os.ReadFile(filepath.Join(n.o.StateDir, guestsFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), sec) || strings.Contains(logs.String(), sec) || strings.Contains(rec.Body.String(), sec) {
		t.Fatal("the join secret leaked into the state file, a log or the answer")
	}
	// A guest is not an owner: it can't read status, wake devices or
	// change the guest table.
	for _, c := range []struct{ method, path string }{{"GET", "/v1/status"}, {"POST", "/v1/wake"}, {"GET", "/v1/guests"}, {"POST", "/v1/guests"}} {
		if rec := do(t, n.handler(), c.method, c.path, bobAddr, strings.NewReader("{}"), "home"); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s by a guest: %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

func TestUsedOrExpiredCodeRefused(t *testing.T) {
	n, _, clk, _ := lendHost(t)
	_, sec := mkInvite(t, n, core.TrustTrusted, nil, time.Hour)
	if rec := do(t, n.handler(), "POST", "/v1/join", bobAddr, joinBody(t, joinRequest{Code: sec}), ""); rec.Code != 200 {
		t.Fatalf("first use: %d %s", rec.Code, rec.Body)
	}
	used := do(t, n.handler(), "POST", "/v1/join", bobAddr, joinBody(t, joinRequest{Code: sec}), "")
	if used.Code != http.StatusForbidden || errText(used) != badCodeAnswer {
		t.Fatalf("second use: %d %q", used.Code, errText(used))
	}

	_, sec2 := mkInvite(t, n, core.TrustPublic, nil, time.Minute)
	clk.Add(2 * time.Minute)
	exp := do(t, n.handler(), "POST", "/v1/join", bobAddr, joinBody(t, joinRequest{Code: sec2}), "")
	if exp.Code != http.StatusForbidden || errText(exp) != badCodeAnswer {
		t.Fatalf("expired: %d %q", exp.Code, errText(exp))
	}
	wrong := do(t, n.handler(), "POST", "/v1/join", bobAddr, joinBody(t, joinRequest{Code: strings.Repeat("a", 32)}), "")
	if wrong.Code != http.StatusForbidden || errText(wrong) != badCodeAnswer {
		t.Fatalf("wrong code: %d %q; every bad code gets the same answer", wrong.Code, errText(wrong))
	}
	if g := n.guestList(true); len(g) != 1 || g[0].Trust != core.TrustTrusted {
		t.Fatalf("guests %+v: a refused code changed the table", g)
	}
}

func TestJoinRateLimited(t *testing.T) {
	n, _, clk, _ := lendHost(t)
	_, sec := mkInvite(t, n, core.TrustTrusted, nil, time.Hour)
	for i := range joinFailsNode {
		rec := do(t, n.handler(), "POST", "/v1/join", bobAddr, joinBody(t, joinRequest{Code: strings.Repeat("b", 32)}), "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("try %d: %d", i, rec.Code)
		}
	}
	rec := do(t, n.handler(), "POST", "/v1/join", bobAddr, joinBody(t, joinRequest{Code: sec}), "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: %d, want 429 even for the right code", joinFailsNode, rec.Code)
	}
	clk.Add(joinWindow + time.Second)
	if rec := do(t, n.handler(), "POST", "/v1/join", bobAddr, joinBody(t, joinRequest{Code: sec}), ""); rec.Code != 200 {
		t.Fatalf("after the window: %d %s", rec.Code, rec.Body)
	}
}

func TestJoinRefusesTaggedAndOwner(t *testing.T) {
	n, _, _, _ := lendHost(t)
	_, sec := mkInvite(t, n, core.TrustTrusted, nil, time.Hour)
	if rec := do(t, n.handler(), "POST", "/v1/join", "100.64.0.6:1", joinBody(t, joinRequest{Code: sec}), ""); rec.Code != http.StatusForbidden {
		t.Fatalf("tagged: %d", rec.Code)
	}
	if rec := do(t, n.handler(), "POST", "/v1/join", "100.64.0.2:1", joinBody(t, joinRequest{Code: sec}), ""); rec.Code != http.StatusConflict {
		t.Fatalf("the owner's own device: %d", rec.Code)
	}
	// Neither burned the code.
	if rec := do(t, n.handler(), "POST", "/v1/join", bobAddr, joinBody(t, joinRequest{Code: sec}), ""); rec.Code != 200 {
		t.Fatalf("bob after refusals: %d %s", rec.Code, rec.Body)
	}
}

func TestGuestWorkCarriesLendAndRevokeStopsIt(t *testing.T) {
	n, fb, _, _ := lendHost(t)
	_, sec := mkInvite(t, n, core.TrustTrusted, []string{"alice/app"}, time.Hour)
	if rec := do(t, n.handler(), "POST", "/v1/join", bobAddr, joinBody(t, joinRequest{Code: sec, Device: "bob-pc"}), ""); rec.Code != 200 {
		t.Fatalf("join: %d %s", rec.Code, rec.Body)
	}
	knowPeer(n, bob.ID, "100.64.0.5", "bob-pc")
	work := func() int {
		b := joinBody(t, core.WorkRequest{Device: "bob-pc", Free: 1, Platforms: []core.Platform{{OS: "linux", Arch: "x64"}}})
		return do(t, n.handler(), "POST", "/v1/work", bobAddr, b, "home").Code
	}
	if c := work(); c != 200 {
		t.Fatalf("guest work: %d", c)
	}
	fb.mu.Lock()
	l := fb.lends[len(fb.lends)-1]
	fb.mu.Unlock()
	if l == nil || l.Trust != core.TrustTrusted || len(l.Repos) != 1 || l.Repos[0] != "alice/app" {
		t.Fatalf("broker saw lend %+v, want the invite's", l)
	}
	if _, err := n.RevokeGuest(context.Background(), "BOB-PC"); err != nil {
		t.Fatal(err)
	}
	c := work()
	fb.mu.Lock()
	l = fb.lends[len(fb.lends)-1]
	fb.mu.Unlock()
	if c == 200 && (l == nil || l.Trust != core.TrustRevoked) {
		t.Fatalf("revoked guest reached the broker with lend %+v", l)
	}
	if _, err := n.RevokeGuest(context.Background(), "bob-pc"); err != ErrNoGuest {
		t.Fatalf("second revoke: %v", err)
	}
}

func TestWakeOnlyFromSameUser(t *testing.T) {
	// bob is a mesh.members login: owner trust for work, as before, but
	// not the same tailnet user, so its wake facts and wake requests are
	// not honored (#58).
	n, _, _, _ := lendHost(t, "bob@example.com")
	if rec := do(t, n.handler(), "POST", "/v1/wake", bobAddr, strings.NewReader(`{"device":"desk"}`), "home"); rec.Code != http.StatusForbidden {
		t.Fatalf("wake from a member: %d, want 403", rec.Code)
	}
	knowPeer(n, alice.ID, "100.64.0.2", "alice-linux")
	knowPeer(n, bob.ID, "100.64.0.5", "bob-pc")
	if rec := do(t, n.handler(), "POST", "/v1/wake", bobAddr, strings.NewReader(`{"device":"desk"}`), "home"); rec.Code != http.StatusForbidden {
		t.Fatalf("wake from a known member: %d, want 403", rec.Code)
	}
	if rec := do(t, n.handler(), "POST", "/v1/wake", "100.64.0.2:1", strings.NewReader(`{"device":"nope"}`), "home"); rec.Code == http.StatusForbidden {
		t.Fatalf("wake from the same user: 403")
	}
	n.wakeMu.Lock()
	n.selfWake = wakeInfo("a4:5e:60:00:00:01", homeSubnet, homeBcast)
	n.wakeMu.Unlock()
	var h helloResponse
	rec := do(t, n.handler(), "GET", "/v1/hello", bobAddr, nil, "")
	if rec.Code != 200 {
		t.Fatalf("hello from a member: %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &h)
	if h.Wake != nil {
		t.Fatal("hello told a member this device's wake facts")
	}
	rec = do(t, n.handler(), "GET", "/v1/hello", "100.64.0.2:1", nil, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &h)
	if h.Wake == nil {
		t.Fatal("hello hid wake facts from the same user")
	}
}

func TestMembersKeepOwnerTrust(t *testing.T) {
	n, fb, _, _ := lendHost(t, "bob@example.com")
	knowPeer(n, bob.ID, "100.64.0.5", "bob-pc")
	b := joinBody(t, core.WorkRequest{Device: "bob-pc", Free: 1, Platforms: []core.Platform{{OS: "linux", Arch: "x64"}}})
	if rec := do(t, n.handler(), "POST", "/v1/work", bobAddr, b, "home"); rec.Code != 200 {
		t.Fatalf("member work: %d %s", rec.Code, rec.Body)
	}
	if l := fb.lends[len(fb.lends)-1]; l != nil {
		t.Fatalf("a member got lend %+v, want owner trust (none)", l)
	}
	if rec := do(t, n.handler(), "GET", "/v1/status", bobAddr, nil, "home"); rec.Code != 200 {
		t.Fatalf("member status: %d", rec.Code)
	}
}

// TestJoinAcrossUsers runs the whole flow over HTTP: alice's broker
// invites, bob's device joins, works for alice's pool with the invite's
// trust, and alice's broker keeps none of bob's wake facts.
func TestJoinAcrossUsers(t *testing.T) {
	ms := newCluster(t,
		nodeSpec{device: "desk", pool: "home", broker: &fakeBroker{secret: []byte("JIT-secret-for-the-runner-abcdef")}, state: t.TempDir()},
		nodeSpec{device: "bob-pc", pool: "bobs", user: 1002, state: t.TempDir(),
			wake: wakeInfo("a4:5e:60:00:00:09", homeSubnet, homeBcast)},
	)
	host, guest := ms[0], ms[1]
	ctx := context.Background()
	guest.node.hostURL = func(netip.AddrPort) string { return host.srv.URL }

	if bs := guest.node.Brokers(ctx); len(bs) != 0 {
		t.Fatalf("before join bob sees brokers %v", names(bs))
	}
	ic, err := host.node.Invite(ctx, core.TrustTrusted, []string{"alice/app"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	l, err := guest.node.Join(ctx, ic.Code)
	if err != nil {
		t.Fatal(err)
	}
	if l.Pool != "home" || l.OwnerUserID != 1001 || l.HostNode != host.id || l.Trust != core.TrustTrusted {
		t.Fatalf("lent %+v", l)
	}
	sec, _, _ := parseCode(ic.Code)
	for _, m := range ms {
		if strings.Contains(m.logs.String(), sec) {
			t.Fatalf("%s logged the join secret", m.spec.device)
		}
	}
	if _, err := guest.node.Join(ctx, ic.Code); err == nil || strings.Contains(err.Error(), sec) {
		t.Fatalf("second join: %v", err)
	}

	bs := guest.node.Brokers(ctx)
	if len(bs) != 1 {
		t.Fatalf("lent bob sees brokers %v", names(bs))
	}
	ls, ok := bs[0].(core.LentSource)
	if !ok || ls.Lend() == nil || ls.Lend().Trust != core.TrustTrusted {
		t.Fatalf("the host's broker must carry the lend bob accepted")
	}
	if _, err := bs[0].Work(ctx, core.WorkRequest{Device: "bob-pc", Free: 1, Platforms: []core.Platform{{OS: "linux", Arch: "x64"}}}); err != nil {
		t.Fatal(err)
	}
	fb := host.spec.broker
	fb.mu.Lock()
	got := fb.lends[len(fb.lends)-1]
	fb.mu.Unlock()
	if got == nil || got.Trust != core.TrustTrusted || got.Repos[0] != "alice/app" {
		t.Fatalf("host broker saw lend %+v", got)
	}

	// alice's broker found bob's device but kept none of its wake facts.
	host.node.markStale()
	devs := host.node.Devices(ctx)
	found := false
	for _, d := range devs {
		if d.Node == guest.id {
			found = true
			if !d.Lent || d.Trust != core.TrustTrusted || d.Owner != "bob@example.com" {
				t.Fatalf("bob's device in alice's list: %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("alice's devices %+v lack bob's", devs)
	}
	if k, ok := host.node.knownByID(guest.id); ok && k.Wake != nil {
		t.Fatalf("alice kept a guest's wake facts %+v", k.Wake)
	}

	// Revoked: the host's broker sees a revoked lend or no request at all.
	if _, err := host.node.RevokeGuest(ctx, "bob-pc"); err != nil {
		t.Fatal(err)
	}
	fb.mu.Lock()
	before := len(fb.lends)
	fb.mu.Unlock()
	_, _ = bs[0].Work(ctx, core.WorkRequest{Device: "bob-pc", Free: 1})
	fb.mu.Lock()
	if len(fb.lends) > before && (fb.lends[len(fb.lends)-1] == nil || fb.lends[len(fb.lends)-1].Trust != core.TrustRevoked) {
		t.Fatalf("a revoked guest reached the broker as %+v", fb.lends[len(fb.lends)-1])
	}
	fb.mu.Unlock()

	// bob leaves: back to its own pool, no brokers.
	if _, err := guest.node.Leave(ctx); err != nil {
		t.Fatal(err)
	}
	if bs := guest.node.Brokers(ctx); len(bs) != 0 {
		t.Fatalf("after leave bob sees brokers %v", names(bs))
	}
	if guest.node.PoolName() != "bobs" {
		t.Fatalf("after leave the pool is %q", guest.node.PoolName())
	}
}

func TestJoinRefusesOwnDeviceCode(t *testing.T) {
	ms := newCluster(t,
		nodeSpec{device: "desk", pool: "home", broker: &fakeBroker{}, state: t.TempDir()},
		nodeSpec{device: "laptop", pool: "home", state: t.TempDir()},
	)
	ms[1].node.hostURL = func(netip.AddrPort) string { return ms[0].srv.URL }
	ic, err := ms[0].node.Invite(context.Background(), core.TrustTrusted, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ms[1].node.Join(context.Background(), ic.Code); err == nil {
		t.Fatal("a device joined its own user's pool with a code")
	}
	if _, err := ms[1].node.Join(context.Background(), "tm1-nope"); err != errBadCode {
		t.Fatalf("bad code: %v", err)
	}
}

func TestGuestTableSharedAmongOwnerDevices(t *testing.T) {
	ms := newCluster(t,
		nodeSpec{device: "desk", pool: "home", broker: &fakeBroker{}, state: t.TempDir()},
		nodeSpec{device: "laptop", pool: "home", broker: &fakeBroker{}, state: t.TempDir()},
		nodeSpec{device: "bob-pc", pool: "bobs", user: 1002, state: t.TempDir()},
	)
	ctx := context.Background()
	desk, laptop, guest := ms[0], ms[1], ms[2]
	desk.node.Brokers(ctx) // discover laptop, so the join is pushed to it
	guest.node.hostURL = func(netip.AddrPort) string { return desk.srv.URL }
	ic, err := desk.node.Invite(ctx, core.TrustPublic, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guest.node.Join(ctx, ic.Code); err != nil {
		t.Fatal(err)
	}
	// The join is pushed to the owner's other devices after the answer.
	for end := time.Now().Add(5 * time.Second); len(laptop.node.guestList(false)) == 0 && time.Now().Before(end); {
		time.Sleep(5 * time.Millisecond)
	}
	if g := laptop.node.guestList(false); len(g) != 1 || g[0].Trust != core.TrustPublic {
		t.Fatalf("laptop's guests %+v", g)
	}
	if _, err := laptop.node.RevokeGuest(ctx, guest.id); err != nil {
		t.Fatal(err)
	}
	if g := desk.node.guestList(false); len(g) != 0 {
		t.Fatalf("desk still has active guests %+v after laptop revoked", g)
	}
	// A guest can't push a table that re-admits itself.
	b := joinBody(t, guestsMsg{Guests: []Guest{{Node: guest.id, UserID: 1002, Login: "bob@example.com", Trust: core.TrustTrusted,
		Joined: desk.clock.Now(), Updated: desk.clock.Now().Add(time.Hour)}}})
	if rec := do(t, desk.node.handler(), "POST", "/v1/guests", "100.64.1.3:1", b, "home"); rec.Code != http.StatusForbidden {
		t.Fatalf("guest pushing the guest table: %d", rec.Code)
	}
	if g := desk.node.guestList(false); len(g) != 0 {
		t.Fatalf("desk re-admitted %+v", g)
	}
}

// A removal wins over a record another owner device stamped with a clock
// that runs ahead (review F1).
func TestRevokeBeatsAFastClock(t *testing.T) {
	a, _, clk, _ := lendHost(t)
	b, _, _, _ := lendHost(t)
	b.now = func() time.Time { return clk.Now().Add(10 * time.Minute) }
	_, sec := mkInvite(t, b, core.TrustTrusted, nil, time.Hour)
	if rec := do(t, b.handler(), "POST", "/v1/join", bobAddr, joinBody(t, joinRequest{Code: sec, Device: "bob-pc"}), ""); rec.Code != 200 {
		t.Fatalf("join on b: %d %s", rec.Code, rec.Body)
	}
	a.mergeGuests(b.guestList(true))
	if _, err := a.RevokeGuest(context.Background(), "bob-pc"); err != nil {
		t.Fatal(err)
	}
	b.mergeGuests(a.guestList(true))
	a.mergeGuests(b.guestList(true))
	for name, n := range map[string]*Node{"a": a, "b": b} {
		if g := n.guestList(false); len(g) != 0 {
			t.Fatalf("%s still has the removed guest active: %+v", name, g)
		}
	}
}

// A lent device that announces a sleeping owner device's name neither
// shadows it for wakes nor takes it at the broker (review F2, F3).
func TestGuestCantShadowASleepingOwnerDevice(t *testing.T) {
	n, _, clk, _ := lendHost(t)
	_, sec := mkInvite(t, n, core.TrustTrusted, nil, time.Hour)
	if rec := do(t, n.handler(), "POST", "/v1/join", bobAddr, joinBody(t, joinRequest{Code: sec}), ""); rec.Code != 200 {
		t.Fatalf("join: %d", rec.Code)
	}
	n.noteKnown("n-desk", helloResponse{Device: "desk", Pool: "home", Wake: wakeInfo("a4:5e:60:00:00:07", homeSubnet, homeBcast)},
		kindOwner, "alice@example.com", clk.Now().Add(-time.Hour))
	n.noteKnown(bob.ID, helloResponse{Device: "desk", Pool: "home"}, kindGuest, bob.LoginName, clk.Now())
	n.noteKnown("n-nas", helloResponse{Device: "nas", Pool: "home", Wake: wakeInfo("a4:5e:60:00:00:08", homeSubnet, homeBcast)},
		kindGuest, bob.LoginName, clk.Now())
	if d, ok := n.knownByName("desk"); !ok || d.ID != "n-desk" {
		t.Fatalf("desk resolves to %+v, want the owner's sleeping desk", d)
	}
	if d, ok := n.knownByName("nas"); ok {
		t.Fatalf("a guest's record stands for a wake target: %+v", d)
	}
	if !n.lastSeen("desk").Equal(clk.Now().Add(-time.Hour)) {
		t.Fatal("a guest's hello made the sleeping desk look awake")
	}
	// The guest's node announces "desk" while desk sleeps.
	n.peersMu.Lock()
	n.peers[bob.ID] = &peerState{id: bob.ID, kind: kindGuest, ips: []netip.Addr{netip.MustParseAddr("100.64.0.5")},
		hello: helloResponse{Device: "desk", Pool: "home"}}
	n.peersMu.Unlock()
	n.discMu.Lock()
	n.discAt = n.now()
	n.discMu.Unlock()
	b := joinBody(t, core.WorkRequest{Device: "desk", Free: 1, Platforms: []core.Platform{{OS: "linux", Arch: "x64"}}})
	if rec := do(t, n.handler(), "POST", "/v1/work", bobAddr, b, "home"); rec.Code != http.StatusForbidden {
		t.Fatalf("a guest acting as the sleeping desk: %d, want 403", rec.Code)
	}
}

// The pool owner sees a lent device's reduced status only (review F4).
func TestHostSeesLentStatusOnly(t *testing.T) {
	log, _ := debugLog()
	ts := newFakeTS()
	n := New(Options{Device: "alice-mac", Pool: "mine", Tailscale: ts, StateDir: t.TempDir(), Log: log,
		Status:     func() any { return map[string]string{"secret_repo": "alice/private"} },
		LentStatus: func() any { return map[string]int{"slots": 2} }})
	n.lent = &Lent{Pool: "bobs", Host: "bob-pc", HostNode: bob.ID, HostAddr: "100.64.0.5:7433", OwnerUserID: bob.UserID,
		OwnerLogin: bob.LoginName, Trust: core.TrustTrusted, Joined: n.now()}
	rec := do(t, n.handler(), "GET", "/v1/status", bobAddr, nil, "bobs")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "private") || !strings.Contains(rec.Body.String(), `"slots":2`) {
		t.Fatalf("host's status view: %d %s", rec.Code, rec.Body)
	}
}

// Wake facts recorded for another user's node before this version are
// dropped at the next discovery (#58).
func TestDiscoveryDropsForeignWakeFacts(t *testing.T) {
	n, _, clk, _ := lendHost(t)
	n.known.mu.Lock()
	n.known.devs[bob.ID] = &knownDevice{ID: bob.ID, Device: "bob-pc", LastSeen: clk.Now(),
		Wake: wakeInfo("a4:5e:60:00:00:05", homeSubnet, homeBcast)}
	n.known.devs[alice.ID] = &knownDevice{ID: alice.ID, Device: "alice-linux", LastSeen: clk.Now(),
		Wake: wakeInfo("a4:5e:60:00:00:02", homeSubnet, homeBcast)}
	n.known.mu.Unlock()
	n.o.Tailscale.(*fakeTS).set(func(f *fakeTS) {
		f.peers = []Peer{{ID: bob.ID, UserID: bob.UserID, LoginName: bob.LoginName}, {ID: alice.ID, UserID: alice.UserID, LoginName: alice.LoginName}}
	})
	n.discover(context.Background())
	if d, _ := n.knownByID(bob.ID); d.Wake != nil {
		t.Fatal("another user's wake facts survived discovery")
	}
	if d, _ := n.knownByID(alice.ID); d.Wake == nil {
		t.Fatal("the same user's wake facts were dropped")
	}
}
