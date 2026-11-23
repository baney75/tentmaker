package mesh

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// fakeSender records magic packets instead of sending them: no test opens
// a socket to the network.
type fakeSender struct {
	mu   sync.Mutex
	sent []sentPacket
	err  error
}

type sentPacket struct{ MAC, Broadcast string }

func (f *fakeSender) Send(ctx context.Context, mac net.HardwareAddr, bcast netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sentPacket{mac.String(), bcast.String()})
	return nil
}

func (f *fakeSender) packets() []sentPacket {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentPacket(nil), f.sent...)
}

const (
	homeSubnet = "192.168.1.0/24"
	homeBcast  = "192.168.1.255"
	deskMAC    = "a4:5e:60:00:00:01"
)

func wakeInfo(mac, subnet, bcast string) *WakeInfo {
	return &WakeInfo{
		LAN:       LAN{Interface: "enp3s0", MAC: mac, Subnet: subnet, Broadcast: bcast, Wired: true, Wakeable: true},
		Power:     &core.PowerState{Known: true, OnAC: true},
		Labels:    []string{"self-hosted", "tentmaker"},
		Platforms: []core.Platform{{OS: "linux", Arch: "x64"}},
		Slots:     1,
	}
}

func linuxNeed(key string, since time.Time) WakeNeed {
	return WakeNeed{Key: key, Job: "alice/app: " + key, Since: since,
		Fits: func(labels []string, plats []core.Platform) bool {
			for _, p := range plats {
				if p.OS == "linux" {
					return true
				}
			}
			return false
		}}
}

// asleep records a device on nodes as discovery did before it went to
// sleep at seen.
func asleep(name string, w *WakeInfo, seen time.Time, nodes ...*Node) {
	for _, n := range nodes {
		n.noteKnown("n-"+name, helloResponse{Device: name, Pool: n.o.Pool, Wake: w}, seen)
	}
}

// heartbeat runs every member's heartbeat: its wake facts, then discovery.
func heartbeat(ms []*member) {
	for _, m := range ms {
		m.node.refreshWake(context.Background())
	}
	for _, m := range ms {
		m.node.discover(context.Background())
	}
}

// warm runs heartbeats until every member's view of the pool is fresh.
func warm(ms []*member) {
	heartbeat(ms)
	ms[0].clock.Add(awakeWithin)
	heartbeat(ms)
}

type needList struct {
	mu    sync.Mutex
	needs []WakeNeed
}

func (l *needList) set(n ...WakeNeed) { l.mu.Lock(); l.needs = n; l.mu.Unlock() }
func (l *needList) get() []WakeNeed {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]WakeNeed(nil), l.needs...)
}

