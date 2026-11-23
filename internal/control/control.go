// Package control is the daemon's local control API. On Linux and macOS
// the daemon serves HTTP on the Unix socket /var/run/tentmaker/control.sock;
// on Windows on 127.0.0.1:7434.
//
// The server names every caller from the connection, never from anything
// it sends: the socket's peer uid on Linux and macOS, and on Windows the
// account of the process that owns the TCP connection. A CI job (the
// runner user, or on Linux one of its subordinate uids) gets 403 on every
// route, wherever the job fence leaks; Windows Firewall doesn't filter
// loopback at all. Anyone else may read status and pool, the metrics
// (GET /metrics) and the status page (GET /). Doctor makes the
// daemon call the forges, so it needs the token or an admin caller (root,
// SYSTEM or an elevated Administrator). POSTs (pause, resume, reload) need
// the token from <state>/control.token, which only root (or SYSTEM and
// Administrators on Windows) can read. The debug routes (debug.go), which
// run commands in a failed job's kept sandbox, take the token or an admin
// caller.
//
// The source archive relay has its own TCP listener, 127.0.0.1:7435, which
// serves GET /v1/relay/<id> and nothing else: it streams a running check
// job's source archive, and the id is 32 random bytes the worker minted for
// that job alone, so it needs no token and names no caller.
package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/httpsafe"
)

// TCPAddr is the control API's address on Windows (DefaultAddr there).
const TCPAddr = "127.0.0.1:7434"

// unixPrefix marks a control address that is a Unix socket path.
const unixPrefix = "unix:"

// DefaultRelayAddr is where the daemon serves the source archive relay. The
// job fence allows this port and blocks DefaultAddr; keep it in step with
// RelayPort in internal/executor/linux and internal/executor/windows.
const DefaultRelayAddr = "127.0.0.1:7435"

// DefaultCacheAddr is where the daemon serves the GitHub Actions cache
// service (internal/actionscache), and the build caches beside it
// (internal/buildcache), to Linux runner jobs. The job fence allows
// it like the relay; keep it in step with CachePort in internal/executor/linux.
const DefaultCacheAddr = "127.0.0.1:7436"

// TokenFile is the token's name in the state directory.
const TokenFile = "control.token"

// Timeouts.
const (
	ClientTimeout = 30 * time.Second
	maxBody       = 1 << 20
)

// TokenPath is the token file in stateDir.
func TokenPath(stateDir string) string { return filepath.Join(stateDir, TokenFile) }

// Handler is what the daemon serves.
type Handler interface {
	Status(ctx context.Context) (any, error)
	Pool(ctx context.Context) (any, error)
	Doctor(ctx context.Context) (any, error)
	Pause(ctx context.Context, reason string, d time.Duration) error
	Resume(ctx context.Context) error
	Reload(ctx context.Context) error
}

// PROpener is a Handler that can open a pull request with the forge token
// the daemon holds (POST /v1/pr, token only). It is optional: a Handler
// without it serves no /v1/pr. The token never leaves the daemon; the
// caller gets the pull request's number and URL.
type PROpener interface {
	OpenPR(ctx context.Context, pr core.PullRequest) (core.PullRequestResult, error)
}

// ViewsHandler serves the metrics and the status page (internal/observe):
// GET /metrics, and the page at GET / and GET /status. They are open like
// status, so every caller check in ServeHTTP applies and a CI job gets 403.
type ViewsHandler interface {
	Views() http.Handler
}

// Relayer streams check jobs' source archives. An unknown or expired id is
// core.ErrNotFound.
type Relayer interface {
	Relay(ctx context.Context, id string, w io.Writer) error
}

// relayTimeout bounds one relay stream.
const relayTimeout = 11 * time.Minute

// PauseRequest is the body of POST /v1/pause.
type PauseRequest struct {
	Reason string `json:"reason"`
	ForS   int64  `json:"for_s"` // 0 = until resume
}

// NewToken makes 32 random bytes, hex encoded, and writes them to path so
// only root (Windows: SYSTEM and Administrators) can read them.
func NewToken(path string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := writeToken(path, []byte(tok+"\n")); err != nil {
		return "", fmt.Errorf("write control token: %w", err)
	}
	return tok, nil
}

// ReadToken reads the token file.
func ReadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if len(tok) != 64 {
		return "", errors.New("control token file is malformed")
	}
	return tok, nil
}

