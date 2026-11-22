package control

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

type fakeHandler struct {
	mu      sync.Mutex
	paused  string
	pauseD  time.Duration
	reloads int
	reruns  []RerunRequest
}

func (f *fakeHandler) Status(ctx context.Context) (any, error) {
	return map[string]string{"device": "box"}, nil
}
func (f *fakeHandler) Pool(ctx context.Context) (any, error) { return []string{"box"}, nil }

// Relay serves one known relay id.
func (f *fakeHandler) Relay(ctx context.Context, id string, w io.Writer) error {
	switch id {
	case relayOK:
		_, err := w.Write([]byte("tarball"))
		return err
	case relayBroken:
		return errors.New("the broker went away")
	}
	return core.ErrNotFound
}

var (
	relayOK     = strings.Repeat("ab", 32)
	relayBroken = strings.Repeat("cd", 32)
)

func (f *fakeHandler) Doctor(ctx context.Context) (any, error) {
	return nil, errors.New("doctor broke")
}
func (f *fakeHandler) Pause(ctx context.Context, reason string, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused, f.pauseD = reason, d
	return nil
}
func (f *fakeHandler) Resume(ctx context.Context) error { return nil }
func (f *fakeHandler) Reload(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	return nil
}

// reader is an ordinary local user: not a CI job, not an admin.
var reader = Caller{Who: "uid 1000"}

func start(t *testing.T) (*fakeHandler, *Client, string) {
	t.Helper()
	return startAs(t, func(net.Conn) (Caller, error) { return reader, nil })
}

// startAs serves the control API with ident naming every caller.
func startAs(t *testing.T, ident Identify) (*fakeHandler, *Client, string) {
	t.Helper()
	dir := t.TempDir()
	tokPath := TokenPath(dir)
	tok, err := NewToken(tokPath)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewServer(h, tok, ident, nil).Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return h, &Client{Addr: ln.Addr().String(), TokenPath: tokPath}, tok
}

func TestTokenFile(t *testing.T) {
	p := TokenPath(t.TempDir())
	tok, err := NewToken(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length %d", len(tok))
	}
	got, err := ReadToken(p)
	if err != nil || got != tok {
		t.Fatalf("read %q %v", got, err)
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(p)
		if st.Mode().Perm() != 0o600 {
			t.Errorf("mode %v, want 0600", st.Mode().Perm())
		}
	}
	tok2, _ := NewToken(p)
	if tok2 == tok {
		t.Error("a new token repeats the old one")
	}
}

// An ordinary user reads status and pool with no token; doctor, which
// makes the daemon call the forges, needs the token or an admin.
func TestReadersGetStatusAndPool(t *testing.T) {
	_, c, _ := start(t)
	tokPath := c.TokenPath
	c.TokenPath = filepath.Join(t.TempDir(), "none")
	var st map[string]string
	if err := c.Get(context.Background(), "/v1/status", &st); err != nil || st["device"] != "box" {
		t.Fatalf("status %v %v", st, err)
	}
	var pool []string
	if err := c.Get(context.Background(), "/v1/pool", &pool); err != nil || len(pool) != 1 {
		t.Fatalf("pool %v %v", pool, err)
	}
	var e *Error
	if err := c.Get(context.Background(), "/v1/doctor", nil); !errors.As(err, &e) || e.Status != http.StatusUnauthorized {
		t.Fatalf("doctor without the token: %v", err)
	}
	c.TokenPath = tokPath
	if err := c.Get(context.Background(), "/v1/doctor", nil); !errors.As(err, &e) || e.Status != 500 || e.Message != "doctor broke" {
		t.Fatalf("doctor with the token: %v", err)
	}
	_, admin, _ := startAs(t, func(net.Conn) (Caller, error) { return Caller{Admin: true, Who: "uid 0"}, nil })
	admin.TokenPath = filepath.Join(t.TempDir(), "none")
	if err := admin.Get(context.Background(), "/v1/doctor", nil); !errors.As(err, &e) || e.Status != 500 {
		t.Fatalf("doctor as an admin: %v", err)
	}
}