func TestWakeAsksAPeerOnTheSleepersLAN(t *testing.T) {
	// The broker is away from home; the desk sleeps at home, where a
	// laptop is awake.
	needs := &needList{}
	away, home := &fakeSender{}, &fakeSender{}
	ms := newCluster(t,
		nodeSpec{device: "broker", pool: "home", broker: &fakeBroker{}, wake: wakeInfo("a4:5e:60:00:00:09", "10.9.0.0/24", "10.9.0.255"), sender: away, needs: needs.get},
		nodeSpec{device: "laptop", pool: "home", wake: wakeInfo("a4:5e:60:00:00:02", homeSubnet, homeBcast), sender: home},
	)
	broker, laptop := ms[0], ms[1]
	clk := broker.clock
	asleep("desk", wakeInfo(deskMAC, homeSubnet, homeBcast), clk.Now().Add(-10*time.Minute), broker.node, laptop.node)
	warm(ms)

	needs.set(linuxNeed("job1", clk.Now()))
	broker.node.WakeTick(context.Background())
	if p := home.packets(); len(p) != 1 || p[0] != (sentPacket{deskMAC, homeBcast}) {
		t.Fatalf("laptop sent %+v", p)
	}
	if p := away.packets(); len(p) != 0 {
		t.Fatalf("the broker sent on another LAN: %+v", p)
	}
	st := broker.node.WakeStatus()
	var desk WakeDevice
	for _, d := range st.Devices {
		if d.Device == "desk" {
			desk = d
		}
	}
	if desk.Last == nil || desk.Last.Result != WakeSent || desk.Last.Sender != "laptop" || !desk.Last.LastWake.Equal(clk.Now()) {
		t.Fatalf("desk record: %+v", desk.Last)
	}
	if st.Counts[WakeSent] != 1 {
		t.Errorf("counts %+v", st.Counts)
	}

	// The desk is on its way: the job is covered, no second packet.
	clk.Add(time.Minute)
	heartbeat(ms)
	broker.node.WakeTick(context.Background())
	if n := len(home.packets()); n != 1 {
		t.Fatalf("sent again while the desk was on its way: %d packets", n)
	}

	// No answer in 3 minutes: a timeout, and the next try backs off to 20.
	clk.Add(2 * time.Minute)
	heartbeat(ms)
	broker.node.WakeTick(context.Background())
	r, _ := broker.node.wakes.record("desk")
	if r.Result != WakeTimeout || r.Misses != 1 || !r.NextAllowed.Equal(r.LastWake.Add(20*time.Minute)) {
		t.Fatalf("after 3 minutes: %+v", r)
	}
	if n := len(home.packets()); n != 1 {
		t.Fatalf("woke again before the backoff ended: %d packets", n)
	}
	clk.Add(14 * time.Minute) // 17 minutes after the first wake
	heartbeat(ms)
	broker.node.WakeTick(context.Background())
	if n := len(home.packets()); n != 1 {
		t.Fatalf("woke again inside the backoff: %d packets", n)
	}
	clk.Add(4 * time.Minute) // 21 minutes
	heartbeat(ms)
	broker.node.WakeTick(context.Background())
	if n := len(home.packets()); n != 2 {
		t.Fatalf("no second wake after the backoff: %d packets", n)
	}

	// The desk answers 40 s later: online, and its misses reset.
	clk.Add(40 * time.Second)
	broker.node.noteKnown("n-desk", helloResponse{Device: "desk", Pool: "home", Wake: wakeInfo(deskMAC, homeSubnet, homeBcast)}, clk.Now())
	needs.set()
	broker.node.WakeTick(context.Background())
	r, _ = broker.node.wakes.record("desk")
	if r.Result != WakeOnline || r.Misses != 0 || r.OnlineAfterS != 40 {
		t.Fatalf("online: %+v", r)
	}
	c := broker.node.wakes.Counts()
	if c[WakeSent] != 2 || c[WakeTimeout] != 1 || c[WakeOnline] != 1 || c[WakeFailed] != 0 {
		t.Errorf("counts %+v", c)
	}
}

func TestWakeFromThisDeviceWhenItSharesTheLAN(t *testing.T) {
	needs := &needList{}
	snd := &fakeSender{}
	ms := newCluster(t, nodeSpec{device: "broker", pool: "home", broker: &fakeBroker{},
		wake: wakeInfo("a4:5e:60:00:00:09", homeSubnet, homeBcast), sender: snd, needs: needs.get})
	n, clk := ms[0].node, ms[0].clock
	asleep("desk", wakeInfo(deskMAC, homeSubnet, homeBcast), clk.Now(), n)
	warm(ms)
	needs.set(linuxNeed("job1", clk.Now()))
	n.WakeTick(context.Background())
	if p := snd.packets(); len(p) != 1 || p[0] != (sentPacket{deskMAC, homeBcast}) {
		t.Fatalf("sent %+v", p)
	}
	if r, _ := n.wakes.record("desk"); r.Sender != "broker" || r.Result != WakeSent {
		t.Errorf("record %+v", r)
	}
}

func TestWakeRecordsAFailureWithNoSenderOnTheLAN(t *testing.T) {
	needs := &needList{}
	ms := newCluster(t, nodeSpec{device: "broker", pool: "home", broker: &fakeBroker{},
		wake: wakeInfo("a4:5e:60:00:00:09", "10.9.0.0/24", "10.9.0.255"), sender: &fakeSender{}, needs: needs.get})
	n, clk := ms[0].node, ms[0].clock
	asleep("desk", wakeInfo(deskMAC, homeSubnet, homeBcast), clk.Now(), n)
	warm(ms)
	needs.set(linuxNeed("job1", clk.Now()))
	n.WakeTick(context.Background())
	r, _ := n.wakes.record("desk")
	if r.Result != WakeFailed || !strings.Contains(r.Error, "no awake pool device shares its LAN") || r.Misses != 1 {
		t.Fatalf("record %+v", r)
	}
	// Rate limited: the next tick doesn't try again.
	n.WakeTick(context.Background())
	if c := n.wakes.Counts(); c[WakeFailed] != 1 {
		t.Errorf("counts %+v", c)
	}
}

