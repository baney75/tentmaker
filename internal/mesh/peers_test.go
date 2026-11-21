package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type nodeSpec struct {
	device string
	pool   string
	broker *fakeBroker
	user   int64 // 0 = alice (1001)
	online *bool
}

// member is one device in a test cluster: a Node behind a loopback server
// whose handler the test can swap out.
type member struct {
	spec  nodeSpec
	id    string
	node  *Node
	srv   *httptest.Server
	ts    *fakeTS
	logs  *syncBuf
	hits  atomic.Int32
	fail  atomic.Bool // answer 500 to everything
	hang  atomic.Bool // hang /v1/status until the caller gives up
	clock *clock
}

func newCluster(t *testing.T, specs ...nodeSpec) []*member {
	t.Helper()
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	ms := make([]*member, len(specs))
	urls := map[string]string{}
	for i, s := range specs {
		m := &member{spec: s, id: fmt.Sprintf("n-%s", s.device), clock: clk}
		ms[i] = m
		m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.hits.Add(1)
			if m.fail.Load() {
				writeErr(w, 500, "broken")
				return
			}
			if from := r.Header.Get(testFromHeader); from != "" {
				r.RemoteAddr = from + ":40000"
			}
			if m.hang.Load() && r.URL.Path == "/v1/status" {
				<-r.Context().Done()
				return
			}
			m.node.handler().ServeHTTP(w, r)
		}))
		t.Cleanup(m.srv.Close)
		urls[m.id] = m.srv.URL
	}
	for i, m := range ms {
		user := m.spec.user
		if user == 0 {
			user = 1001
		}
		m.ts = newFakeTS()
		m.ts.self = Peer{ID: m.id, HostName: m.spec.device, Online: true, UserID: 1001, LoginName: "alice@example.com",
			IPs: []netip.Addr{netip.MustParseAddr(fmt.Sprintf("100.64.1.%d", i+1))}}
		for j, o := range ms {
			if j == i {
				continue
			}
			ou := o.spec.user
			if ou == 0 {
				ou = 1001
			}
			online := o.spec.online == nil || *o.spec.online
			login := "alice@example.com"
			if ou != 1001 {
				login = "bob@example.com"
			}
			m.ts.peers = append(m.ts.peers, Peer{ID: o.id, HostName: o.spec.device, Online: online, UserID: ou, LoginName: login,
				IPs: []netip.Addr{netip.MustParseAddr(fmt.Sprintf("100.64.1.%d", j+1))}})
		}
		var log, buf = debugLog()
		m.logs = buf
		opts := Options{Device: m.spec.device, Pool: m.spec.pool, Tailscale: m.ts, Version: "1.0.0", Log: log,
			Status: func() any { return map[string]string{"device": m.spec.device} }}
		if m.spec.broker != nil {
			opts.Broker = m.spec.broker
		}
		m.node = New(opts)
		m.node.now = clk.Now
		m.node.callTimeout = 2 * time.Second
		m.node.peerURL = func(p Peer) string { return urls[p.ID] }
		// Every member's calls arrive from its own tailnet address, as
		// they would on a real tailnet, not all from 127.0.0.1.
		from := fmt.Sprintf("100.64.1.%d", i+1)
		m.node.client.Transport = fromTransport{from, m.node.client.Transport}
		m.node.stream.Transport = fromTransport{from, m.node.stream.Transport}
		for j, o := range ms {
			ou, login := o.spec.user, "alice@example.com"
			if ou == 0 {
				ou = 1001
			}
			if ou != 1001 {
				login = "bob@example.com"
			}
			m.ts.ids[fmt.Sprintf("100.64.1.%d", j+1)] = Identity{Node: o.spec.device + ".example-tailnet.ts.net", HostName: o.spec.device, UserID: ou, LoginName: login}
		}
	}
	return ms
}

// testFromHeader carries a test member's tailnet address to the loopback
// server, which makes it the request's RemoteAddr.
const testFromHeader = "X-Test-From"

type fromTransport struct {
	from string
	rt   http.RoundTripper
}

