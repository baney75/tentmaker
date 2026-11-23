package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/runs"
)

// TestsHandler serves test results and failure answers. A Handler that
// also implements it gets these routes, open like /v1/runs/log:
//
//	GET /v1/runs/tests?id=                      runs.TestsAnswer
//	GET /v1/runs/flaky?repo=&since=             FlakyAnswer
//	GET /v1/runs/why?id= | repo=&sha= | repo=&run_id=   runs.WhyAnswer
type TestsHandler interface {
	Tests(ctx context.Context, id string) (runs.TestsAnswer, error)
	Flaky(ctx context.Context, q runs.Query) (FlakyAnswer, error)
	Why(ctx context.Context, req WhyRequest) (runs.WhyAnswer, error)
}

// DebugHandler runs commands in a failed job's kept sandbox on this
// device. A Handler that also implements it gets these routes, which need
// the token or an admin caller (CI jobs get 403 like everywhere):
//
//	POST /v1/debug/exec    {id, command, timeout_s, max_bytes}  DebugExecAnswer
//	POST /v1/debug/release {id}
//	POST /v1/debug/shell   {id}  101 Switching Protocols, then raw bytes
//
// The server never logs a command or its output.
type DebugHandler interface {
	DebugExec(ctx context.Context, req DebugExecRequest) (DebugExecAnswer, error)
	DebugShell(ctx context.Context, id string, stdin io.Reader, stdout io.Writer) error
	DebugRelease(ctx context.Context, id string) error
}

// FlakyAnswer is GET /v1/runs/flaky.
type FlakyAnswer struct {
	Since  time.Time        `json:"since"`
	Tests  []runs.FlakyTest `json:"tests"`
	Errors []string         `json:"errors,omitempty"`
}

// WhyRequest names the failed job to explain: a job id, or a repo with a
// commit or a run id (the newest failed job of it).
type WhyRequest struct {
	ID    string `json:"id,omitempty"`
	Repo  string `json:"repo,omitempty"`
	SHA   string `json:"sha,omitempty"`
	RunID int64  `json:"run_id,omitempty"`
}

// Values encodes r as URL query parameters.
func (r WhyRequest) Values() string {
	q := runs.Query{ID: r.ID, Repo: r.Repo, SHA: r.SHA, RunID: r.RunID}
	return q.Values().Encode()
}

// Check says what is wrong with r, or nil.
func (r WhyRequest) Check() error {
	switch {
	case r.ID != "":
		if r.Repo != "" || r.SHA != "" || r.RunID != 0 {
			return errors.New("name a job id alone, or a repo with a sha or a run id")
		}
		if !runs.ValidID(r.ID) {
			return errors.New("bad job id")
		}
	case r.Repo == "":
		return errors.New("name a job id, or a repo with a sha or a run id")
	case (r.SHA == "") == (r.RunID == 0):
		return errors.New("with a repo, name a sha or a run id (one of them)")
	}
	return nil
}

// Debug limits; they match internal/dispatch's (a daemon test checks).
const (
	MaxDebugTimeoutS = 600
	MaxDebugBytes    = 1 << 20
	// debugSlack is how long past a command's time limit the call may take.
	debugSlack = time.Minute
	// defaultDebugTimeout is what timeout_s 0 means, for the deadlines.
	defaultDebugTimeout = time.Minute
	// shellHandshake bounds the shell's request and its 101 answer.
	shellHandshake = 15 * time.Second
	// ShellUpgrade is the Upgrade token of POST /v1/debug/shell.
	ShellUpgrade = "tentmaker-debug"
)

// DebugExecRequest is the body of POST /v1/debug/exec. TimeoutS and
// MaxBytes of 0 take the defaults (60 s, 64 KiB).
type DebugExecRequest struct {
	ID       string `json:"id"`
	Command  string `json:"command"`
	TimeoutS int64  `json:"timeout_s,omitempty"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
}

// DebugExecAnswer is how the command ended. Output is redacted, stdout and
// stderr together, and its last max_bytes when Truncated.
type DebugExecAnswer struct {
	ID        string `json:"id"`
	ExitCode  int    `json:"exit_code"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	TimedOut  bool   `json:"timed_out"`
}

// DebugID is the body of POST /v1/debug/release and /v1/debug/shell.
type DebugID struct {
	ID string `json:"id"`
}