func TestWakeEachDeviceCoversItsSlots(t *testing.T) {
	needs := &needList{}
	snd := &fakeSender{}
	ms := newCluster(t, nodeSpec{device: "broker", pool: "home", broker: &fakeBroker{},
		wake: wakeInfo("a4:5e:60:00:00:09", homeSubnet, homeBcast), sender: snd, needs: needs.get})
	n, clk := ms[0].node, ms[0].clock
	big := wakeInfo(deskMAC, homeSubnet, homeBcast)
	big.Slots = 2
	asleep("desk", big, clk.Now(), n)
	asleep("nuc", wakeInfo("a4:5e:60:00:00:03", homeSubnet, homeBcast), clk.Now().Add(-time.Hour), n)
	asleep("mini", wakeInfo("a4:5e:60:00:00:04", homeSubnet, homeBcast), clk.Now().Add(-2*time.Hour), n)
	warm(ms)
	// Three jobs: the desk (2 slots, seen last) takes two, the nuc one; the
	// mini stays asleep.
	now := clk.Now()
	needs.set(linuxNeed("a", now), linuxNeed("b", now.Add(time.Second)), linuxNeed("c", now.Add(2*time.Second)))
	n.WakeTick(context.Background())
	got := map[string]bool{}
	for _, p := range snd.packets() {
		got[p.MAC] = true
	}
	if len(snd.packets()) != 2 || !got[deskMAC] || !got["a4:5e:60:00:00:03"] {
		t.Fatalf("sent %+v", snd.packets())
	}
}

func TestWakeNeedsAFreshView(t *testing.T) {
	needs := &needList{}
	snd := &fakeSender{}
	ms := newCluster(t, nodeSpec{device: "broker", pool: "home", broker: &fakeBroker{},
		wake: wakeInfo("a4:5e:60:00:00:09", homeSubnet, homeBcast), sender: snd, needs: needs.get})
	n, clk := ms[0].node, ms[0].clock
	asleep("desk", wakeInfo(deskMAC, homeSubnet, homeBcast), clk.Now().Add(-time.Hour), n)
	needs.set(linuxNeed("job1", clk.Now()))
	// Never discovered: every device would look asleep. Wake nobody.
	n.WakeTick(context.Background())
	// Discovered just now: a device may answer within awakeWithin.
	heartbeat(ms)
	n.WakeTick(context.Background())
	// Discovery stopped (tailscale down) long ago: stale, wake nobody.
	clk.Add(awakeWithin)
	heartbeat(ms)
	clk.Add(5 * time.Minute)
	n.WakeTick(context.Background())
	if p := snd.packets(); len(p) != 0 {
		t.Fatalf("woke with a stale view: %+v", p)
	}
	heartbeat(ms)
	n.WakeTick(context.Background())
	if p := snd.packets(); len(p) != 1 {
		t.Fatalf("fresh view, no wake: %+v", p)
	}
}

