package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

// debugState is what the fake daemon's debug methods saw.
type debugState struct {
	mu       sync.Mutex
	execs    []DebugExecRequest
	shells   []string
	releases []string
}

var debugSeen sync.Map // *fakeHandler -> *debugState

func (f *fakeHandler) debug() *debugState {
	v, _ := debugSeen.LoadOrStore(f, &debugState{})
	return v.(*debugState)
}

func (f *fakeHandler) Tests(ctx context.Context, id string) (runs.TestsAnswer, error) {
	if id != "tm-a-1" {
		return runs.TestsAnswer{}, core.ErrNotFound
	}
	return runs.TestsAnswer{Record: runs.Record{ID: id, Device: "box"}, Results: runs.TestResults{ID: id,
		Cases: []runs.TestCase{{Name: "TestX", Status: runs.TestFailed, Message: "boom"}}}}, nil
}

func (f *fakeHandler) Flaky(ctx context.Context, q runs.Query) (FlakyAnswer, error) {
	return FlakyAnswer{Since: q.Since, Tests: []runs.FlakyTest{{Repo: q.Repo, Job: "test", Test: "TestX", Reason: runs.FlakyFlipped}}}, nil
}

func (f *fakeHandler) Why(ctx context.Context, req WhyRequest) (runs.WhyAnswer, error) {
	if req.ID == "tm-zz" {
		return runs.WhyAnswer{}, core.ErrNotFound
	}
	return runs.WhyAnswer{ID: firstNonEmptyTest(req.ID, "tm-a-1"), Repo: req.Repo, SHA: req.SHA, RunID: req.RunID, Conclusion: runs.Failure}, nil
}

