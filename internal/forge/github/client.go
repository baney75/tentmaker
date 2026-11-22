// Package github is Tentmaker's GitHub Actions forge: it finds queued jobs,
// grants single-use JIT runners, watches and deletes them, reads and sets a
// repo's fork policy, serves checks mode (commit statuses, files, archives),
// and migrates workflows to the pool.
//
// The client is careful with the rate limit: list calls are ETag-conditional
// GETs (a 304 costs nothing and reuses the cached body), jobs of an
// in-progress run are refetched at most every 15 s unless the run changed,
// and below 200 remaining requests it waits for the reset.
//
// The token never leaves this process except in the Authorization header of
// a request to BaseURL. It is never logged and never part of an error.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/httpsafe"
)

const (
	// DefaultBaseURL is the public GitHub API.
	DefaultBaseURL = "https://api.github.com"
	// HTTPTimeout bounds every request.
	HTTPTimeout = 30 * time.Second
	// RunMaxAge: runs older than this are ignored.
	RunMaxAge = 24 * time.Hour
	// InProgressRecheck: jobs of an in-progress run are refetched at most
	// this often unless the run's updated_at changed.
	InProgressRecheck = 15 * time.Second
	// RateFloor: below this many remaining requests, wait for the reset.
	RateFloor = 200
	// Tries is how many times a request is sent before giving up.
	Tries = 3
	// GrantTTL is how long Tentmaker treats a JIT config as usable.
	GrantTTL = time.Hour
	// RunnerGroupID is the default runner group.
	RunnerGroupID = 1

	maxPages  = 10
	perPage   = 100
	maxBody   = 32 << 20
	apiVer    = "2022-11-28"
	userAgent = "tentmaker"
)

// Options configures a Client.
type Options struct {
	BaseURL string                                    // default https://api.github.com
	Token   func(ctx context.Context) (string, error) // asked once, again after a 401
	HTTP    *http.Client                              // default: 30 s timeout
	Now     func() time.Time
	Log     *slog.Logger
}

// Client talks to the GitHub REST API. It implements core.Forge and
// core.StatusForge. It is safe for concurrent use.
type Client struct {
	base     string
	tokenFn  func(ctx context.Context) (string, error)
	http     *http.Client
	now      func() time.Time
	log      *slog.Logger
	sleep    func(ctx context.Context, d time.Duration) error // a seam for tests
	backoff  func(try int) time.Duration                      // a seam for tests
	mu       sync.Mutex
	token    string
	etags    map[string]etagEntry
	runJobs  map[int64]runJobsEntry
	rateLeft int // -1 = unknown
	rateSet  time.Time
}

type etagEntry struct {
	etag string
	body []byte
}

type runJobsEntry struct {
	repo      string
	fetched   time.Time
	updatedAt string
	jobs      []apiJob
}

var (
	_ core.Forge       = (*Client)(nil)
	_ core.StatusForge = (*Client)(nil)
)

// New returns a Client.
func New(o Options) *Client {
	c := &Client{
		base:     strings.TrimRight(o.BaseURL, "/"),
		tokenFn:  o.Token,
		http:     o.HTTP,
		now:      o.Now,
		log:      o.Log,
		etags:    map[string]etagEntry{},
		runJobs:  map[int64]runJobsEntry{},
		rateLeft: -1,
	}
	if c.base == "" {
		c.base = DefaultBaseURL
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: HTTPTimeout}
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.tokenFn == nil {
		c.tokenFn = TokenFromGH
	}
	c.sleep = sleepCtx
	c.backoff = func(try int) time.Duration {
		d := time.Second << try // 1 s, 2 s, 4 s
		return d/2 + rand.N(d)  // jittered: 0.5x to 1.5x
	}
	return c
}

// Name is "github".
func (c *Client) Name() string { return "github" }

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// APIError is a non-success HTTP status. It carries the method, the path
// without its query, the status and GitHub's message; never the token.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("github: %s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Is lets errors.Is(err, core.ErrNotFound) match a 404.
func (e *APIError) Is(target error) bool {
	return target == core.ErrNotFound && e.Status == http.StatusNotFound
}

func statusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

type response struct {
	status int
	header http.Header
	body   []byte
	cached bool // served from the ETag cache after a 304
}

type request struct {
	method      string
	path        string // starts with "/", may carry a query
	body        any
	conditional bool
	accept      string
	noRedirect  bool
}