func TestWhyNotWake(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ok := func() *WakeInfo { return wakeInfo(deskMAC, homeSubnet, homeBcast) }
	cases := []struct {
		name string
		edit func(w *WakeInfo)
		seen time.Duration
		want string
	}{
		{"fits", func(w *WakeInfo) {}, time.Hour, ""},
		{"battery", func(w *WakeInfo) { w.Power = &core.PowerState{Known: true, HasBattery: true, BatteryPercent: 90} }, time.Hour, "on battery"},
		{"desktop with no AC entry", func(w *WakeInfo) { w.Power = &core.PowerState{Known: true} }, time.Hour, ""},
		{"power unknown", func(w *WakeInfo) { w.Power = &core.PowerState{} }, time.Hour, "power state was unknown"},
		{"power unread", func(w *WakeInfo) { w.Power = nil }, time.Hour, "power state was unknown"},
		{"do not disturb", func(w *WakeInfo) { w.DoNotDisturb = true }, time.Hour, "do not disturb"},
		{"opt out", func(w *WakeInfo) { w.OptOut = true }, time.Hour, "wake = false"},
		{"no MAC", func(w *WakeInfo) { w.LAN.MAC = "" }, time.Hour, "no LAN interface"},
		{"not wakeable", func(w *WakeInfo) { w.LAN.Wakeable, w.LAN.Note = false, "wakeup is disabled" }, time.Hour, "wakeup is disabled"},
		{"no slots", func(w *WakeInfo) { w.Slots = 0 }, time.Hour, "fits no job"},
		{"no platforms", func(w *WakeInfo) { w.Platforms = nil }, time.Hour, "no platforms"},
		// now is 08:00 UTC. An off night that ends at 07:00 UTC has ended;
		// read five hours west it is 03:00 there, inside it (review second-F6).
		{"off window over", func(w *WakeInfo) { w.Off = []config.Window{{From: "22:00", To: "07:00", Profile: "off"}} }, time.Hour, ""},
		{"off window now", func(w *WakeInfo) {
			w.Off, w.UTCOffsetS = []config.Window{{From: "22:00", To: "07:00", Profile: "off"}}, -5*3600
		}, time.Hour, "schedule is off"},
		{"stale", func(w *WakeInfo) {}, 8 * 24 * time.Hour, "7 days"},
	}
	for _, c := range cases {
		w := ok()
		c.edit(w)
		got := whyNotWake(knownDevice{Device: "desk", Wake: w, LastSeen: now.Add(-c.seen)}, now)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := whyNotWake(knownDevice{Device: "old"}, now); !strings.Contains(got, "older Tentmaker") {
		t.Errorf("no wake info: %q", got)
	}
}

func TestWakeNeverWakesABatteryDevice(t *testing.T) {
	needs := &needList{}
	snd := &fakeSender{}
	ms := newCluster(t, nodeSpec{device: "broker", pool: "home", broker: &fakeBroker{},
		wake: wakeInfo("a4:5e:60:00:00:09", homeSubnet, homeBcast), sender: snd, needs: needs.get})
	n, clk := ms[0].node, ms[0].clock
	lap := wakeInfo(deskMAC, homeSubnet, homeBcast)
	lap.Power = &core.PowerState{Known: true, HasBattery: true, BatteryPercent: 100}
	asleep("laptop", lap, clk.Now(), n)
	dnd := wakeInfo("a4:5e:60:00:00:05", homeSubnet, homeBcast)
	dnd.DoNotDisturb = true
	asleep("studio", dnd, clk.Now(), n)
	warm(ms)
	needs.set(linuxNeed("job1", clk.Now()))
	n.WakeTick(context.Background())
	if p := snd.packets(); len(p) != 0 {
		t.Fatalf("woke a device on battery or do not disturb: %+v", p)
	}
	// Even asked directly, this device won't send to them.
	for _, d := range []string{"laptop", "studio"} {
		if _, err := n.sendWake(context.Background(), d, "test"); err == nil {
			t.Errorf("%s: sent", d)
		}
	}
	st := n.WakeStatus()
	for _, d := range st.Devices {
		if d.CanWake || d.Why == "" {
			t.Errorf("status says %s can wake: %+v", d.Device, d)
		}
	}
}

// wakeEndpoint is a node that sends wakes, a sleeping desk it knows, and a
// known pool peer at 100.64.0.2 (alice) to ask it.
func wakeEndpoint(t *testing.T, self *WakeInfo) (*Node, *fakeSender, *clock) {
	t.Helper()
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	snd := &fakeSender{}
	n := New(Options{Device: "laptop", Pool: "home", Tailscale: newFakeTS(), Sender: snd,
		Wake: func(context.Context) WakeInfo { return *self }})
	n.now = clk.Now
	n.refreshWake(context.Background())
	knowPeer(n, "n2", "100.64.0.2", "broker")
	asleep("desk", wakeInfo(deskMAC, homeSubnet, homeBcast), clk.Now().Add(-10*time.Minute), n)
	return n, snd, clk
}

func TestWakeEndpointUsesOnlyItsOwnRecords(t *testing.T) {
	n, snd, clk := wakeEndpoint(t, wakeInfo("a4:5e:60:00:00:02", "192.168.1.77/24", homeBcast))
	// The body names a MAC and a broadcast address; both are ignored.
	rec := do(t, n.handler(), "POST", "/v1/wake", "100.64.0.2:1",
		strings.NewReader(`{"device":"desk","mac":"de:ad:be:ef:00:01","broadcast":"255.255.255.255"}`), "home")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"sent":true`) {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if p := snd.packets(); len(p) != 1 || p[0] != (sentPacket{deskMAC, homeBcast}) {
		t.Fatalf("sent %+v", p)
	}
	// A second ask within 10 minutes sends nothing and says when.
	clk.Add(5 * time.Minute)
	rec = do(t, n.handler(), "POST", "/v1/wake", "100.64.0.2:1", strings.NewReader(`{"device":"desk"}`), "home")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"sent":false`) || !strings.Contains(rec.Body.String(), `"already"`) {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if len(snd.packets()) != 1 {
		t.Fatal("rate limit broken")
	}
	clk.Add(5 * time.Minute)
	if rec = do(t, n.handler(), "POST", "/v1/wake", "100.64.0.2:1", strings.NewReader(`{"device":"desk"}`), "home"); rec.Code != 200 || len(snd.packets()) != 2 {
		t.Fatalf("after 10 minutes: code %d, %d packets", rec.Code, len(snd.packets()))
	}

	for _, c := range []struct {
		body string
		code int
		why  string
	}{
		{`{"device":"nobody"}`, 409, "not a known pool device"},
		{`{"device":"laptop"}`, 409, "not a known pool device"},
		{`{"device":"broker"}`, 409, "it is awake"},
		{`{"device":""}`, 400, "device must be"},
		{`{"mac":"de:ad:be:ef:00:01"}`, 400, "device must be"},
	} {
		if c.body == `{"device":"broker"}` {
			n.noteKnown("n2", helloResponse{Device: "broker", Pool: "home", Wake: wakeInfo("a4:5e:60:00:00:07", homeSubnet, homeBcast)}, clk.Now())
		}
		rec := do(t, n.handler(), "POST", "/v1/wake", "100.64.0.2:1", strings.NewReader(c.body), "home")
		if rec.Code != c.code || !strings.Contains(errText(rec), c.why) {
			t.Errorf("%s: code %d %q", c.body, rec.Code, errText(rec))
		}
	}
	if len(snd.packets()) != 2 {
		t.Errorf("refused requests sent packets: %+v", snd.packets())
	}
}

func TestWakeEndpointRefusesOtherLANsAndStrangers(t *testing.T) {
	n, snd, _ := wakeEndpoint(t, wakeInfo("a4:5e:60:00:00:02", "10.9.0.0/24", "10.9.0.255"))
	rec := do(t, n.handler(), "POST", "/v1/wake", "100.64.0.2:1", strings.NewReader(`{"device":"desk"}`), "home")
	if rec.Code != 409 || !strings.Contains(errText(rec), "not on this device's LAN") {
		t.Fatalf("other LAN: code %d %q", rec.Code, errText(rec))
	}
	// Another tailnet user: refused before anything else.
	if rec := do(t, n.handler(), "POST", "/v1/wake", "100.64.0.5:1", strings.NewReader(`{"device":"desk"}`), "home"); rec.Code != 403 {
		t.Errorf("other user: code %d", rec.Code)
	}
	// The same user's node that is not a pool device: refused.
	n2, snd2, _ := wakeEndpoint(t, wakeInfo("a4:5e:60:00:00:02", homeSubnet, homeBcast))
	n2.peersMu.Lock()
	n2.peers = map[string]*peerState{}
	n2.peersMu.Unlock()
	if rec := do(t, n2.handler(), "POST", "/v1/wake", "100.64.0.2:1", strings.NewReader(`{"device":"desk"}`), "home"); rec.Code != 403 {
		t.Errorf("not a pool device: code %d", rec.Code)
	}
	// No sender: no endpoint.
	n3 := New(Options{Device: "x", Pool: "home", Tailscale: newFakeTS()})
	knowPeer(n3, "n2", "100.64.0.2", "broker")
	if rec := do(t, n3.handler(), "POST", "/v1/wake", "100.64.0.2:1", strings.NewReader(`{"device":"desk"}`), "home"); rec.Code != 409 {
		t.Errorf("no sender: code %d", rec.Code)
	}
	if len(snd.packets())+len(snd2.packets()) != 0 {
		t.Error("a refused request sent a packet")
	}
}

func TestWakeSendErrorIsNotRateLimited(t *testing.T) {
	n, snd, _ := wakeEndpoint(t, wakeInfo("a4:5e:60:00:00:02", homeSubnet, homeBcast))
	snd.err = errors.New("network is unreachable")
	rec := do(t, n.handler(), "POST", "/v1/wake", "100.64.0.2:1", strings.NewReader(`{"device":"desk"}`), "home")
	if rec.Code != 502 || strings.Contains(errText(rec), "unreachable") {
		t.Fatalf("code %d %q", rec.Code, errText(rec))
	}
	snd.err = nil
	if rec := do(t, n.handler(), "POST", "/v1/wake", "100.64.0.2:1", strings.NewReader(`{"device":"desk"}`), "home"); rec.Code != 200 || len(snd.packets()) != 1 {
		t.Fatalf("retry after a send error: code %d", rec.Code)
	}
}

func TestKnownDevicesSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	n := New(Options{Device: "broker", Pool: "home", Tailscale: newFakeTS(), StateDir: dir})
	seen := time.Unix(1_800_000_000, 0)
	asleep("desk", wakeInfo(deskMAC, homeSubnet, homeBcast), seen, n)
	n.saveKnown(seen)
	n2 := New(Options{Device: "broker", Pool: "home", Tailscale: newFakeTS(), StateDir: dir})
	d, ok := n2.knownByName("desk")
	if !ok || d.Wake == nil || d.Wake.LAN.MAC != deskMAC || !d.LastSeen.Equal(seen) {
		t.Fatalf("after restart: %+v %v", d, ok)
	}
	// A device gone 30 days is forgotten.
	n2.saveKnown(seen.Add(31 * 24 * time.Hour))
	n3 := New(Options{Device: "broker", Pool: "home", Tailscale: newFakeTS(), StateDir: dir})
	if _, ok := n3.knownByName("desk"); ok {
		t.Error("a device gone 30 days was kept")
	}
}