// Listen opens a listener on addr: "unix:<absolute path>" for a Unix
// socket anyone may connect to, or a loopback IP and port.
func Listen(addr string) (net.Listener, error) {
	if path, ok := strings.CutPrefix(addr, unixPrefix); ok {
		return listenUnix(path)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("control API address %q must be a loopback IP like 127.0.0.1", addr)
	}
	return net.Listen("tcp", addr)
}

// Server serves the control API or the relay.
type Server struct {
	relayer Relayer
	token   string
	ident   Identify
	guarded bool // the control API: every request needs a named caller that is not a CI job
	log     *slog.Logger
	mux     *http.ServeMux
}

// NewServer returns a Server for the control API. ident names each
// connection's caller (HostIdentify in the daemon); a CI job gets 403 on
// every route, and so does every caller when ident is nil or fails. Doctor
// needs token or an admin caller, and POSTs need token. It serves no relay;
// see NewRelayServer.
func NewServer(h Handler, token string, ident Identify, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{token: token, ident: ident, guarded: true, log: log, mux: http.NewServeMux()}
	get := func(f func(ctx context.Context) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			v, err := f(r.Context())
			s.reply(w, v, err)
		}
	}
	s.mux.HandleFunc("GET /v1/status", get(h.Status))
	s.mux.HandleFunc("GET /v1/pool", get(h.Pool))
	s.mux.HandleFunc("GET /v1/doctor", s.tokenOrAdmin(get(h.Doctor)))
	s.mux.HandleFunc("POST /v1/pause", s.auth(func(w http.ResponseWriter, r *http.Request) {
		var req PauseRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"reason\": \"...\", \"for_s\": N}"})
			return
		}
		if req.ForS < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "for_s can't be negative"})
			return
		}
		s.reply(w, nil, h.Pause(r.Context(), req.Reason, time.Duration(req.ForS)*time.Second))
	}))
	s.mux.HandleFunc("POST /v1/resume", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, nil, h.Resume(r.Context()))
	}))
	s.mux.HandleFunc("POST /v1/reload", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.reply(w, nil, h.Reload(r.Context()))
	}))
	if vh, ok := h.(ViewsHandler); ok {
		if v := vh.Views(); v != nil {
			s.mux.Handle("GET /metrics", v)
			s.mux.Handle("GET /status", v)
			s.mux.Handle("GET /{$}", v)
		}
	}
	if rh, ok := h.(RunsHandler); ok {
		s.addRuns(rh)
	}
	if th, ok := h.(TestsHandler); ok {
		s.addTests(th)
	}
	if dh, ok := h.(DebugHandler); ok {
		s.addDebug(dh)
	}
	if po, ok := h.(PROpener); ok {
		s.mux.HandleFunc("POST /v1/pr", s.auth(func(w http.ResponseWriter, r *http.Request) {
			var req core.PullRequest
			if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&req); err != nil || req.Repo == "" || req.Head == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"repo\": \"owner/name\", \"head\": \"branch\", \"title\": ..., \"body\": ...}"})
				return
			}
			res, err := po.OpenPR(r.Context(), req)
			s.reply(w, res, err)
		}))
	}
	if sh, ok := h.(SandboxHandler); ok {
		s.sandboxRoutes(sh)
	}
	if lh, ok := h.(LendHandler); ok {
		s.lendRoutes(lh)
	}
	return s
}

// NewRelayServer returns a Server for the relay listener: GET
// /v1/relay/<id> and nothing else, everything else 404.
func NewRelayServer(r Relayer, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{relayer: r, log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /v1/relay/{id}", s.relay)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "the relay listener serves only /v1/relay/<id>"})
	})
	return s
}

// NewMetricsServer returns a Server for the loopback metrics listener
// (config metrics_listen): GET /metrics from views and nothing else, so a
// Prometheus on this device can scrape it over TCP, which the control
// API's Unix socket doesn't allow. Like the control API, it names every
// caller from the connection (TCPIdentify in the daemon) and refuses a CI
// job, and every caller when ident is nil or can't name one. It takes no
// token and has no other route.
func NewMetricsServer(views http.Handler, ident Identify, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{ident: ident, guarded: true, log: log, mux: http.NewServeMux()}
	if views != nil {
		s.mux.Handle("GET /metrics", views)
	}
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "the metrics listener serves only GET /metrics"})
	})
	return s
}

