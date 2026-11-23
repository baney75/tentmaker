package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/sandbox"
)

// fakeSandboxes records the caller of every call. Its one sandbox runs an
// echo that answers each stdin line as soon as it arrives.
type fakeSandboxes struct {
	fakeHandler
	mu      sync.Mutex
	callers []sandbox.Caller
}

func (f *fakeSandboxes) SandboxService() SandboxService { return f }

func (f *fakeSandboxes) saw(c sandbox.Caller) {
	f.mu.Lock()
	f.callers = append(f.callers, c)
	f.mu.Unlock()
}

func (f *fakeSandboxes) last() sandbox.Caller {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callers[len(f.callers)-1]
}

const sbxID = "sbx-0123456789ab"

func (f *fakeSandboxes) Create(ctx context.Context, c sandbox.Caller, req sandbox.CreateRequest) (sandbox.Info, error) {
	f.saw(c)
	if req.OS == "windows" {
		return sandbox.Info{}, &sandbox.Error{Status: http.StatusConflict, Msg: "no room", Fix: "remove one"}
	}
	return sandbox.Info{ID: sbxID, OS: req.OS}, nil
}
func (f *fakeSandboxes) List(ctx context.Context, c sandbox.Caller) ([]sandbox.Info, error) {
	f.saw(c)
	return []sandbox.Info{{ID: sbxID}}, nil
}
func (f *fakeSandboxes) Get(ctx context.Context, c sandbox.Caller, id string) (sandbox.Info, error) {
	f.saw(c)
	if id != sbxID {
		return sandbox.Info{}, &sandbox.Error{Status: http.StatusNotFound, Msg: "no sandbox " + id, Fix: "tentmaker sandbox ls"}
	}
	return sandbox.Info{ID: sbxID, Expires: time.Now().Add(time.Minute)}, nil
}
func (f *fakeSandboxes) Remove(ctx context.Context, c sandbox.Caller, id string) error {
	f.saw(c)
	return nil
}
func (f *fakeSandboxes) Exec(ctx context.Context, c sandbox.Caller, id string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.saw(c)
	switch req.Argv[0] {
	case "refuse":
		return -1, &sandbox.Error{Status: http.StatusConflict, Msg: "sandbox is stopping", Fix: "create a new one"}
	case "break":
		io.WriteString(stdout, "some output")
		return -1, errors.New("the unit vanished")
	}
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		io.WriteString(stdout, "echo: "+sc.Text()+"\n")
	}
	io.WriteString(stderr, "done")
	return 9, nil
}
func (f *fakeSandboxes) CopyIn(ctx context.Context, c sandbox.Caller, id, dst string, in io.Reader) (sandbox.CopyResult, error) {
	n, _ := io.Copy(io.Discard, in)
	return sandbox.CopyResult{ID: id, Path: dst, Bytes: n}, nil
}
func (f *fakeSandboxes) CopyOut(ctx context.Context, c sandbox.Caller, id, src string, out, stderr io.Writer) (int, error) {
	io.WriteString(out, "tar of "+src)
	return 0, nil
}