func TestHelloCarriesWakeInfo(t *testing.T) {
	w := wakeInfo(deskMAC, homeSubnet, homeBcast)
	n := New(Options{Device: "desk", Pool: "home", Tailscale: newFakeTS(), Wake: func(context.Context) WakeInfo { return *w }})
	n.refreshWake(context.Background())
	rec := do(t, n.handler(), "GET", "/v1/hello", "100.64.0.2:1", nil, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"mac":"`+deskMAC+`"`) || !strings.Contains(rec.Body.String(), `"subnet":"`+homeSubnet+`"`) {
		t.Fatalf("hello: %s", rec.Body)
	}
}

// A device woken for a job that comes online and doesn't take it is a
// miss when the same job wakes it again, so it isn't woken every
// wakeEvery all night (review second-F6).
func TestRepeatWakeForTheSameJobBacksOff(t *testing.T) {
	l := NewWakeLog()
	now := time.Unix(1_800_000_000, 0)
	wake := func(job string) time.Duration {
		l.attempt("desk", job, "broker", WakeSent, "", now)
		at := now.Add(time.Minute) // it comes online, then sleeps again
		l.settle(at, func(string) time.Time { return at })
		got := l.recs["desk"].NextAllowed.Sub(now)
		now = now.Add(8 * time.Hour)
		return got
	}
	for i, want := range []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute} {
		if got := wake("acme/w: build"); got != want {
			t.Fatalf("wake %d for the same job: next in %v, want %v", i, got, want)
		}
	}
	if got := wake("acme/w: test"); got != wakeEvery {
		t.Fatalf("a wake for another job: next in %v, want %v", got, wakeEvery)
	}
}

func TestBackoff(t *testing.T) {
	for misses, want := range map[int]time.Duration{0: 10 * time.Minute, 1: 20 * time.Minute, 2: 40 * time.Minute, 5: 4 * time.Hour, 50: 4 * time.Hour} {
		if got := backoff(misses); got != want {
			t.Errorf("%d misses: %v, want %v", misses, got, want)
		}
	}
}