// A CI job gets 403 on every route, token or not, and the daemon is never
// called (review r1 fence verification, item 18).
func TestRunnerGetsNothing(t *testing.T) {
	h, c, _ := startAs(t, func(net.Conn) (Caller, error) { return Caller{Runner: true, Who: "uid 900"}, nil })
	ctx := context.Background()
	var e *Error
	for _, path := range []string{"/v1/status", "/v1/pool", "/v1/doctor", "/v1/runs", "/v1/runs/log?id=tm-a-1"} {
		if err := c.Get(ctx, path, nil); !errors.As(err, &e) || e.Status != http.StatusForbidden {
			t.Errorf("GET %s as a CI job: %v, want 403", path, err)
		}
	}
	for _, path := range []string{"/v1/pause", "/v1/resume", "/v1/reload", "/v1/rerun", "/v1/alerts/test"} {
		if err := c.Post(ctx, path, RerunRequest{Repo: "o/r", RunID: 1}); !errors.As(err, &e) || e.Status != http.StatusForbidden {
			t.Errorf("POST %s as a CI job with the token: %v, want 403", path, err)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.paused != "" || h.reloads != 0 || len(h.reruns) != 0 {
		t.Fatalf("the daemon acted for a CI job: paused %q, reloads %d", h.paused, h.reloads)
	}
}

// A caller the server can't name is refused, and so is every caller of a
// server with no way to name them.
func TestUnnamedCallerRefused(t *testing.T) {
	var e *Error
	_, c, _ := startAs(t, func(net.Conn) (Caller, error) { return Caller{}, errors.New("no such socket") })
	if err := c.Get(context.Background(), "/v1/status", nil); !errors.As(err, &e) || e.Status != http.StatusForbidden {
		t.Fatalf("status for an unnamed caller: %v", err)
	}
	_, c, _ = startAs(t, nil)
	if err := c.Get(context.Background(), "/v1/status", nil); !errors.As(err, &e) || e.Status != http.StatusForbidden {
		t.Fatalf("status with no identify: %v", err)
	}
}

func TestPostsNeedToken(t *testing.T) {
	h, c, tok := start(t)
	ctx := context.Background()
	if err := c.Post(ctx, "/v1/pause", PauseRequest{Reason: "call", ForS: 90}); err != nil {
		t.Fatal(err)
	}
	if h.paused != "call" || h.pauseD != 90*time.Second {
		t.Errorf("pause got %q %v", h.paused, h.pauseD)
	}
	if err := c.Post(ctx, "/v1/reload", nil); err != nil || h.reloads != 1 {
		t.Fatalf("reload %v %d", err, h.reloads)
	}
	for _, auth := range []string{"", "Bearer nope", "Bearer " + strings.Repeat("0", 64), tok} {
		req, _ := http.NewRequest("POST", "http://"+c.Addr+"/v1/reload", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("auth %q: status %d, want 401", auth, resp.StatusCode)
		}
	}
	if h.reloads != 1 {
		t.Errorf("unauthenticated reloads ran: %d", h.reloads)
	}
	// The client with a wrong token file gets 401 as an Error.
	bad := filepath.Join(t.TempDir(), TokenFile)
	os.WriteFile(bad, []byte(strings.Repeat("a", 64)), 0o600)
	c2 := &Client{Addr: c.Addr, TokenPath: bad}
	var e *Error
	if err := c2.Post(ctx, "/v1/resume", nil); !errors.As(err, &e) || e.Status != 401 {
		t.Errorf("wrong token: %v", err)
	}
}

func TestRejectsForeignHost(t *testing.T) {
	_, c, _ := start(t)
	req, _ := http.NewRequest("GET", "http://"+c.Addr+"/v1/status", nil)
	req.Host = "evil.example:7434"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
}

func TestListenLoopbackOnly(t *testing.T) {
	for _, a := range []string{"0.0.0.0:0", ":0", "[::]:0", "192.168.1.5:0", "localhost:0"} {
		if ln, err := Listen(a); err == nil {
			ln.Close()
			t.Errorf("%s: listened", a)
		}
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ip := ln.Addr().(*net.TCPAddr).IP
	if !ip.IsLoopback() {
		t.Errorf("bound to %v", ip)
	}
}

func TestClientNotRunning(t *testing.T) {
	ln, _ := Listen("127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	c := &Client{Addr: addr, TokenPath: filepath.Join(t.TempDir(), "x")}
	if err := c.Get(context.Background(), "/v1/status", nil); !errors.Is(err, ErrNotRunning) {
		t.Errorf("got %v, want ErrNotRunning", err)
	}
	if err := c.Post(context.Background(), "/v1/reload", nil); !errors.Is(err, ErrNotRunning) {
		t.Errorf("post got %v, want ErrNotRunning", err)
	}
}

// startRelay serves the relay listener for h.
func startRelay(t *testing.T, h *fakeHandler) string {
	t.Helper()
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewRelayServer(h, nil).Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return ln.Addr().String()
}

// The fence lets jobs reach the relay port, so the relay listener serves
// /v1/relay/ and nothing else, and the control listener (which jobs can't
// reach) serves no relay. Review r1 partB first-F1: both used to share
// 7434, so a job could read status and pool and loop /v1/doctor.
func TestRelayListenerServesOnlyRelay(t *testing.T) {
	h, c, tok := start(t)
	relay := startRelay(t, h)
	code := func(method, addr, path, token string) int {
		req, _ := http.NewRequest(method, "http://"+addr+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, p := range []string{"/v1/status", "/v1/pool", "/v1/doctor", "/", "/v1/relay/"} {
		if got := code("GET", relay, p, ""); got != http.StatusNotFound {
			t.Errorf("relay listener GET %s: %d, want 404", p, got)
		}
	}
	for _, p := range []string{"/v1/pause", "/v1/resume", "/v1/reload"} {
		if got := code("POST", relay, p, tok); got != http.StatusNotFound && got != http.StatusMethodNotAllowed {
			t.Errorf("relay listener POST %s: %d", p, got)
		}
	}
	if got := code("GET", c.Addr, "/v1/relay/"+relayOK, ""); got != http.StatusNotFound {
		t.Errorf("control listener serves the relay: %d", got)
	}
	if got := code("GET", c.Addr, "/v1/status", ""); got != http.StatusOK {
		t.Errorf("control listener status: %d", got)
	}
	if got := code("POST", c.Addr, "/v1/reload", tok); got != http.StatusOK {
		t.Errorf("control listener reload: %d", got)
	}
}

// The relay serves a running job's source on loopback with no token; any
// id it doesn't know is 404.
func TestRelay(t *testing.T) {
	h, _, _ := start(t)
	addr := startRelay(t, h)
	get := func(id string) (int, string) {
		resp, err := http.Get("http://" + addr + "/v1/relay/" + id)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get(relayOK); code != 200 || body != "tarball" {
		t.Fatalf("relay: %d %q", code, body)
	}
	for _, id := range []string{strings.Repeat("00", 32), "nope", "..%2f..%2fv1%2fstatus"} {
		if code, _ := get(id); code != http.StatusNotFound {
			t.Errorf("unknown id %q: %d", id, code)
		}
	}
	if code, _ := get(relayBroken); code != http.StatusBadGateway {
		t.Errorf("broken relay: %d", code)
	}
	req, _ := http.NewRequest("GET", "http://"+addr+"/v1/relay/"+relayOK, nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign host: %d", resp.StatusCode)
	}
}