func firstNonEmptyTest(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (f *fakeHandler) DebugExec(ctx context.Context, req DebugExecRequest) (DebugExecAnswer, error) {
	d := f.debug()
	d.mu.Lock()
	d.execs = append(d.execs, req)
	d.mu.Unlock()
	if req.ID != "tm-a-1" {
		return DebugExecAnswer{}, fmt.Errorf("no kept sandbox for job %s: %w", req.ID, core.ErrNotFound)
	}
	return DebugExecAnswer{ID: req.ID, ExitCode: 3, Output: "out of " + req.Command + "\n"}, nil
}

func (f *fakeHandler) DebugShell(ctx context.Context, id string, stdin io.Reader, stdout io.Writer) error {
	d := f.debug()
	d.mu.Lock()
	d.shells = append(d.shells, id)
	d.mu.Unlock()
	if id != "tm-a-1" {
		return fmt.Errorf("no kept sandbox for job %s: %w", id, core.ErrNotFound)
	}
	if _, err := io.WriteString(stdout, "shell in "+id+"\n"); err != nil {
		return err
	}
	_, err := io.Copy(stdout, stdin)
	return err
}

func (f *fakeHandler) DebugRelease(ctx context.Context, id string) error {
	d := f.debug()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.releases = append(d.releases, id)
	return nil
}

func TestTestsFlakyWhyRoutes(t *testing.T) {
	_, c, _ := start(t)
	ctx := context.Background()
	noTok := &Client{Addr: c.Addr}
	ta, err := noTok.Tests(ctx, "tm-a-1")
	if err != nil || len(ta.Results.Cases) != 1 || ta.Results.Cases[0].Message != "boom" {
		t.Fatalf("tests = %+v %v", ta, err)
	}
	var e *Error
	if _, err := noTok.Tests(ctx, "tm-zz"); !errors.As(err, &e) || e.Status != http.StatusNotFound {
		t.Fatalf("missing tests: %v", err)
	}
	if _, err := noTok.Tests(ctx, "../x"); !errors.As(err, &e) || e.Status != http.StatusBadRequest {
		t.Fatalf("bad id: %v", err)
	}
	since := time.Unix(1_800_000_000, 0).UTC()
	fa, err := noTok.Flaky(ctx, runs.Query{Repo: "o/r", Since: since})
	if err != nil || len(fa.Tests) != 1 || fa.Tests[0].Repo != "o/r" || !fa.Since.Equal(since) {
		t.Fatalf("flaky = %+v %v", fa, err)
	}
	for _, req := range []WhyRequest{{ID: "tm-a-1"}, {Repo: "o/r", SHA: "abcdef1"}, {Repo: "o/r", RunID: 9}} {
		w, err := noTok.Why(ctx, req)
		if err != nil || w.Conclusion != runs.Failure || w.Repo != req.Repo || w.RunID != req.RunID {
			t.Errorf("why %+v = %+v %v", req, w, err)
		}
	}
	for _, req := range []WhyRequest{{}, {Repo: "o/r"}, {Repo: "o/r", SHA: "abcdef1", RunID: 9}, {ID: "tm-a-1", Repo: "o/r"}, {ID: "../x"}} {
		if _, err := noTok.Why(ctx, req); !errors.As(err, &e) || e.Status != http.StatusBadRequest {
			t.Errorf("why %+v: %v, want 400", req, err)
		}
	}
	if _, err := noTok.Why(ctx, WhyRequest{ID: "tm-zz"}); !errors.As(err, &e) || e.Status != http.StatusNotFound {
		t.Errorf("why of a missing job: %v", err)
	}
}

// startLogged is startAs with the server's log in buf.
func startLogged(t *testing.T, ident Identify, buf *syncBuf) (*fakeHandler, *Client) {
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
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	go func() { done <- NewServer(h, tok, ident, log).Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return h, &Client{Addr: ln.Addr().String(), TokenPath: tokPath}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The debug routes take the token or an admin caller. A plain user
// without the token gets 401 and a CI job 403, token or not, and neither
// reaches the daemon. The server logs the job id and exit code, never the
// command or its output.
func TestDebugRoutesGuard(t *testing.T) {
	ctx := context.Background()
	const secretCmd = "cat /tmp/very-private-file"
	var logBuf syncBuf
	h, c := startLogged(t, func(net.Conn) (Caller, error) { return reader, nil }, &logBuf)

	ans, err := c.DebugExec(ctx, DebugExecRequest{ID: "tm-a-1", Command: secretCmd, TimeoutS: 5, MaxBytes: 100})
	if err != nil || ans.ExitCode != 3 || ans.Output != "out of "+secretCmd+"\n" {
		t.Fatalf("exec with the token = %+v %v", ans, err)
	}
	var out bytes.Buffer
	if err := c.DebugShell(ctx, "tm-a-1", strings.NewReader("ls -la\n"), &out); err != nil || out.String() != "shell in tm-a-1\nls -la\n" {
		t.Fatalf("shell with the token = %q %v", out.String(), err)
	}
	if err := c.DebugRelease(ctx, "tm-a-1"); err != nil {
		t.Fatalf("release with the token: %v", err)
	}
	var e *Error
	if _, err := c.DebugExec(ctx, DebugExecRequest{ID: "tm-zz", Command: "true"}); !errors.As(err, &e) || e.Status != http.StatusNotFound {
		t.Fatalf("exec in a job with no kept sandbox: %v", err)
	}
	if err := c.DebugShell(ctx, "tm-zz", strings.NewReader(""), io.Discard); !errors.As(err, &e) || e.Status != http.StatusNotFound ||
		!strings.Contains(e.Message, "no kept sandbox") {
		t.Fatalf("shell in a job with no kept sandbox: %v", err)
	}
	for _, req := range []DebugExecRequest{{ID: "tm-a-1"}, {ID: "../x", Command: "true"}, {ID: "tm-a-1", Command: "true", TimeoutS: MaxDebugTimeoutS + 1},
		{ID: "tm-a-1", Command: "true", MaxBytes: MaxDebugBytes + 1}, {ID: "tm-a-1", Command: "true", TimeoutS: -1}} {
		if _, err := c.DebugExec(ctx, req); !errors.As(err, &e) || e.Status != http.StatusBadRequest {
			t.Errorf("exec %+v: %v, want 400", req, err)
		}
	}
	if logs := logBuf.String(); strings.Contains(logs, "very-private") || strings.Contains(logs, "ls -la") ||
		!strings.Contains(logs, "runner=tm-a-1") || !strings.Contains(logs, "exit_code=3") {
		t.Errorf("server log:\n%s", logs)
	}

	// A plain user with a wrong token, or none at all, gets 401.
	bad := filepath.Join(t.TempDir(), TokenFile)
	os.WriteFile(bad, []byte(strings.Repeat("a", 64)), 0o600)
	plain := &Client{Addr: c.Addr, TokenPath: bad}
	if _, err := plain.DebugExec(ctx, DebugExecRequest{ID: "tm-a-1", Command: "true"}); !errors.As(err, &e) || e.Status != http.StatusUnauthorized {
		t.Errorf("exec as a plain user: %v, want 401", err)
	}
	if err := plain.DebugShell(ctx, "tm-a-1", strings.NewReader("x\n"), io.Discard); !errors.As(err, &e) || e.Status != http.StatusUnauthorized {
		t.Errorf("shell as a plain user: %v, want 401", err)
	}
	if err := plain.DebugRelease(ctx, "tm-a-1"); !errors.As(err, &e) || e.Status != http.StatusUnauthorized {
		t.Errorf("release as a plain user: %v, want 401", err)
	}
	for _, path := range []string{"/v1/debug/exec", "/v1/debug/shell", "/v1/debug/release"} {
		resp, err := http.Post("http://"+c.Addr+path, "application/json", strings.NewReader(`{"id":"tm-a-1","command":"true"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST %s with no token: %d, want 401", path, resp.StatusCode)
		}
	}

	// An admin needs no token.
	_, admin, _ := startAs(t, func(net.Conn) (Caller, error) { return Caller{Admin: true, Who: "uid 0"}, nil })
	admin.TokenPath = bad
	if ans, err := admin.DebugExec(ctx, DebugExecRequest{ID: "tm-a-1", Command: "id"}); err != nil || ans.ExitCode != 3 {
		t.Errorf("exec as an admin with a wrong token: %+v %v", ans, err)
	}
	out.Reset()
	if err := admin.DebugShell(ctx, "tm-a-1", strings.NewReader("pwd\n"), &out); err != nil || !strings.Contains(out.String(), "pwd") {
		t.Errorf("shell as an admin: %q %v", out.String(), err)
	}

	// A CI job gets 403 with the right token.
	rh, runner, _ := startAs(t, func(net.Conn) (Caller, error) { return Caller{Runner: true, Who: "uid 900"}, nil })
	if _, err := runner.DebugExec(ctx, DebugExecRequest{ID: "tm-a-1", Command: "id"}); !errors.As(err, &e) || e.Status != http.StatusForbidden {
		t.Errorf("exec as a CI job: %v, want 403", err)
	}
	if err := runner.DebugShell(ctx, "tm-a-1", strings.NewReader("id\n"), io.Discard); !errors.As(err, &e) || e.Status != http.StatusForbidden {
		t.Errorf("shell as a CI job: %v, want 403", err)
	}
	if err := runner.DebugRelease(ctx, "tm-a-1"); !errors.As(err, &e) || e.Status != http.StatusForbidden {
		t.Errorf("release as a CI job: %v, want 403", err)
	}
	for _, path := range []string{"/v1/runs/tests?id=tm-a-1", "/v1/runs/flaky", "/v1/runs/why?id=tm-a-1"} {
		if err := runner.Get(ctx, path, nil); !errors.As(err, &e) || e.Status != http.StatusForbidden {
			t.Errorf("GET %s as a CI job: %v, want 403", path, err)
		}
	}
	rd := rh.debug()
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if len(rd.execs)+len(rd.shells)+len(rd.releases) != 0 {
		t.Fatalf("the daemon acted for a CI job: %+v", rd)
	}
	d := h.debug()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.execs) != 2 || len(d.shells) != 2 || len(d.releases) != 1 {
		t.Fatalf("the daemon saw execs %d shells %d releases %d", len(d.execs), len(d.shells), len(d.releases))
	}
}

// A shell that runs until the caller goes away ends when the client's
// context does.
func TestDebugShellContext(t *testing.T) {
	_, c, _ := start(t)
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	defer pw.Close()
	done := make(chan error, 1)
	var out syncBuf
	go func() { done <- c.DebugShell(ctx, "tm-a-1", pr, &out) }()
	io.WriteString(pw, "hello\n")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "hello") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "shell in tm-a-1\nhello\n") {
		t.Fatalf("shell output %q", out.String())
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("shell after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the shell did not end with its context")
	}
}