func (r DebugExecRequest) wait() time.Duration {
	d := time.Duration(r.TimeoutS) * time.Second
	if d <= 0 {
		d = defaultDebugTimeout
	}
	return d + debugSlack
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

func (s *Server) addTests(h TestsHandler) {
	s.mux.HandleFunc("GET /v1/runs/tests", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if !runs.ValidID(id) {
			badRequest(w, "bad job id")
			return
		}
		v, err := h.Tests(r.Context(), id)
		s.replyErr(w, v, err)
	})
	s.mux.HandleFunc("GET /v1/runs/flaky", func(w http.ResponseWriter, r *http.Request) {
		q, err := runs.ParseQuery(r.URL.Query())
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		v, err := h.Flaky(r.Context(), q)
		s.replyErr(w, v, err)
	})
	s.mux.HandleFunc("GET /v1/runs/why", func(w http.ResponseWriter, r *http.Request) {
		q, err := runs.ParseQuery(r.URL.Query())
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		req := WhyRequest{ID: q.ID, Repo: q.Repo, SHA: q.SHA, RunID: q.RunID}
		if err := req.Check(); err != nil {
			badRequest(w, err.Error())
			return
		}
		v, err := h.Why(r.Context(), req)
		s.replyErr(w, v, err)
	})
}

func (s *Server) addDebug(h DebugHandler) {
	s.mux.HandleFunc("POST /v1/debug/exec", s.tokenOrAdmin(func(w http.ResponseWriter, r *http.Request) {
		var req DebugExecRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil || !runs.ValidID(req.ID) ||
			strings.TrimSpace(req.Command) == "" {
			badRequest(w, `body must be {"id": "tm-...", "command": "...", "timeout_s": N, "max_bytes": N}`)
			return
		}
		if req.TimeoutS < 0 || req.TimeoutS > MaxDebugTimeoutS || req.MaxBytes < 0 || req.MaxBytes > MaxDebugBytes {
			badRequest(w, fmt.Sprintf("timeout_s must be 0 to %d and max_bytes 0 to %d", MaxDebugTimeoutS, MaxDebugBytes))
			return
		}
		// The server's write timeout is for quick answers; a command may
		// run for minutes.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(req.wait() + debugSlack))
		v, err := h.DebugExec(r.Context(), req)
		if err != nil {
			s.log.Info("control: debug command refused", "runner", req.ID)
		} else {
			s.log.Info("control: debug command ran", "runner", req.ID, "exit_code", v.ExitCode)
		}
		s.replyErr(w, v, err)
	}))
	s.mux.HandleFunc("POST /v1/debug/release", s.tokenOrAdmin(func(w http.ResponseWriter, r *http.Request) {
		var req DebugID
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil || !runs.ValidID(req.ID) {
			badRequest(w, `body must be {"id": "tm-..."}`)
			return
		}
		s.replyErr(w, nil, h.DebugRelease(r.Context(), req.ID))
	}))
	s.mux.HandleFunc("POST /v1/debug/shell", s.tokenOrAdmin(func(w http.ResponseWriter, r *http.Request) {
		var req DebugID
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil || !runs.ValidID(req.ID) {
			badRequest(w, `body must be {"id": "tm-..."}`)
			return
		}
		s.debugShell(w, r, h, req.ID)
	}))
}

// debugShell runs the shell over the hijacked connection. It answers 101
// only once the shell reads or writes, so a job with no kept sandbox still
// gets a JSON error.
func (s *Server) debugShell(w http.ResponseWriter, r *http.Request, h DebugHandler, id string) {
	u := &upgrader{w: w}
	err := h.DebugShell(r.Context(), id, u.reader(), u.writer())
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case u.conn == nil && u.err == nil:
		if err == nil {
			err = errors.New("the shell ended before it started")
		}
		s.log.Info("control: debug shell refused", "runner", id)
		s.replyErr(w, nil, err)
		return
	case u.conn == nil:
		s.log.Warn("control: debug shell upgrade failed", "runner", id, "error", u.err)
		return
	}
	if err != nil {
		fmt.Fprintf(u.conn, "\r\ntentmaker: the debug shell ended: %v\r\n", err)
	}
	_ = u.conn.Close()
	s.log.Info("control: debug shell closed", "runner", id)
}

// upgrader hijacks the connection on the shell's first read or write.
type upgrader struct {
	w    http.ResponseWriter
	mu   sync.Mutex
	conn net.Conn
	rd   io.Reader
	err  error
}

