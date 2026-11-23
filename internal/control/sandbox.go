package control

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/sandbox"
)

// ClientHeader must be on every sandbox request. A web page can't send it
// without a CORS preflight, which the API never answers, so a page open in
// a browser on the device can't drive sandboxes through the loopback port.
const ClientHeader = "Tentmaker-Client"

// SandboxService runs sandboxes for named callers (sandbox.Manager).
type SandboxService interface {
	Create(ctx context.Context, c sandbox.Caller, req sandbox.CreateRequest) (sandbox.Info, error)
	List(ctx context.Context, c sandbox.Caller) ([]sandbox.Info, error)
	Get(ctx context.Context, c sandbox.Caller, id string) (sandbox.Info, error)
	Remove(ctx context.Context, c sandbox.Caller, id string) error
	Exec(ctx context.Context, c sandbox.Caller, id string, req core.SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	CopyIn(ctx context.Context, c sandbox.Caller, id, dst string, in io.Reader) (sandbox.CopyResult, error)
	CopyOut(ctx context.Context, c sandbox.Caller, id, src string, out, stderr io.Writer) (int, error)
}

// SandboxHandler is a Handler that also serves sandboxes.
type SandboxHandler interface {
	SandboxService() SandboxService
}

// Sandbox timeouts.
const (
	sandboxCreateTimeout = 11 * time.Minute // dispatch's start timeout is 10
	sandboxCopyTimeout   = 30 * time.Minute
	maxExecLine          = 1 << 20
)

func (s *Server) sandboxRoutes(h SandboxHandler) {
	route := func(f func(w http.ResponseWriter, r *http.Request, svc SandboxService, c sandbox.Caller)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, ok := s.sandboxCaller(w, r)
			if !ok {
				return
			}
			f(w, r, h.SandboxService(), c)
		}
	}
	s.mux.HandleFunc("POST /v1/sandboxes", route(func(w http.ResponseWriter, r *http.Request, svc SandboxService, c sandbox.Caller) {
		var req sandbox.CreateRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			sandboxError(w, &sandbox.Error{Status: http.StatusBadRequest, Msg: "the body must be a sandbox request as JSON", Fix: "use tentmaker sandbox create"})
			return
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(sandboxCreateTimeout + time.Minute))
		ctx, cancel := context.WithTimeout(r.Context(), sandboxCreateTimeout)
		defer cancel()
		info, err := svc.Create(ctx, c, req)
		sandboxReply(w, info, err)
	}))
	s.mux.HandleFunc("GET /v1/sandboxes", route(func(w http.ResponseWriter, r *http.Request, svc SandboxService, c sandbox.Caller) {
		list, err := svc.List(r.Context(), c)
		sandboxReply(w, map[string]any{"sandboxes": list}, err)
	}))
	s.mux.HandleFunc("GET /v1/sandboxes/{id}", route(func(w http.ResponseWriter, r *http.Request, svc SandboxService, c sandbox.Caller) {
		info, err := svc.Get(r.Context(), c, r.PathValue("id"))
		sandboxReply(w, info, err)
	}))
	s.mux.HandleFunc("DELETE /v1/sandboxes/{id}", route(func(w http.ResponseWriter, r *http.Request, svc SandboxService, c sandbox.Caller) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Minute))
		id := r.PathValue("id")
		err := svc.Remove(r.Context(), c, id)
		sandboxReply(w, map[string]any{"id": id, "removed": true}, err)
	}))
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/exec", route(s.sandboxExec))
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/files", route(func(w http.ResponseWriter, r *http.Request, svc SandboxService, c sandbox.Caller) {
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(sandboxCopyTimeout))
		_ = rc.SetWriteDeadline(time.Now().Add(sandboxCopyTimeout + time.Minute))
		ctx, cancel := context.WithTimeout(r.Context(), sandboxCopyTimeout)
		defer cancel()
		res, err := svc.CopyIn(ctx, c, r.PathValue("id"), r.URL.Query().Get("path"), r.Body)
		sandboxReply(w, res, err)
	}))
	s.mux.HandleFunc("GET /v1/sandboxes/{id}/files", route(func(w http.ResponseWriter, r *http.Request, svc SandboxService, c sandbox.Caller) {
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Now().Add(sandboxCopyTimeout + time.Minute))
		ctx, cancel := context.WithTimeout(r.Context(), sandboxCopyTimeout)
		defer cancel()
		fw, lw := frameResponse(w, rc)
		code, err := svc.CopyOut(ctx, c, r.PathValue("id"), r.URL.Query().Get("path"), fw.Stream(sandbox.FrameStdout), fw.Stream(sandbox.FrameStderr))
		finishFrames(w, fw, lw, code, err)
	}))
}

