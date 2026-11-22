package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
)

// nodeBroker binds every grant to the tailnet node that asked for it, as
// dispatch.Broker does: while a device's grant is on record, only that
// node may report for the device or fetch the grant's archive.
type nodeBroker struct {
	mu      sync.Mutex
	grants  map[string]string // runner -> device
	nodes   map[string]string // runner -> node
	results []core.CheckResult
	refs    map[string]string // archive ref -> runner
	n       int
}

func (b *nodeBroker) Name() string { return "node-broker" }

func (b *nodeBroker) Work(ctx context.Context, req core.WorkRequest) (core.WorkResponse, error) {
	node := core.CallerNode(ctx)
	b.mu.Lock()
	defer b.mu.Unlock()
	for r, d := range b.grants {
		if d == req.Device && b.nodes[r] != node {
			return core.WorkResponse{}, core.ErrOtherNode
		}
	}
	for _, res := range req.Results {
		if b.grants[res.Runner] == req.Device {
			b.results = append(b.results, res)
			delete(b.grants, res.Runner)
		}
	}
	var resp core.WorkResponse
	for i := 0; i < req.Free; i++ {
		b.n++
		r := fmt.Sprintf("tm-%s-%d", req.Device, b.n)
		b.grants[r], b.nodes[r] = req.Device, node
		b.refs[strings.Repeat(fmt.Sprintf("%02x", b.n), 32)] = r
		resp.Offers = append(resp.Offers, core.WorkOffer{Grant: core.Grant{RunnerName: r}})
	}
	return resp, nil
}

func (b *nodeBroker) FetchArchive(ctx context.Context, ref, device string, w io.Writer) error {
	b.mu.Lock()
	r, ok := b.refs[ref]
	ok = ok && b.grants[r] == device && b.nodes[r] == core.CallerNode(ctx)
	b.mu.Unlock()
	if !ok {
		return core.ErrNotFound
	}
	_, err := w.Write([]byte("tarball of " + r))
	return err
}

// Worker A (node n2) holds a grant, then drops out of discovery. Node B
// (n7, another device of an allowed member) announces A's name. B's report
// and archive request for A's grant are refused, and B gets no work as A;
// once A is back, its own report and archive request work (review r2
// partS sol-S2-F2).
func TestReplacementHelloDoesNotInheritGrants(t *testing.T) {
	ts := newFakeTS()
	ts.ids["100.64.0.7"] = Identity{ID: "n7", Node: "bob-laptop.example-tailnet.ts.net", HostName: "bob-laptop",
		UserID: 1002, LoginName: "bob@example.com"}
	nb := &nodeBroker{grants: map[string]string{}, nodes: map[string]string{}, refs: map[string]string{}}
	n := New(Options{Device: "alice-mac", Pool: "home", Members: []string{"bob@example.com"}, Tailscale: ts, Broker: nb})
	h := n.handler()
	work := func(remote string, req core.WorkRequest) *httptest.ResponseRecorder {
		b, _ := json.Marshal(req)
		return do(t, h, "POST", "/v1/work", remote, bytes.NewReader(b), "home")
	}
	archive := func(remote, ref string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/v1/archive/"+ref, nil)
		req.RemoteAddr = remote
		req.Header.Set(poolHeader, "home")
		req.Header.Set(deviceHeader, "a-worker")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	const fromA, fromB = "100.64.0.2:40000", "100.64.0.7:40000"
	ref := strings.Repeat("01", 32)

	knowPeer(n, "n2", "100.64.0.2", "a-worker")
	rec := work(fromA, core.WorkRequest{Device: "a-worker", Free: 1})
	var resp core.WorkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); rec.Code != 200 || err != nil || len(resp.Offers) != 1 {
		t.Fatalf("A's work: %d %s", rec.Code, rec.Body)
	}
	runner := resp.Offers[0].Grant.RunnerName

	// A drops out; B announces A's name.
	n.peersMu.Lock()
	delete(n.peers, "n2")
	n.peersMu.Unlock()
	knowPeer(n, "n7", "100.64.0.7", "a-worker")
	forged := core.WorkRequest{Device: "a-worker", Free: 1,
		Results: []core.CheckResult{{Runner: runner, State: "success", Description: "forged"}}}
	if rec := work(fromB, forged); rec.Code != http.StatusForbidden {
		t.Fatalf("B's report as A: %d %s", rec.Code, rec.Body)
	}
	if rec := archive(fromB, ref); rec.Code == 200 || strings.Contains(rec.Body.String(), "tarball") {
		t.Fatalf("B's archive request for A's grant: %d %s", rec.Code, rec.Body)
	}
	nb.mu.Lock()
	got, held := len(nb.results), nb.grants[runner] != ""
	nb.mu.Unlock()
	if got != 0 || !held {
		t.Fatalf("B's report reached the grant: results %d, grant held %v", got, held)
	}

	// B leaves and A is back: A's own calls work.
	n.peersMu.Lock()
	delete(n.peers, "n7")
	n.peersMu.Unlock()
	knowPeer(n, "n2", "100.64.0.2", "a-worker")
	if rec := archive(fromA, ref); rec.Code != 200 || rec.Body.String() != "tarball of "+runner {
		t.Fatalf("A's archive request: %d %s", rec.Code, rec.Body)
	}
	if rec := work(fromA, core.WorkRequest{Device: "a-worker",
		Results: []core.CheckResult{{Runner: runner, State: "success"}}}); rec.Code != 200 {
		t.Fatalf("A's report: %d %s", rec.Code, rec.Body)
	}
	nb.mu.Lock()
	defer nb.mu.Unlock()
	if len(nb.results) != 1 || nb.results[0].Description == "forged" {
		t.Fatalf("results = %+v", nb.results)
	}
}

// A WhoIs answer without a stable node ID can't be bound to grants, so the
// caller acts as no device.
func TestCallerWithoutNodeIDActsAsNoDevice(t *testing.T) {
	ts := newFakeTS()
	noID := alice
	noID.ID = ""
	ts.ids["100.64.0.2"] = noID
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts, Broker: &fakeBroker{}})
	knowPeer(n, "n2", "100.64.0.2", "alice-linux")
	b, _ := json.Marshal(core.WorkRequest{Device: "alice-linux"})
	if rec := do(t, n.handler(), "POST", "/v1/work", "100.64.0.2:1", bytes.NewReader(b), "home"); rec.Code != http.StatusForbidden {
		t.Fatalf("no node ID: %d %s", rec.Code, rec.Body)
	}
}