func (t fromTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(testFromHeader, t.from)
	return t.rt.RoundTrip(r)
}

func names(srcs []core.WorkSource) []string {
	var out []string
	for _, s := range srcs {
		out = append(out, s.Name())
	}
	return out
}

func TestBrokersAndRemoteWork(t *testing.T) {
	secret := []byte("JIT-secret-for-the-runner-abcdef")
	off := false
	ms := newCluster(t,
		nodeSpec{device: "a-worker", pool: "home"},
		nodeSpec{device: "b-broker", pool: "home", broker: &fakeBroker{secret: secret}},
		nodeSpec{device: "c-other-pool", pool: "work", broker: &fakeBroker{secret: secret}},
		nodeSpec{device: "d-bob", pool: "home", broker: &fakeBroker{secret: secret}, user: 1002},
		nodeSpec{device: "e-offline", pool: "home", broker: &fakeBroker{secret: secret}, online: &off},
	)
	a := ms[0]
	srcs := a.node.Brokers(context.Background())
	if got := names(srcs); len(got) != 1 || got[0] != "mesh:b-broker" {
		t.Fatalf("brokers: %v", got)
	}
	if ms[3].hits.Load() != 0 {
		t.Error("another user's device was probed")
	}
	if ms[4].hits.Load() != 0 {
		t.Error("an offline device was probed")
	}
	resp, err := srcs[0].Work(context.Background(), core.WorkRequest{Device: "a-worker", Free: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Offers) != 1 || string(resp.Offers[0].Grant.Secret) != string(secret) {
		t.Fatalf("grant secret not restored: %+v", resp.Offers)
	}
	if resp.Offers[0].Secret != nil {
		t.Error("wire secret left on the offer")
	}
	if resp.Offers[0].Grant.RunnerName != "tm-alice-mac-1" || len(resp.Stop) != 1 {
		t.Errorf("response: %+v", resp)
	}
	for _, m := range ms {
		assertNoSecret(t, m.logs.String(), secret)
	}

	// The pool view sees self and the one pool peer.
	pool, err := a.node.Pool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 2 || !pool[0].Self || pool[0].Device != "a-worker" || pool[1].Device != "b-broker" || !pool[1].Broker {
		t.Fatalf("pool: %+v", pool)
	}
	var st map[string]string
	if err := json.Unmarshal(pool[1].Status, &st); err != nil || st["device"] != "b-broker" {
		t.Fatalf("peer status: %s %v", pool[1].Status, err)
	}
}

func TestPeerSkippedAfterThreeFailures(t *testing.T) {
	ms := newCluster(t,
		nodeSpec{device: "a", pool: "home"},
		nodeSpec{device: "b", pool: "home", broker: &fakeBroker{secret: []byte("x")}},
	)
	a, b := ms[0], ms[1]
	srcs := a.node.Brokers(context.Background())
	if len(srcs) != 1 {
		t.Fatalf("brokers: %v", names(srcs))
	}
	b.fail.Store(true)
	for i := 0; i < 3; i++ {
		if _, err := srcs[0].Work(context.Background(), core.WorkRequest{Device: "a"}); err == nil {
			t.Fatal("want error")
		}
	}
	if got := a.node.Brokers(context.Background()); len(got) != 0 {
		t.Fatalf("failing broker not skipped: %v", names(got))
	}
	// Discovery during the skip does not call it.
	hits := b.hits.Load()
	a.clock.Add(31 * time.Second)
	a.node.Brokers(context.Background())
	if b.hits.Load() != hits {
		t.Error("skipped peer was probed")
	}
	// After a minute it gets another chance.
	b.fail.Store(false)
	a.clock.Add(30 * time.Second)
	if got := a.node.Brokers(context.Background()); len(got) != 1 {
		t.Fatalf("peer not back after the skip: %v", names(got))
	}
}

func TestClaimAcrossBrokers(t *testing.T) {
	ms := newCluster(t,
		nodeSpec{device: "a", pool: "home", broker: &fakeBroker{}},
		nodeSpec{device: "b", pool: "home", broker: &fakeBroker{}},
	)
	a, b := ms[0], ms[1]
	ctx := context.Background()
	job := core.Job{Forge: "github", Repo: "alice/app", ID: 99}
	if !a.node.Claim(ctx, job, "worker-1", time.Minute) {
		t.Fatal("first claim refused")
	}
	if b.node.Claim(ctx, job, "worker-2", time.Minute) {
		t.Fatal("second device won a held job")
	}
	if b.node.leases.holder(job.Key(), b.clock.Now()) != "worker-1" {
		t.Error("claim did not reach the peer's table")
	}
	// The holder renews.
	if !b.node.Claim(ctx, job, "worker-1", time.Minute) {
		t.Fatal("holder renew refused")
	}
	a.node.Release(ctx, job, "worker-1")
	if a.node.leases.holder(job.Key(), a.clock.Now()) != "" || b.node.leases.holder(job.Key(), b.clock.Now()) != "" {
		t.Fatal("release did not reach every broker")
	}
	if !b.node.Claim(ctx, job, "worker-2", time.Minute) {
		t.Fatal("claim after release refused")
	}
}

func TestClaimLossReleasesPartialWins(t *testing.T) {
	ms := newCluster(t,
		nodeSpec{device: "a", pool: "home", broker: &fakeBroker{}},
		nodeSpec{device: "b", pool: "home", broker: &fakeBroker{}},
	)
	a, b := ms[0], ms[1]
	job := core.Job{Forge: "github", Repo: "alice/app", ID: 5}
	// b's table alone says another device holds it; a wins its own table
	// first (a sorts before b), then loses at b and must undo at a.
	b.node.leases.claim(job.Key(), "other", time.Minute, b.clock.Now())
	if a.node.Claim(context.Background(), job, "worker-1", time.Minute) {
		t.Fatal("claim granted though a peer holds the job")
	}
	if h := a.node.leases.holder(job.Key(), a.clock.Now()); h != "" {
		t.Fatalf("partial win not released: %q", h)
	}
}

func TestClaimUnreachablePeersDontBlock(t *testing.T) {
	ms := newCluster(t,
		nodeSpec{device: "a", pool: "home", broker: &fakeBroker{}},
		nodeSpec{device: "b", pool: "home", broker: &fakeBroker{}},
	)
	a, b := ms[0], ms[1]
	a.node.Brokers(context.Background()) // discover b
	b.srv.Close()
	start := time.Now()
	if !a.node.Claim(context.Background(), core.Job{Forge: "github", Repo: "x/y", ID: 1}, "w", time.Minute) {
		t.Fatal("unreachable peer blocked the claim")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("claim stalled")
	}
	// No peers at all: granted by the local table alone.
	solo := New(Options{Device: "solo", Pool: "home", Tailscale: newFakeTS()})
	if !solo.Claim(context.Background(), core.Job{ID: 1}, "w", time.Minute) {
		t.Fatal("solo claim refused")
	}
	if solo.Claim(context.Background(), core.Job{ID: 1}, "w2", time.Minute) {
		t.Fatal("solo table let a second device in")
	}
}

func TestClaimConcurrentOneWinner(t *testing.T) {
	ms := newCluster(t,
		nodeSpec{device: "a", pool: "home", broker: &fakeBroker{}},
		nodeSpec{device: "b", pool: "home", broker: &fakeBroker{}},
		nodeSpec{device: "c", pool: "home", broker: &fakeBroker{}},
	)
	for _, m := range ms {
		m.node.Brokers(context.Background()) // discover before the race
	}
	for round := 0; round < 10; round++ {
		job := core.Job{Forge: "github", Repo: "alice/app", ID: int64(1000 + round)}
		var wins atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, m := range ms {
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func(m *member, dev string) {
					defer wg.Done()
					<-start
					if m.node.Claim(context.Background(), job, dev, time.Minute) {
						wins.Add(1)
					}
				}(m, fmt.Sprintf("w-%d-%d", i, g))
			}
		}
		close(start)
		wg.Wait()
		if w := wins.Load(); w != 1 {
			t.Fatalf("round %d: %d winners, want exactly 1", round, w)
		}
	}
}