func startSandboxes(t *testing.T, caller Caller) (*fakeSandboxes, *Client) {
	t.Helper()
	dir := t.TempDir()
	tok, err := NewToken(TokenPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeSandboxes{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- NewServer(h, tok, func(net.Conn) (Caller, error) { return caller, nil }, nil).Serve(ctx, ln)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return h, &Client{Addr: ln.Addr().String(), TokenPath: TokenPath(dir)}
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestSandboxRoutesNameTheCaller(t *testing.T) {
	h, c := startSandboxes(t, Caller{ID: "1000", Who: "uid 1000"})
	tok := c.TokenPath
	c.TokenPath = filepath.Join(t.TempDir(), "none")
	var info sandbox.Info
	if err := c.SandboxCall(ctxT(t), http.MethodPost, "/v1/sandboxes", sandbox.CreateRequest{OS: "linux"}, &info, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := h.last(); got.ID != "1000" || got.Admin {
		t.Fatalf("caller %+v, want uid 1000 and no admin", got)
	}
	// The control token makes the caller an admin.
	c.TokenPath = tok
	if err := c.SandboxCall(ctxT(t), http.MethodGet, "/v1/sandboxes", nil, nil, time.Minute); err != nil {
		t.Fatal(err)
	}
	if !h.last().Admin {
		t.Fatal("the token holder was not an admin")
	}
	// A refusal keeps its status and fix.
	err := c.SandboxCall(ctxT(t), http.MethodPost, "/v1/sandboxes", sandbox.CreateRequest{OS: "windows"}, nil, time.Minute)
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusConflict || e.Fix != "remove one" {
		t.Fatalf("refusal %#v", err)
	}
}

func TestSandboxRoutesNeedTheClientHeader(t *testing.T) {
	h, c := startSandboxes(t, Caller{Admin: true, ID: "0"})
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/v1/sandboxes"},
		{http.MethodGet, "/v1/sandboxes"},
		{http.MethodPost, "/v1/sandboxes/" + sbxID + "/exec"},
		{http.MethodDelete, "/v1/sandboxes/" + sbxID},
	} {
		// Like a web page's request: no Tentmaker-Client header.
		req, _ := http.NewRequest(r.method, "http://"+c.Addr+r.path, strings.NewReader(`{"argv":["x"]}`+"\n"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s without the header: %d", r.method, r.path, resp.StatusCode)
		}
	}
	if len(h.callers) != 0 {
		t.Fatal("a request without the header reached the sandboxes")
	}
}

func TestSandboxRoutesRefuseCIJobs(t *testing.T) {
	h, c := startSandboxes(t, Caller{Runner: true, ID: "900"})
	if err := c.SandboxCall(ctxT(t), http.MethodGet, "/v1/sandboxes", nil, nil, time.Minute); err == nil {
		t.Fatal("a CI job listed sandboxes")
	}
	if len(h.callers) != 0 {
		t.Fatal("a CI job reached the sandboxes")
	}
}

// exec streams both ways: the answer to each stdin line arrives before
// the next line is sent.
func TestSandboxExecIsFullDuplex(t *testing.T) {
	_, c := startSandboxes(t, Caller{Admin: true, ID: "0"})
	pr, pw := io.Pipe()
	line, _ := json.Marshal(sandbox.ExecRequest{Argv: []string{"echo"}})
	next := make(chan string, 4)
	go func() {
		pw.Write(append(line, '\n'))
		for s := range next {
			io.WriteString(pw, s+"\n")
		}
		pw.Close()
	}()
	// The first line goes before the answer's headers can arrive: they
	// come with the first output.
	next <- "one"
	resp, err := c.Stream(ctxT(t), http.MethodPost, "/v1/sandboxes/"+sbxID+"/exec", pr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	outR, outW := io.Pipe()
	type res struct {
		e   sandbox.Exit
		err error
	}
	done := make(chan res, 1)
	var errs bytes.Buffer
	go func() {
		e, err := sandbox.ReadFrames(resp.Body, outW, &errs)
		outW.Close()
		done <- res{e, err}
	}()
	br := bufio.NewReader(outR)
	for i, s := range []string{"one", "two", "three"} {
		if i > 0 {
			next <- s
		}
		got, err := br.ReadString('\n')
		if err != nil || got != "echo: "+s+"\n" {
			t.Fatalf("answer to %s: %q %v", s, got, err)
		}
	}
	close(next)
	io.Copy(io.Discard, br)
	r := <-done
	if r.err != nil || r.e.ExitCode != 9 || errs.String() != "done" {
		t.Fatalf("exit %+v err %v stderr %q", r.e, r.err, errs.String())
	}
}

func TestSandboxExecErrors(t *testing.T) {
	_, c := startSandboxes(t, Caller{Admin: true, ID: "0"})
	exec := func(id string, argv ...string) (*http.Response, error) {
		line, _ := json.Marshal(sandbox.ExecRequest{Argv: argv})
		return c.Stream(ctxT(t), http.MethodPost, "/v1/sandboxes/"+id+"/exec", bytes.NewReader(append(line, '\n')))
	}
	var e *Error
	// Refused before any output: a status and a fix.
	if _, err := exec(sbxID, "refuse"); !errors.As(err, &e) || e.Status != http.StatusConflict || e.Fix != "create a new one" {
		t.Fatalf("refuse: %#v", err)
	}
	if _, err := exec("sbx-ffffffffffff", "x"); !errors.As(err, &e) || e.Status != http.StatusNotFound {
		t.Fatalf("missing: %#v", err)
	}
	// Broken after output: the exit frame says so.
	resp, err := exec(sbxID, "break")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	x, err := sandbox.ReadFrames(resp.Body, &out, io.Discard)
	if err != nil || x.ExitCode != -1 || !strings.Contains(x.Error, "vanished") || out.String() != "some output" {
		t.Fatalf("break: %+v %v %q", x, err, out.String())
	}
	// A body that isn't an exec request.
	resp2, err := c.Stream(ctxT(t), http.MethodPost, "/v1/sandboxes/"+sbxID+"/exec", strings.NewReader("not json\n"))
	if !errors.As(err, &e) || e.Status != http.StatusBadRequest {
		t.Fatalf("bad body: %v %v", resp2, err)
	}
	// Stream refuses a call without a deadline.
	if _, err := c.Stream(context.Background(), http.MethodGet, "/v1/sandboxes", nil); err == nil {
		t.Fatal("a stream without a deadline")
	}
}