// sandboxCaller names the caller of a sandbox route: the account from the
// connection, an admin when it holds the control token.
func (s *Server) sandboxCaller(w http.ResponseWriter, r *http.Request) (sandbox.Caller, bool) {
	if r.Header.Get(ClientHeader) == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "sandbox requests need the " + ClientHeader + " header",
			"fix": "use the tentmaker CLI or tentmaker mcp"})
		return sandbox.Caller{}, false
	}
	c, err := callerOf(r.Context())
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: " + err.Error()})
		return sandbox.Caller{}, false
	}
	sc := sandbox.Caller{ID: c.ID, Admin: c.Admin}
	if got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && s.token != "" &&
		subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(s.token)) == 1 {
		sc.Admin = true
	}
	return sc, true
}

func (s *Server) sandboxExec(w http.ResponseWriter, r *http.Request, svc SandboxService, c sandbox.Caller) {
	id := r.PathValue("id")
	info, err := svc.Get(r.Context(), c, id)
	if err != nil {
		sandboxError(w, err)
		return
	}
	// The command may run until the sandbox expires; stdin may come in
	// while its output goes out.
	rc := http.NewResponseController(w)
	deadline := info.Expires.Add(time.Minute)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
	_ = rc.EnableFullDuplex()
	br := bufio.NewReader(r.Body)
	line, err := readLine(br, maxExecLine)
	var req sandbox.ExecRequest
	if err == nil {
		err = json.Unmarshal(line, &req)
	}
	if err != nil || req.TimeoutS < 0 {
		sandboxError(w, &sandbox.Error{Status: http.StatusBadRequest, Msg: "the body must start with an exec request as one JSON line",
			Fix: "use tentmaker sandbox exec"})
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), info.Expires)
	defer cancel()
	fw, lw := frameResponse(w, rc)
	code, err := svc.Exec(ctx, c, id, req.Core(), br, fw.Stream(sandbox.FrameStdout), fw.Stream(sandbox.FrameStderr))
	finishFrames(w, fw, lw, code, err)
}

// readLine reads one line of at most max bytes, without the newline.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		b, err := br.ReadSlice('\n')
		out = append(out, b...)
		if len(out) > max {
			return nil, errors.New("line too long")
		}
		if err == nil {
			return bytes.TrimRight(out, "\r\n"), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			if errors.Is(err, io.EOF) && len(out) > 0 {
				return out, nil
			}
			return nil, err
		}
	}
}

// frameResponse starts a frame stream whose 200 goes out with the first
// frame, so a refusal before any output can still answer with its status.
func frameResponse(w http.ResponseWriter, rc *http.ResponseController) (*sandbox.FrameWriter, *lazyFrames) {
	lw := &lazyFrames{w: w}
	return sandbox.NewFrameWriter(lw, func() { _ = rc.Flush() }), lw
}

type lazyFrames struct {
	w       http.ResponseWriter
	started bool
}

func (l *lazyFrames) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		l.w.Header().Set("Content-Type", sandbox.ContentType)
		l.w.Header().Set("Cache-Control", "no-store")
		l.w.WriteHeader(http.StatusOK)
	}
	return l.w.Write(p)
}

func finishFrames(w http.ResponseWriter, fw *sandbox.FrameWriter, lw *lazyFrames, code int, err error) {
	if err != nil && !lw.started {
		sandboxError(w, err)
		return
	}
	e := sandbox.Exit{ExitCode: code}
	if err != nil {
		e.ExitCode = -1
		e.Error = err.Error()
		var se *sandbox.Error
		if errors.As(err, &se) {
			e.Fix = se.Fix
		}
	}
	_ = fw.Exit(e)
}

func sandboxReply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		sandboxError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func sandboxError(w http.ResponseWriter, err error) {
	var se *sandbox.Error
	if errors.As(err, &se) {
		writeJSON(w, se.Status, map[string]string{"error": se.Msg, "fix": se.Fix})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error(), "fix": "check tentmaker status and the service log"})
}

// SandboxCall sends a sandbox request with a JSON body (nil for none) and reads
// the JSON answer into out. It sends the token when the caller can read
// it, and waits up to timeout.
func (c *Client) SandboxCall(ctx context.Context, method, path string, body, out any, timeout time.Duration) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := c.Stream(ctx, method, path, rd)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Stream sends a sandbox request and returns the answer for the caller to
// read and close. A non-success answer is an *Error. ctx bounds it all.
func (c *Client) Stream(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("a sandbox request needs a deadline")
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(ClientHeader, "tentmaker")
	if c.TokenPath != "" {
		if tok, err := ReadToken(c.TokenPath); err == nil {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	// No client time limit: ctx bounds the stream, and a fixed one would
	// cut off a long command.
	hc := c.http(0)
	resp, err := hc.Do(req)
	if err != nil {
		if isRefused(err) {
			return nil, ErrNotRunning
		}
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("control API: timed out: %w", err)
		}
		return nil, fmt.Errorf("control API: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, answerErr(resp, data)
	}
	return resp, nil
}