func TestPoolPeerTimeout(t *testing.T) {
	ms := newCluster(t,
		nodeSpec{device: "a", pool: "home"},
		nodeSpec{device: "b", pool: "home"},
		nodeSpec{device: "c", pool: "home"},
	)
	a := ms[0]
	a.node.statusTimeout = 200 * time.Millisecond
	a.node.Brokers(context.Background()) // discover
	ms[2].hang.Store(true)
	start := time.Now()
	pool, err := a.node.Pool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("pool took %v", time.Since(start))
	}
	if len(pool) != 3 || pool[1].Device != "b" || pool[1].Error != "" || pool[2].Device != "c" || pool[2].Error == "" {
		t.Fatalf("pool: %+v", pool)
	}
}

func TestSortByLatency(t *testing.T) {
	ps := []peerRef{
		{hello: helloResponse{Device: "none"}},
		{hello: helloResponse{Device: "slow"}, latency: 300 * time.Millisecond},
		{hello: helloResponse{Device: "fast"}, latency: 5 * time.Millisecond},
	}
	sortByLatency(ps)
	if ps[0].hello.Device != "fast" || ps[1].hello.Device != "slow" || ps[2].hello.Device != "none" {
		t.Fatalf("order: %v %v %v", ps[0].hello.Device, ps[1].hello.Device, ps[2].hello.Device)
	}
}

