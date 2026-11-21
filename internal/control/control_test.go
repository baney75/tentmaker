package control

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeHandler struct {
	mu      sync.Mutex
	paused  string
	pauseD  time.Duration
	reloads int
}

func (f *fakeHandler) Status(ctx context.Context) (any, error) {
	return map[string]string{"device": "box"}, nil
}
func (f *fakeHandler) Pool(ctx context.Context) (any, error) { return []string{"box"}, nil }
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

func start(t *testing.T) (*fakeHandler, *Client, string) {
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
	go func() { done <- NewServer(h, tok, nil).Serve(ctx, ln) }()
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

func TestGetsNeedNoToken(t *testing.T) {
	_, c, _ := start(t)
	c.TokenPath = filepath.Join(t.TempDir(), "none")
	var st map[string]string
	if err := c.Get(context.Background(), "/v1/status", &st); err != nil || st["device"] != "box" {
		t.Fatalf("status %v %v", st, err)
	}
	var pool []string
	if err := c.Get(context.Background(), "/v1/pool", &pool); err != nil || len(pool) != 1 {
		t.Fatalf("pool %v %v", pool, err)
	}
	err := c.Get(context.Background(), "/v1/doctor", nil)
	var e *Error
	if !errors.As(err, &e) || e.Status != 500 || e.Message != "doctor broke" {
		t.Fatalf("doctor error %v", err)
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