func (c *Client) getToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	t := c.token
	c.mu.Unlock()
	if t != "" {
		return t, nil
	}
	t, err := c.tokenFn(ctx)
	if err != nil {
		return "", fmt.Errorf("github: no token: %w", err)
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return "", errors.New("github: no token: the token source returned nothing")
	}
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
	return t, nil
}

// redact removes the token from s, in case a lower layer echoed it.
func (c *Client) redact(s string) string {
	c.mu.Lock()
	t := c.token
	c.mu.Unlock()
	if t != "" {
		s = strings.ReplaceAll(s, t, "[redacted]")
	}
	return s
}

func cleanPath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// waitForRate sleeps until the reset while fewer than RateFloor requests
// remain.
func (c *Client) waitForRate(ctx context.Context) error {
	c.mu.Lock()
	left, reset := c.rateLeft, c.rateSet
	c.mu.Unlock()
	if left < 0 || left >= RateFloor {
		return nil
	}
	wait := reset.Sub(c.now())
	if wait <= 0 {
		return nil
	}
	c.log.Info("github rate limit low; waiting for the reset", "remaining", left, "wait", wait.Round(time.Second).String())
	if err := c.sleep(ctx, wait); err != nil {
		return fmt.Errorf("github: waiting for the rate limit reset: %w", err)
	}
	c.mu.Lock()
	if c.rateSet.Equal(reset) {
		c.rateLeft = -1
	}
	c.mu.Unlock()
	return nil
}

func (c *Client) noteRate(h http.Header) {
	left := h.Get("X-RateLimit-Remaining")
	if left == "" {
		return
	}
	n, err := strconv.Atoi(left)
	if err != nil {
		return
	}
	reset, _ := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	c.mu.Lock()
	c.rateLeft = n
	c.rateSet = time.Unix(reset, 0)
	c.mu.Unlock()
}

// rateLimited reports whether a 403/429 is a rate limit, and how long to wait.
func (c *Client) rateLimited(status int, h http.Header, body []byte) (time.Duration, bool) {
	if status != http.StatusTooManyRequests && status != http.StatusForbidden {
		return 0, false
	}
	if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return time.Duration(n) * time.Second, true
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		reset, _ := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
		if d := time.Unix(reset, 0).Sub(c.now()); d > 0 {
			return d, true
		}
		return time.Second, true
	}
	if status == http.StatusTooManyRequests || bytes.Contains(bytes.ToLower(body), []byte("rate limit")) {
		return time.Minute, true
	}
	return 0, false
}

func (c *Client) do(ctx context.Context, r request) (response, error) {
	var payload []byte
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return response{}, err
		}
		payload = b
	}
	var lastErr error
	for try := 0; try < Tries; try++ {
		if try > 0 {
			c.log.Debug("github retry", "method", r.method, "path", cleanPath(r.path), "try", try+1)
		}
		if err := c.waitForRate(ctx); err != nil {
			return response{}, err
		}
		resp, wait, err := c.once(ctx, r, payload)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return response{}, lastErr
		}
		if wait < 0 || try == Tries-1 {
			break
		}
		if wait == 0 {
			wait = c.backoff(try)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return response{}, lastErr
		}
	}
	return response{}, lastErr
}