func TestPeerURL(t *testing.T) {
	n := New(Options{Device: "a", Pool: "home", Port: 7433, Tailscale: newFakeTS()})
	p := Peer{IPs: []netip.Addr{netip.MustParseAddr("fd7a:115c:a1e0::2"), netip.MustParseAddr("100.64.0.2")}}
	if got := n.peerURL(p); got != "http://100.64.0.2:7433" {
		t.Errorf("v4 preferred: %s", got)
	}
	p.IPs = p.IPs[:1]
	if got := n.peerURL(p); got != "http://[fd7a:115c:a1e0::2]:7433" {
		t.Errorf("v6: %s", got)
	}
	if got := n.peerURL(Peer{}); got != "" {
		t.Errorf("no IP: %s", got)
	}
}

// A pool device can ask a remote broker for work only as itself: the
// device name must be the one its tailnet address announced (review r1
// partS first-F4).
func TestRemoteWorkActsOnlyAsItself(t *testing.T) {
	ref := strings.Repeat("ab", 32)
	fb := &fakeBroker{secret: []byte("s"), archives: map[string]fakeArchive{ref: {device: "a-worker", data: []byte("tar")}}}
	ms := newCluster(t,
		nodeSpec{device: "a-worker", pool: "home"},
		nodeSpec{device: "b-broker", pool: "home", broker: fb},
		nodeSpec{device: "c-worker", pool: "home"},
	)
	srcs := ms[2].node.Brokers(context.Background())
	if len(srcs) != 1 {
		t.Fatalf("brokers: %v", names(srcs))
	}
	if _, err := srcs[0].Work(context.Background(), core.WorkRequest{Device: "a-worker", Free: 1}); err == nil {
		t.Fatal("c-worker asked for work as a-worker and was served")
	}
	var buf bytes.Buffer
	if err := srcs[0].(core.ArchiveSource).FetchArchive(context.Background(), ref, "a-worker", &buf); err == nil || buf.Len() != 0 {
		t.Fatalf("c-worker fetched a-worker's archive: err=%v bytes=%d", err, buf.Len())
	}
	if _, err := srcs[0].Work(context.Background(), core.WorkRequest{Device: "c-worker", Free: 1}); err != nil {
		t.Fatalf("c-worker as itself: %v", err)
	}
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.reqs) != 1 || fb.reqs[0].Device != "c-worker" {
		t.Fatalf("broker saw %+v", fb.reqs)
	}
}
