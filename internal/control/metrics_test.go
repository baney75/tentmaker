package control

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// serveMetrics serves the loopback metrics listener with ident naming
// every caller; it returns the base URL and the views' call count.
func serveMetrics(t *testing.T, ident Identify) (string, *atomic.Int32) {
	t.Helper()
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var served atomic.Int32
	views := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		_, _ = w.Write([]byte("view " + r.URL.Path))
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewMetricsServer(views, ident, nil).Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return "http://" + ln.Addr().String(), &served
}

// The metrics listener lets a Prometheus on this device scrape /metrics
// over TCP (review second-F4) and serves nothing else: not the status
// page, not a control route.
func TestMetricsListenerServesMetricsAlone(t *testing.T) {
	base, served := serveMetrics(t, func(net.Conn) (Caller, error) { return reader, nil })
	if code, body := get(t, base+"/metrics"); code != 200 || body != "view /metrics" {
		t.Fatalf("GET /metrics: %d %q", code, body)
	}
	for _, p := range []string{"/", "/status", "/v1/status", "/v1/pool", "/v1/runs", "/v1/relay/x"} {
		if code, _ := get(t, base+p); code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, code)
		}
	}
	resp, err := http.Post(base+"/metrics", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("POST /metrics: %d", resp.StatusCode)
	}
	if n := served.Load(); n != 1 {
		t.Fatalf("served %d views, want 1", n)
	}
}

// It refuses whom the control API refuses: a CI job, a caller it can't
// name, every caller when it has no way to name one (macOS), and a page
// that rebinds its own name to loopback.
func TestMetricsListenerRefusesJobsAndStrangers(t *testing.T) {
	for name, ident := range map[string]Identify{
		"a CI job":        func(net.Conn) (Caller, error) { return Caller{Runner: true, Who: "uid 900"}, nil },
		"an unnamed peer": func(net.Conn) (Caller, error) { return Caller{}, io.EOF },
		"no identify":     nil,
		"this OS":         unsupportedTCPIdentify,
	} {
		base, served := serveMetrics(t, ident)
		if code, _ := get(t, base+"/metrics"); code != http.StatusForbidden {
			t.Errorf("%s: GET /metrics: %d, want 403", name, code)
		}
		if n := served.Load(); n != 0 {
			t.Errorf("%s got %d views", name, n)
		}
	}
	base, served := serveMetrics(t, func(net.Conn) (Caller, error) { return reader, nil })
	req, _ := http.NewRequest("GET", base+"/metrics", nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || served.Load() != 0 {
		t.Fatalf("foreign Host: %d, served %d", resp.StatusCode, served.Load())
	}
}

// unsupportedTCPIdentify is what TCPIdentify is where the OS can't name a
// TCP caller.
var unsupportedTCPIdentify Identify = func(net.Conn) (Caller, error) {
	return Caller{}, errors.New("this OS can't name a TCP caller")
}

// fakeTCPConn is a net.Conn with fixed TCP ends.
type fakeTCPConn struct {
	net.Conn
	local, remote netip.AddrPort
}

func (c fakeTCPConn) LocalAddr() net.Addr  { return net.TCPAddrFromAddrPort(c.local) }
func (c fakeTCPConn) RemoteAddr() net.Addr { return net.TCPAddrFromAddrPort(c.remote) }

// On Linux the listener names a caller by the uid that owns the client
// end of the connection in /proc/net/tcp or tcp6; the runner's uid and its
// subordinate uids are CI jobs, and a connection it can't find is refused.
func TestProcTCPIdentify(t *testing.T) {
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	head := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	// 127.0.0.1 is 0100007F in /proc's byte order; 9437 is 0x24DD.
	tcp := head +
		"   0: 0100007F:24DD 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 1 0 100 0 0 10 0\n" +
		"   1: 0100007F:C350 0100007F:24DD 01 00000000:00000000 00:00000000 00000000  1000        0 1002 1 0 20 4 30 10 -1\n" +
		"   2: 0100007F:24DD 0100007F:C350 01 00000000:00000000 00:00000000 00000000     0        0 1003 1 0 20 4 30 10 -1\n" +
		"   3: 0100007F:C351 0100007F:24DD 01 00000000:00000000 00:00000000 00000000   900        0 1004 1 0 20 4 30 10 -1\n" +
		"   4: 0100007F:C352 0100007F:24DD 01 00000000:00000000 00:00000000 00000000 100005        0 1005 1 0 20 4 30 10 -1\n"
	// A dual-stack client: the v4-mapped 127.0.0.1 in tcp6.
	tcp6 := head +
		"   0: 0000000000000000FFFF00000100007F:C353 0000000000000000FFFF00000100007F:24DD 01 00000000:00000000 00:00000000 00000000  1000        0 2001 1 0 20 4 30 10 -1\n"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(proc, "net", "tcp"), []byte(tcp), 0o644))
	must(os.WriteFile(filepath.Join(proc, "net", "tcp6"), []byte(tcp6), 0o644))
	ident := procTCPIdentify(proc, func() ([]IDRange, error) {
		return []IDRange{{First: 900, Count: 1}, {First: 100000, Count: 65536}}, nil
	})
	server := netip.MustParseAddrPort("127.0.0.1:9437")
	conn := func(client string) net.Conn {
		return fakeTCPConn{local: server, remote: netip.MustParseAddrPort(client)}
	}
	for client, want := range map[string]Caller{
		"127.0.0.1:50000": {Who: "uid 1000", ID: "1000"},
		"127.0.0.1:50001": {Runner: true, Who: "uid 900", ID: "900"},
		"127.0.0.1:50002": {Runner: true, Who: "uid 100005", ID: "100005"},
		"127.0.0.1:50003": {Who: "uid 1000", ID: "1000"},
	} {
		c, err := ident(conn(client))
		if err != nil || c != want {
			t.Errorf("caller from %s = %+v %v, want %+v", client, c, err, want)
		}
	}
	if _, err := ident(conn("127.0.0.1:50009")); err == nil {
		t.Error("a connection with no row was named")
	}
	if _, err := ident(conn("100.64.0.2:50000")); err == nil {
		t.Error("a caller from off the machine was named")
	}
	broken := procTCPIdentify(proc, func() ([]IDRange, error) { return nil, errors.New("no passwd") })
	if _, err := broken(conn("127.0.0.1:50000")); err == nil {
		t.Error("a caller was served while the runner's ids can't be read")
	}
	if c, err := broken(conn("127.0.0.1:50000")); err == nil || c.Admin {
		t.Errorf("unreadable runner ids: %+v %v", c, err)
	}
}