// once sends one request. On failure it returns how long to wait before
// trying again (0: the usual jittered backoff), or -1 when the error is
// final.
func (c *Client) once(ctx context.Context, r request, payload []byte) (response, time.Duration, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return response{}, -1, err
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, c.base+r.path, body)
	if err != nil {
		return response{}, -1, fmt.Errorf("github: %s %s: %s", r.method, cleanPath(r.path), c.redact(err.Error()))
	}
	accept := r.accept
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVer)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	var cached etagEntry
	if r.conditional {
		c.mu.Lock()
		cached = c.etags[r.path]
		c.mu.Unlock()
		if cached.etag != "" {
			req.Header.Set("If-None-Match", cached.etag)
		}
	}
	hc := httpsafe.Guard(c.http)
	if r.noRedirect {
		cp := *hc
		cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		hc = &cp
	}
	if hc.Timeout == 0 {
		cp := *hc
		cp.Timeout = HTTPTimeout
		hc = &cp
	}
	res, err := hc.Do(req)
	if err != nil {
		msg := err.Error()
		var ue *url.Error
		if errors.As(err, &ue) {
			msg = ue.Err.Error()
		}
		return response{}, 0, fmt.Errorf("github: %s %s: %s", r.method, cleanPath(r.path), c.redact(msg))
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return response{}, 0, fmt.Errorf("github: %s %s: read body: %s", r.method, cleanPath(r.path), c.redact(err.Error()))
	}
	c.noteRate(res.Header)
	c.log.Debug("github", "method", r.method, "path", cleanPath(r.path), "status", res.StatusCode)
	out := response{status: res.StatusCode, header: res.Header, body: data}
	switch {
	case res.StatusCode == http.StatusNotModified && cached.etag != "":
		out.status, out.body, out.cached = http.StatusOK, cached.body, true
		return out, 0, nil
	case res.StatusCode < 400:
		if r.conditional && res.StatusCode == http.StatusOK {
			if et := res.Header.Get("ETag"); et != "" {
				c.mu.Lock()
				c.etags[r.path] = etagEntry{etag: et, body: data}
				c.mu.Unlock()
			}
		}
		return out, 0, nil
	}
	apiErr := &APIError{Method: r.method, Path: cleanPath(r.path), Status: res.StatusCode, Message: c.redact(message(data))}
	if res.StatusCode == http.StatusUnauthorized {
		c.mu.Lock()
		c.token = "" // the source may have a fresh one next time
		c.mu.Unlock()
		return out, -1, apiErr
	}
	if wait, ok := c.rateLimited(res.StatusCode, res.Header, data); ok {
		c.log.Info("github rate limited", "method", r.method, "path", cleanPath(r.path), "wait", wait.Round(time.Second).String())
		return out, wait, apiErr
	}
	if res.StatusCode >= 500 {
		return out, 0, apiErr
	}
	return out, -1, apiErr
}

// message pulls GitHub's error message out of a body, kept short.
func message(b []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		return truncate(m.Message, 200)
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func (c *Client) getJSON(ctx context.Context, path string, conditional bool, v any) error {
	res, err := c.do(ctx, request{method: http.MethodGet, path: path, conditional: conditional})
	if err != nil {
		return err
	}
	if res.status != http.StatusOK {
		return &APIError{Method: http.MethodGet, Path: cleanPath(path), Status: res.status}
	}
	if err := json.Unmarshal(res.body, v); err != nil {
		return fmt.Errorf("github: GET %s: bad JSON: %v", cleanPath(path), err)
	}
	return nil
}

func (c *Client) send(ctx context.Context, method, path string, body any, ok ...int) (response, error) {
	res, err := c.do(ctx, request{method: method, path: path, body: body})
	for _, s := range ok {
		if res.status == s || (err != nil && statusOf(err) == s) {
			return res, nil
		}
	}
	if err != nil {
		return res, err
	}
	return res, &APIError{Method: method, Path: cleanPath(path), Status: res.status, Message: c.redact(message(res.body))}
}

// pages GETs path (which must not carry page or per_page) one page at a
// time, calling each with the page body; each returns how many items the
// page had. It stops at a short page and fails when maxPages pages are all
// full, so a truncated list is never mistaken for a complete one.
func (c *Client) pages(ctx context.Context, path string, conditional bool, each func(body []byte) (int, error)) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= maxPages; page++ {
		p := fmt.Sprintf("%s%sper_page=%d&page=%d", path, sep, perPage, page)
		res, err := c.do(ctx, request{method: http.MethodGet, path: p, conditional: conditional})
		if err != nil {
			return err
		}
		if res.status != http.StatusOK {
			return &APIError{Method: http.MethodGet, Path: cleanPath(path), Status: res.status}
		}
		n, err := each(res.body)
		if err != nil {
			return fmt.Errorf("github: GET %s: bad JSON: %v", cleanPath(path), err)
		}
		if n < perPage {
			return nil
		}
	}
	return fmt.Errorf("github: GET %s: more than %d items; the list is truncated", cleanPath(path), maxPages*perPage)
}

// forgetExact drops the cached body of one path.
func (c *Client) forgetExact(path string) {
	c.mu.Lock()
	delete(c.etags, path)
	c.mu.Unlock()
}

// forget drops cached bodies whose path starts with prefix.
func (c *Client) forget(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.etags {
		if strings.HasPrefix(k, prefix) {
			delete(c.etags, k)
		}
	}
}

// repoPath validates "owner/name" and returns "/repos/owner/name".
func repoPath(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || !safeSeg(owner) || !safeSeg(name) {
		return "", fmt.Errorf("github: repo %q must be owner/name", repo)
	}
	return "/repos/" + owner + "/" + name, nil
}

func safeSeg(s string) bool {
	if s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// escapePath escapes each segment of a repo file path.
func escapePath(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("github: bad path %q", p)
		}
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/"), nil
}