// ServeHTTP checks the Host header (no DNS rebinding) and dispatches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip, err := netip.ParseAddr(host); !(host == "localhost" || (err == nil && ip.IsLoopback())) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "the control API answers only on loopback"})
		return
	}
	if s.guarded {
		c, err := callerOf(r.Context())
		switch {
		case err != nil:
			s.log.Warn("control: refused a caller it can't name", "err", err)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: " + err.Error()})
			return
		case c.Runner:
			s.log.Warn("control: refused a CI job", "caller", c.Who, "path", r.URL.Path)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden: CI jobs can't use the control API"})
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) relay(w http.ResponseWriter, r *http.Request) {
	rl := s.relayer
	if rl == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such relay"})
		return
	}
	// The server's write timeout is for JSON answers; an archive takes
	// longer.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(relayTimeout + time.Minute))
	ctx, cancel := context.WithTimeout(r.Context(), relayTimeout)
	defer cancel()
	lw := &lazyWriter{w: w}
	err := rl.Relay(ctx, r.PathValue("id"), lw)
	switch {
	case err == nil:
		if !lw.wrote {
			lw.start()
		}
	case lw.wrote:
		// Cut the connection so the job sees a broken download, never a
		// short archive that looks whole.
		s.log.Warn("control: relay stream broke", "err", err)
		panic(http.ErrAbortHandler)
	case errors.Is(err, core.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such relay"})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the source archive is unavailable: " + err.Error()})
	}
}

// lazyWriter sends the 200 header on the first byte, so an error before
// any byte can still answer with an error status.
type lazyWriter struct {
	w     http.ResponseWriter
	wrote bool
}

func (l *lazyWriter) start() {
	l.wrote = true
	l.w.Header().Set("Content-Type", "application/gzip")
	l.w.Header().Set("Cache-Control", "no-store")
	l.w.WriteHeader(http.StatusOK)
}

func (l *lazyWriter) Write(p []byte) (int, error) {
	if !l.wrote {
		l.start()
	}
	return l.w.Write(p)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || s.token == "" || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or wrong control token"})
			return
		}
		next(w, r)
	}
}

// tokenOrAdmin serves an admin caller, or anyone with the token.
func (s *Server) tokenOrAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := callerOf(r.Context()); err == nil && c.Admin {
			next(w, r)
			return
		}
		s.auth(next)(w, r)
	}
}

func (s *Server) reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if v == nil {
		v = map[string]bool{"ok": true}
	}
	writeJSON(w, http.StatusOK, v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve serves on ln until ctx ends, then shuts down within 5 s.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
		ConnContext:       s.ConnContext,
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
			return
		}
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(ln)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ErrNotRunning means nothing answers on the control address.
var ErrNotRunning = errors.New("service not running")

// Error is a non-success answer from the daemon.
type Error struct {
	Status  int
	Message string
	Fix     string // what to do about it, when the daemon said
}

func (e *Error) Error() string { return e.Message }

// Client talks to the daemon.
type Client struct {
	Addr      string // default DefaultAddr; "unix:<path>" for a Unix socket
	TokenPath string // read for POSTs, and sent on GETs when readable
	HTTP      *http.Client
}

func (c *Client) addr() string {
	if c.Addr == "" {
		return DefaultAddr
	}
	return c.Addr
}

func (c *Client) http(timeout time.Duration) *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	hc := &http.Client{Timeout: timeout}
	if path, ok := strings.CutPrefix(c.addr(), unixPrefix); ok {
		hc.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
			DisableKeepAlives: true,
		}
	}
	return httpsafe.Guard(hc)
}

func (c *Client) url(path string) string {
	addr := c.addr()
	if strings.HasPrefix(addr, unixPrefix) {
		addr = "localhost"
	}
	return "http://" + addr + path
}

// Get reads path into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post sends body (may be nil) to path with the token.
func (c *Client) Post(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// Call is Post that reads the answer into out.
func (c *Client) Call(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doFor(ctx, ClientTimeout, method, path, body, out)
}

// doFor is do with a time limit other than ClientTimeout.
func (c *Client) doFor(ctx context.Context, timeout time.Duration, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), rd)
	if err != nil {
		return err
	}
	if method == http.MethodPost {
		if err := c.authorize(req); err != nil {
			return err
		}
	} else if c.TokenPath != "" {
		// Doctor needs the token unless the caller is an admin; a reader
		// who can't read the token is served the rest.
		if tok, err := ReadToken(c.TokenPath); err == nil {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := c.http(timeout).Do(req)
	if err != nil {
		if isRefused(err) {
			return ErrNotRunning
		}
		return fmt.Errorf("control API: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return answerErr(resp, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func isRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	return false
}
