package mesh

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRunWaitsForTailscaleThenServesOnTailnetIPOnly(t *testing.T) {
	ts := newFakeTS()
	ts.selfErr = errors.New("tailscale is not running (state Stopped)")
	ts.self = aliceSelf("127.0.0.1")
	log, buf := debugLog()
	n := New(Options{Device: "alice-mac", Pool: "home", Tailscale: ts, Log: log})
	n.retryEvery = 20 * time.Millisecond
	n.discoverEvery = 20 * time.Millisecond
	var mu sync.Mutex
	var listened []string
	seen := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), listened...) }
	n.listen = func(network, addr string) (net.Listener, error) {
		mu.Lock()
		listened = append(listened, addr)
		mu.Unlock()
		// Port 0 stands in for mesh.port; the host must be the tailnet IP.
		host, _, _ := net.SplitHostPort(addr)
		return net.Listen(network, net.JoinHostPort(host, "0"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()

	time.Sleep(150 * time.Millisecond) // several retries while down
	if got := strings.Count(buf.String(), "tailscale is not up"); got != 1 {
		t.Fatalf("logged %d times while down, want once:\n%s", got, buf)
	}
	if len(n.boundAddrs()) != 0 {
		t.Fatal("serving while tailscale is down")
	}
	select {
	case err := <-done:
		t.Fatalf("Run exited while tailscale was down: %v", err)
	default:
	}

	ts.set(func(f *fakeTS) { f.selfErr = nil })
	waitFor(t, "the peer API", func() bool { return len(n.boundAddrs()) == 1 })
	if l := seen(); len(l) == 0 || l[0] != "127.0.0.1:7433" {
		t.Fatalf("listened on %q, want the tailnet IP at mesh.port", l)
	}
	addr := n.boundAddrs()[0]
	resp, err := http.Get("http://" + addr + "/v1/hello")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// It serves, and refuses this test as it refuses any caller on this
	// device: the listener's only reachable address here is loopback.
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(buf.String(), "from this device itself") {
		t.Fatalf("hello from this device: %d\n%s", resp.StatusCode, buf)
	}

	// A new tailnet IP restarts the listener on it.
	ts.set(func(f *fakeTS) {
		f.self.IPs = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1")}
	})
	waitFor(t, "a restart on the new IPs", func() bool { return len(n.boundAddrs()) >= 1 && len(seen()) >= 2 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if !strings.Contains(buf.String(), "tailscale is up") {
		t.Errorf("recovery not logged:\n%s", buf)
	}
}
