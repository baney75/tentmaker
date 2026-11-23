package control

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/sandbox"
)

// ownerWorker is a sandbox.Worker with sandboxes in a map; it counts the
// commands that reach each one.
type ownerWorker struct {
	mu    sync.Mutex
	sbx   map[string]dispatch.SandboxInfo
	execs map[string]int
}

func (w *ownerWorker) StartSandbox(ctx context.Context, s dispatch.SandboxSpec) (dispatch.SandboxInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	i := dispatch.SandboxInfo{Name: s.Name, Platform: core.Platform{OS: s.Platform.OS, Arch: "x64"}, Owner: s.Owner,
		State: "running", Expires: time.Now().Add(s.TTL)}
	w.sbx[s.Name] = i
	return i, nil
}

func (w *ownerWorker) ExecSandbox(ctx context.Context, name string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	w.mu.Lock()
	w.execs[name]++
	w.mu.Unlock()
	if stdin != nil {
		io.Copy(io.Discard, stdin)
	}
	if req.Argv[0] == "tar" {
		tw := tar.NewWriter(stdout)
		tw.Close()
	}
	return 0, nil
}

func (w *ownerWorker) StopSandbox(ctx context.Context, name, why string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.sbx, name)
	return nil
}

func (w *ownerWorker) Sandbox(name string) (dispatch.SandboxInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	i, ok := w.sbx[name]
	if !ok {
		return i, dispatch.ErrNoSandbox
	}
	return i, nil
}

func (w *ownerWorker) Sandboxes() []dispatch.SandboxInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []dispatch.SandboxInfo
	for _, i := range w.sbx {
		out = append(out, i)
	}
	return out
}

type managerHandler struct {
	fakeHandler
	m *sandbox.Manager
}

func (h *managerHandler) SandboxService() SandboxService { return h.m }

// serveAs serves h's sandboxes to every connection as caller, and returns
// a client that sends no token.
func serveAs(t *testing.T, h Handler, tok string, caller Caller) *Client {
	t.Helper()
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewServer(h, tok, func(net.Conn) (Caller, error) { return caller, nil }, nil).Serve(ctx, ln)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return &Client{Addr: ln.Addr().String(), TokenPath: filepath.Join(t.TempDir(), "none")}
}

// Through the control API, with the caller named by the connection: an
// account may list, exec in, copy to or from and remove only the
// sandboxes it owns, and the control token's holder may act on any.
func TestSandboxOwnerChecksOverTheAPI(t *testing.T) {
	dir := t.TempDir()
	tok, err := NewToken(TokenPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	w := &ownerWorker{sbx: map[string]dispatch.SandboxInfo{}, execs: map[string]int{}}
	h := &managerHandler{m: sandbox.New(sandbox.Options{Worker: func() sandbox.Worker { return w },
		Device: func() string { return "box" }, Users: func() []string { return []string{"1000", "1001"} }})}
	alice := serveAs(t, h, tok, Caller{ID: "1000", Who: "uid 1000"})
	bob := serveAs(t, h, tok, Caller{ID: "1001", Who: "uid 1001"})
	// The token holder connects as an ordinary account, and is an admin.
	admin := serveAs(t, h, tok, Caller{ID: "1001", Who: "uid 1001"})
	admin.TokenPath = TokenPath(dir)

	var info sandbox.Info
	if err := alice.SandboxCall(ctxT(t), http.MethodPost, "/v1/sandboxes", sandbox.CreateRequest{}, &info, time.Minute); err != nil {
		t.Fatal(err)
	}
	id := info.ID
	if info.Owner != "1000" {
		t.Fatalf("owner %q", info.Owner)
	}

	list := func(c *Client) []sandbox.Info {
		var l struct{ Sandboxes []sandbox.Info }
		if err := c.SandboxCall(ctxT(t), http.MethodGet, "/v1/sandboxes", nil, &l, time.Minute); err != nil {
			t.Fatal(err)
		}
		return l.Sandboxes
	}
	// stream runs one streamed route and returns its refusal, or the
	// exit frame's error.
	stream := func(c *Client, method, path string, body io.Reader) error {
		resp, err := c.Stream(ctxT(t), method, path, body)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.Header.Get("Content-Type") != sandbox.ContentType {
			io.Copy(io.Discard, resp.Body)
			return nil
		}
		e, err := sandbox.ReadFrames(resp.Body, io.Discard, io.Discard)
		if err == nil && e.Error != "" {
			err = errors.New(e.Error)
		}
		return err
	}
	execLine, _ := json.Marshal(sandbox.ExecRequest{Argv: []string{"true"}})
	var emptyTar bytes.Buffer
	tar.NewWriter(&emptyTar).Close()
	ops := []struct {
		name string
		do   func(c *Client) error
	}{
		{"get", func(c *Client) error {
			return c.SandboxCall(ctxT(t), http.MethodGet, "/v1/sandboxes/"+id, nil, nil, time.Minute)
		}},
		{"exec", func(c *Client) error {
			return stream(c, http.MethodPost, "/v1/sandboxes/"+id+"/exec", bytes.NewReader(append(execLine, '\n')))
		}},
		{"copy in", func(c *Client) error {
			return stream(c, http.MethodPost, "/v1/sandboxes/"+id+"/files?path=in", bytes.NewReader(emptyTar.Bytes()))
		}},
		{"copy out", func(c *Client) error {
			return stream(c, http.MethodGet, "/v1/sandboxes/"+id+"/files?path="+url.QueryEscape("out"), nil)
		}},
		{"remove", func(c *Client) error {
			return c.SandboxCall(ctxT(t), http.MethodDelete, "/v1/sandboxes/"+id, nil, nil, time.Minute)
		}},
	}

	if len(list(bob)) != 0 {
		t.Error("another account lists the sandbox")
	}
	for _, op := range ops {
		var e *Error
		if err := op.do(bob); !errors.As(err, &e) || e.Status != http.StatusNotFound {
			t.Errorf("%s by another account: %v, want 404", op.name, err)
		}
	}
	if w.execs[id] != 0 || len(w.sbx) != 1 {
		t.Fatalf("another account reached the sandbox: %d commands, %d sandboxes", w.execs[id], len(w.sbx))
	}
	for _, c := range []*Client{alice, admin} {
		if l := list(c); len(l) != 1 || l[0].ID != id {
			t.Errorf("list: %+v", l)
		}
		for _, op := range ops[:len(ops)-1] {
			if err := op.do(c); err != nil {
				t.Errorf("%s by %s: %v", op.name, c.TokenPath, err)
			}
		}
	}
	if w.execs[id] != 6 {
		t.Fatalf("%d commands reached the sandbox, want 3 each for the owner and the admin", w.execs[id])
	}
	if err := ops[len(ops)-1].do(admin); err != nil || len(w.sbx) != 0 {
		t.Fatalf("the admin's remove: %v, %d left", err, len(w.sbx))
	}
}
