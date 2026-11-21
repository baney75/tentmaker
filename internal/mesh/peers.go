package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

const failLimit = 3

var (
	_ core.Claimer       = (*Node)(nil)
	_ core.WorkSource    = (*remoteBroker)(nil)
	_ core.ArchiveSource = (*remoteBroker)(nil)
)

// peerState is one pool device this node has said hello to.
type peerState struct {
	id        string // tailnet node ID
	base      string // http://ip:port
	hello     helloResponse
	failures  int
	skipUntil time.Time
	latency   time.Duration // smoothed latency of recent successes; 0 = none yet
}

// DeviceStatus is one device's answer in a pool view.
type DeviceStatus struct {
	Device  string          `json:"device"`
	Self    bool            `json:"self"`
	Broker  bool            `json:"broker"`
	Version string          `json:"version"`
	Status  json.RawMessage `json:"status,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// peerRef is a snapshot of a peer for one call.
type peerRef struct {
	id, base string
	hello    helloResponse
	latency  time.Duration
	skipped  bool
}

// discover refreshes the pool: online devices of this user (or members) are
// probed with /v1/hello, and those in the same pool are kept.
func (n *Node) discover(ctx context.Context) {
	n.discMu.Lock()
	defer n.discMu.Unlock()
	n.discAt = n.now()

	self, err := n.selfPeer(ctx, false)
	if err != nil {
		n.log.Debug("mesh: discovery skipped", "err", err)
		return
	}
	pctx, cancel := context.WithTimeout(ctx, whoisTimeout)
	all, err := n.o.Tailscale.Peers(pctx)
	cancel()
	if err != nil {
		n.log.Debug("mesh: can't list tailnet peers", "err", err)
		return
	}

	type found struct {
		p     Peer
		base  string
		hello helloResponse
		err   error
		d     time.Duration
		skip  bool
	}
	var cands []*found
	now := n.now()
	n.peersMu.Lock()
	for _, p := range all {
		if !p.Online || p.ID == self.ID || !n.allowed(self, p.UserID, p.LoginName, p.Tags) {
			continue
		}
		base := n.peerURL(p)
		if base == "" {
			continue
		}
		f := &found{p: p, base: base}
		if st, ok := n.peers[p.ID]; ok && now.Before(st.skipUntil) {
			f.skip = true
		}
		cands = append(cands, f)
	}
	n.peersMu.Unlock()

	var wg sync.WaitGroup
	for _, f := range cands {
		if f.skip {
			continue
		}
		wg.Add(1)
		go func(f *found) {
			defer wg.Done()
			hctx, cancel := context.WithTimeout(ctx, n.helloTimeout)
			defer cancel()
			start := time.Now()
			f.err = n.call(hctx, http.MethodGet, f.base+"/v1/hello", nil, &f.hello)
			f.d = time.Since(start)
		}(f)
	}
	wg.Wait()

	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	next := map[string]*peerState{}
	for _, f := range cands {
		st, had := n.peers[f.p.ID]
		switch {
		case f.skip:
			next[f.p.ID] = st
		case f.err != nil:
			if had {
				st.base = f.base
				n.failLocked(st, f.err)
				next[f.p.ID] = st
			}
		case f.hello.Pool != n.o.Pool:
			// Another pool: not ours.
		default:
			if !had {
				st = &peerState{id: f.p.ID}
				n.log.Info("mesh: found a pool device", "device", f.hello.Device, "broker", f.hello.Broker)
			}
			st.base, st.hello = f.base, f.hello
			n.okLocked(st, f.d)
			next[f.p.ID] = st
		}
	}
	n.peers = next
}

// ensureFresh runs discovery if nothing has for one discovery period, so
// Brokers, Claim and Pool work without Run.
func (n *Node) ensureFresh(ctx context.Context) {
	n.discMu.Lock()
	stale := n.discAt.IsZero() || n.now().Sub(n.discAt) >= n.discoverEvery
	n.discMu.Unlock()
	if stale {
		n.discover(ctx)
	}
}

func (n *Node) okLocked(st *peerState, d time.Duration) {
	st.failures = 0
	st.skipUntil = time.Time{}
	if d <= 0 {
		d = time.Microsecond
	}
	if st.latency == 0 {
		st.latency = d
	} else {
		st.latency = (st.latency*7 + d*3) / 10
	}
}

func (n *Node) failLocked(st *peerState, err error) {
	st.failures++
	if st.failures >= failLimit {
		st.failures = 0
		st.skipUntil = n.now().Add(n.skipFor)
		n.log.Warn("mesh: skipping a pool device after repeated failures", "device", st.hello.Device, "for", n.skipFor.String(), "err", err)
	}
}

// record notes the outcome of one call to a peer.
func (n *Node) record(id string, err error, d time.Duration) {
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	st, ok := n.peers[id]
	if !ok {
		return
	}
	if err != nil {
		n.failLocked(st, err)
		return
	}
	n.okLocked(st, d)
}

// snapshot lists known pool peers, healthy or not.
func (n *Node) snapshot() []peerRef {
	now := n.now()
	n.peersMu.Lock()
	defer n.peersMu.Unlock()
	out := make([]peerRef, 0, len(n.peers))
	for _, st := range n.peers {
		out = append(out, peerRef{
			id: st.id, base: st.base, hello: st.hello, latency: st.latency,
			skipped: now.Before(st.skipUntil),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].hello.Device != out[j].hello.Device {
			return out[i].hello.Device < out[j].hello.Device
		}
		return out[i].id < out[j].id
	})
	return out
}

// healthyBrokers lists broker peers not being skipped, fastest first.
func (n *Node) healthyBrokers() []peerRef {
	var out []peerRef
	for _, p := range n.snapshot() {
		if p.hello.Broker && !p.skipped {
			out = append(out, p)
		}
	}
	sortByLatency(out)
	return out
}

// sortByLatency orders peers by recent success latency; peers with no
// success yet go last, and ties keep device order.
func sortByLatency(ps []peerRef) {
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i].latency, ps[j].latency
		if (a == 0) != (b == 0) {
			return b == 0
		}
		return a < b
	})
}

// Brokers returns a WorkSource for each healthy remote broker in the pool,
// fastest first.
func (n *Node) Brokers(ctx context.Context) []core.WorkSource {
	n.ensureFresh(ctx)
	var out []core.WorkSource
	for _, p := range n.healthyBrokers() {
		out = append(out, &remoteBroker{n: n, ref: p})
	}
	return out
}

// remoteBroker is a broker on another device, reached over the tailnet.
type remoteBroker struct {
	n   *Node
	ref peerRef
}

func (b *remoteBroker) Name() string { return "mesh:" + b.ref.hello.Device }

func (b *remoteBroker) Work(ctx context.Context, req core.WorkRequest) (core.WorkResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, ioTimeout)
	defer cancel()
	var resp core.WorkResponse
	start := time.Now()
	err := b.n.call(ctx, http.MethodPost, b.ref.base+"/v1/work", req, &resp)
	b.n.record(b.ref.id, err, time.Since(start))
	if err != nil {
		return core.WorkResponse{}, fmt.Errorf("broker %s: %w", b.ref.hello.Device, err)
	}
	for i := range resp.Offers {
		resp.Offers[i].Grant.Secret = resp.Offers[i].Secret
		resp.Offers[i].Secret = nil
	}
	return resp, nil
}

// FetchArchive implements core.ArchiveSource: GET /v1/archive/<ref> on the
// broker device, as device. A 404 is core.ErrNotFound.
func (b *remoteBroker) FetchArchive(ctx context.Context, ref, device string, w io.Writer) error {
	if !validRef(ref) {
		return core.ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.ref.base+"/v1/archive/"+ref, nil)
	if err != nil {
		return err
	}
	req.Header.Set(poolHeader, b.n.o.Pool)
	req.Header.Set(deviceHeader, device)
	resp, err := b.n.stream.Do(req)
	if err != nil {
		b.n.record(b.ref.id, err, 0)
		return fmt.Errorf("broker %s: archive: %w", b.ref.hello.Device, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return core.ErrNotFound
	default:
		var eb errorBody
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&eb)
		msg := strings.TrimSpace(eb.Error)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("broker %s: archive: %d %s", b.ref.hello.Device, resp.StatusCode, msg)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxArchive+1))
	if err != nil {
		return fmt.Errorf("broker %s: archive stream broke: %w", b.ref.hello.Device, err)
	}
	if n > maxArchive {
		return fmt.Errorf("broker %s: the archive is larger than 2 GiB", b.ref.hello.Device)
	}
	return nil
}

// claimParty is one lease table a claim goes through: this node's or a
// broker peer's.
type claimParty struct {
	name string
	self bool
	ref  peerRef
}

// claimParties lists this node and every healthy broker peer in a fixed
// order (device name), so concurrent claimers meet at the same table first
// and one of them wins instead of each blocking the other.
func (n *Node) claimParties(ctx context.Context) []claimParty {
	n.ensureFresh(ctx)
	parties := []claimParty{{name: n.o.Device, self: true}}
	for _, p := range n.healthyBrokers() {
		parties = append(parties, claimParty{name: p.hello.Device, ref: p})
	}
	sort.SliceStable(parties, func(i, j int) bool {
		if parties[i].name != parties[j].name {
			return parties[i].name < parties[j].name
		}
		return parties[i].self && !parties[j].self
	})
	return parties
}

// Claim implements core.Claimer across the pool's brokers. It is granted
// unless this node or a reachable broker peer says another device holds
// the job. Unreachable peers don't block: the pool falls back to a bounded
// race, never a stall.
func (n *Node) Claim(ctx context.Context, job core.Job, device string, ttl time.Duration) bool {
	key := job.Key()
	ttlS := int64((ttl + time.Second - 1) / time.Second)
	if ttlS < 1 {
		ttlS = 1
	}
	var won []claimParty
	for _, p := range n.claimParties(ctx) {
		if p.self {
			ok, holder := n.leases.claim(key, device, ttl, n.now())
			if !ok {
				n.log.Debug("mesh: claim lost", "job", key, "holder", holder)
				n.undo(ctx, key, device, won)
				return false
			}
			won = append(won, p)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, n.callTimeout)
		var resp claimResponse
		start := time.Now()
		err := n.call(cctx, http.MethodPost, p.ref.base+"/v1/claim", claimRequest{JobKey: key, Device: device, TTLS: ttlS}, &resp)
		cancel()
		n.record(p.ref.id, err, time.Since(start))
		if err != nil {
			continue // unreachable: doesn't block
		}
		if !resp.Granted && resp.Holder != device {
			n.log.Debug("mesh: claim lost", "job", key, "holder", resp.Holder, "at", p.name)
			n.undo(ctx, key, device, won)
			return false
		}
		won = append(won, p)
	}
	return true
}

// undo releases a lost claim where it was granted, so a partial win
// doesn't hold the job until the lease expires.
func (n *Node) undo(ctx context.Context, key, device string, won []claimParty) {
	n.releaseOn(context.WithoutCancel(ctx), key, device, won)
}

// Release drops a claim on this node and every healthy broker peer.
func (n *Node) Release(ctx context.Context, job core.Job, device string) {
	n.releaseOn(ctx, job.Key(), device, n.claimParties(ctx))
}

func (n *Node) releaseOn(ctx context.Context, key, device string, parties []claimParty) {
	var wg sync.WaitGroup
	for _, p := range parties {
		if p.self {
			n.leases.release(key, device)
			continue
		}
		wg.Add(1)
		go func(p claimParty) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, n.callTimeout)
			defer cancel()
			var resp releaseResponse
			start := time.Now()
			err := n.call(cctx, http.MethodPost, p.ref.base+"/v1/release", releaseRequest{JobKey: key, Device: device}, &resp)
			n.record(p.ref.id, err, time.Since(start))
		}(p)
	}
	wg.Wait()
}

// Pool gathers every pool device's status (2 s each, concurrently) plus
// this device's own. Devices that don't answer carry an Error.
func (n *Node) Pool(ctx context.Context) ([]DeviceStatus, error) {
	n.ensureFresh(ctx)
	self := DeviceStatus{Device: n.o.Device, Self: true, Broker: n.o.Broker != nil, Version: n.o.Version}
	var st any = map[string]any{}
	if n.o.Status != nil {
		st = n.o.Status()
	}
	if b, err := json.Marshal(st); err != nil {
		self.Error = "can't encode status: " + err.Error()
	} else {
		self.Status = b
	}

	peers := n.snapshot()
	out := make([]DeviceStatus, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		out[i] = DeviceStatus{Device: p.hello.Device, Broker: p.hello.Broker, Version: p.hello.Version}
		if p.skipped {
			out[i].Error = "not answering; skipped for now"
			continue
		}
		wg.Add(1)
		go func(i int, p peerRef) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, n.statusTimeout)
			defer cancel()
			var raw json.RawMessage
			start := time.Now()
			err := n.call(cctx, http.MethodGet, p.base+"/v1/status", nil, &raw)
			n.record(p.id, err, time.Since(start))
			if err != nil {
				out[i].Error = err.Error()
				return
			}
			out[i].Status = raw
		}(i, p)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]DeviceStatus{self}, out...), nil
}

// call does one JSON request to a peer. Bodies are capped at 1 MiB both
// ways. Error messages carry the peer's error text, never a body.
func (n *Node) call(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		if len(b) > maxBody {
			return errors.New("request is over 1 MiB")
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set(poolHeader, n.o.Pool)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	lr := io.LimitReader(resp.Body, maxBody+1)
	if resp.StatusCode != http.StatusOK {
		var eb errorBody
		_ = json.NewDecoder(io.LimitReader(lr, 4096)).Decode(&eb)
		msg := strings.TrimSpace(eb.Error)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("%s: %d %s", pathOf(url), resp.StatusCode, msg)
	}
	b, err := io.ReadAll(lr)
	if err != nil {
		return err
	}
	if len(b) > maxBody {
		return errors.New("answer is over 1 MiB")
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: bad JSON answer", pathOf(url))
	}
	return nil
}

func pathOf(url string) string {
	if i := strings.Index(url, "/v1/"); i >= 0 {
		return url[i:]
	}
	return url
}
