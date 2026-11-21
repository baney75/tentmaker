// Package control is the daemon's loopback control API. The daemon serves
// HTTP on 127.0.0.1:7434 only. GETs (status, pool, doctor) need nothing;
// POSTs (pause, resume, reload) need the token from <state>/control.token,
// which only root (or SYSTEM and Administrators on Windows) can read.
// GET /v1/relay/<id> streams a running check job's source archive; the id is
// 32 random bytes the worker minted for that job alone, so it needs no token.
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
)

// DefaultAddr is where the daemon listens.
const DefaultAddr = "127.0.0.1:7434"

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

// Relayer is a Handler that also streams check jobs' source archives. An
// unknown or expired id is core.ErrNotFound.
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

// Listen opens a TCP listener on addr, which must be a loopback IP.
func Listen(addr string) (net.Listener, error) {
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

// Server serves the control API.
type Server struct {
	h     Handler
	token string
	log   *slog.Logger
	mux   *http.ServeMux
}

// NewServer returns a Server that requires token on POSTs.
func NewServer(h Handler, token string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{h: h, token: token, log: log, mux: http.NewServeMux()}
	get := func(f func(ctx context.Context) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			v, err := f(r.Context())
			s.reply(w, v, err)
		}
	}
	s.mux.HandleFunc("GET /v1/status", get(h.Status))
	s.mux.HandleFunc("GET /v1/pool", get(h.Pool))
	s.mux.HandleFunc("GET /v1/doctor", get(h.Doctor))
	s.mux.HandleFunc("GET /v1/relay/{id}", s.relay)
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
	s.mux.ServeHTTP(w, r)
}

func (s *Server) relay(w http.ResponseWriter, r *http.Request) {
	rl, ok := s.h.(Relayer)
	if !ok {
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
}

func (e *Error) Error() string { return e.Message }

// Client talks to the daemon.
type Client struct {
	Addr      string // default DefaultAddr
	TokenPath string // read for POSTs
	HTTP      *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: ClientTimeout}
}

func (c *Client) url(path string) string {
	addr := c.Addr
	if addr == "" {
		addr = DefaultAddr
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

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, ClientTimeout)
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
		tok, err := ReadToken(c.TokenPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return ErrNotRunning
			}
			return fmt.Errorf("read the control token (needs root or Administrator): %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
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
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return &Error{Status: resp.StatusCode, Message: e.Error}
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