func (u *upgrader) start() (net.Conn, io.Reader, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.conn != nil || u.err != nil {
		return u.conn, u.rd, u.err
	}
	conn, brw, err := http.NewResponseController(u.w).Hijack()
	if err != nil {
		u.err = err
		return nil, nil, err
	}
	// The server's read and write deadlines are for quick answers.
	_ = conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: "+ShellUpgrade+"\r\nConnection: Upgrade\r\n\r\n"); err != nil {
		conn.Close()
		u.err = err
		return nil, nil, err
	}
	u.conn, u.rd = conn, brw.Reader
	return u.conn, u.rd, nil
}

type upgradeReader struct{ u *upgrader }

func (r upgradeReader) Read(p []byte) (int, error) {
	_, rd, err := r.u.start()
	if err != nil {
		return 0, err
	}
	return rd.Read(p)
}

type upgradeWriter struct{ u *upgrader }

func (w upgradeWriter) Write(p []byte) (int, error) {
	conn, _, err := w.u.start()
	if err != nil {
		return 0, err
	}
	return conn.Write(p)
}

func (u *upgrader) reader() io.Reader { return upgradeReader{u} }
func (u *upgrader) writer() io.Writer { return upgradeWriter{u} }

// ---------------------------------------------------------------- client

// Tests reads a job's test results.
func (c *Client) Tests(ctx context.Context, id string) (runs.TestsAnswer, error) {
	var v runs.TestsAnswer
	err := c.Get(ctx, "/v1/runs/tests?id="+id, &v)
	return v, err
}

// Flaky lists the flaky tests in the pool's records that q matches.
func (c *Client) Flaky(ctx context.Context, q runs.Query) (FlakyAnswer, error) {
	var v FlakyAnswer
	err := c.Get(ctx, "/v1/runs/flaky?"+q.Values().Encode(), &v)
	return v, err
}

// Why explains a failed job.
func (c *Client) Why(ctx context.Context, req WhyRequest) (runs.WhyAnswer, error) {
	var v runs.WhyAnswer
	err := c.Get(ctx, "/v1/runs/why?"+req.Values(), &v)
	return v, err
}

// DebugExec runs one command in a kept sandbox and waits for it.
func (c *Client) DebugExec(ctx context.Context, req DebugExecRequest) (DebugExecAnswer, error) {
	var v DebugExecAnswer
	err := c.doFor(ctx, req.wait()+ClientTimeout, http.MethodPost, "/v1/debug/exec", req, &v)
	return v, err
}

// DebugRelease ends a kept sandbox's grace now.
func (c *Client) DebugRelease(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/debug/release", DebugID{ID: id}, nil)
}

// DebugShell opens an interactive shell in job id's kept sandbox and
// copies stdin to it and its output to stdout until it ends or ctx does.
func (c *Client) DebugShell(ctx context.Context, id string, stdin io.Reader, stdout io.Writer) error {
	body, _ := json.Marshal(DebugID{ID: id})
	req, err := http.NewRequest(http.MethodPost, c.url("/v1/debug/shell"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if err := c.authorize(req); err != nil {
		return err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", ShellUpgrade)
	conn, err := c.dial(ctx)
	if err != nil {
		if isRefused(err) {
			return ErrNotRunning
		}
		return fmt.Errorf("control API: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(shellHandshake))
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("control API: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return fmt.Errorf("control API: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return answerErr(resp, data)
	}
	_ = conn.SetDeadline(time.Time{})
	go func() {
		_, _ = io.Copy(conn, stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, err = io.Copy(stdout, br)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// dial connects to the control address.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: shellHandshake}
	if path, ok := strings.CutPrefix(c.addr(), unixPrefix); ok {
		return d.DialContext(ctx, "unix", path)
	}
	return d.DialContext(ctx, "tcp", c.addr())
}

// authorize sets the token on a POST, or says why it can't.
func (c *Client) authorize(req *http.Request) error {
	tok, err := ReadToken(c.TokenPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotRunning
		}
		return fmt.Errorf("read the control token (needs root or Administrator): %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	return nil
}

// answerErr turns a non-success answer into an *Error.
func answerErr(resp *http.Response, data []byte) error {
	var e struct {
		Error string `json:"error"`
		Fix   string `json:"fix"`
	}
	_ = json.Unmarshal(data, &e)
	if e.Error == "" {
		e.Error = resp.Status
	}
	return &Error{Status: resp.StatusCode, Message: e.Error, Fix: e.Fix}
}
