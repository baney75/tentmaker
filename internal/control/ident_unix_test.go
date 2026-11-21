//go:build linux || darwin

package control

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// unixServe serves the control API on a Unix socket in a short temp dir
// (macOS caps socket paths near 104 bytes), naming callers by their real
// peer uid with runner as the runner's ids.
func unixServe(t *testing.T, runner []IDRange) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "tmctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	addr := "unix:" + filepath.Join(dir, "run", "control.sock")
	ln, err := Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	ident := unixIdentify(func() ([]IDRange, error) { return runner, nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- NewServer(&fakeHandler{}, "", ident, nil).Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return &Client{Addr: addr}
}

// Over the real socket the server reads the caller's uid from the kernel:
// this test's own uid reads status, and gets 403 once it is the runner's.
func TestUnixSocketNamesCallerByUID(t *testing.T) {
	uid := uint32(os.Getuid())
	if uid == 0 {
		t.Skip("root is always served")
	}
	var st map[string]string
	if err := unixServe(t, []IDRange{{uid + 1, 1}}).Get(context.Background(), "/v1/status", &st); err != nil || st["device"] != "box" {
		t.Fatalf("status as an ordinary user: %v %v", st, err)
	}
	var e *Error
	for _, r := range []IDRange{{uid, 1}, {uid - 1, 3}} {
		err := unixServe(t, []IDRange{r}).Get(context.Background(), "/v1/status", nil)
		if !errors.As(err, &e) || e.Status != http.StatusForbidden {
			t.Fatalf("status as the runner (ids %v): %v, want 403", r, err)
		}
	}
}

// The socket is open to every local user, a stale one is replaced, and one
// a live server holds is not.
func TestUnixSocketListen(t *testing.T) {
	dir, err := os.MkdirTemp("", "tmctl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "control.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	ln, err := Listen("unix:" + path)
	if err != nil {
		t.Fatalf("a stale socket blocked the listener: %v", err)
	}
	defer ln.Close()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o666 {
		t.Fatalf("socket mode %v %v, want 0666", fi.Mode().Perm(), err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if _, err := Listen("unix:" + path); err == nil {
		t.Fatal("a second listener took a live socket")
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen("unix:" + filepath.Join(dir, "file")); err == nil {
		t.Fatal("a regular file was replaced by the socket")
	}
	if _, err := Listen("unix:relative.sock"); err == nil {
		t.Fatal("a relative socket path was accepted")
	}
}
