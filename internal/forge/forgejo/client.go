// Package forgejo is Tentmaker's forge for Forgejo and Gitea Actions.
//
// Both forges serve checks mode fully (core.StatusForge): open pull
// requests, files and directory listings at a commit, a tar.gz archive URL,
// and commit statuses.
//
// Runner mode (core.Forge) needs a single-use runner registration, which
// only Forgejo has:
//
//   - Forgejo 15 and later: Grant registers an ephemeral runner with
//     POST /repos/{o}/{r}/actions/runners {"ephemeral": true}. The secret is
//     the forgejo-runner registration file (.runner) as JSON: id, uuid,
//     name, token, address and labels (each label gets the ":host" scheme,
//     since the job already runs inside Tentmaker's sandbox). The runner
//     takes one job, then the forge retires it.
//   - Gitea has no such API: its only registration path is a repo-wide
//     registration token that stays valid for any number of runners, which
//     breaks Tentmaker's single-use grant rule. Grant returns ErrUnsupported
//     on Gitea. Gitea repos should use mode = "checks".
//
// Other gaps, each reported as ErrUnsupported or in CheckRepo's Problems:
// QueuedJobs needs Forgejo 12 (run details) or Gitea 1.25 (jobs list);
// JobByID needs Forgejo 17 for jobs that are no longer waiting or running;
// CancelRun needs Forgejo 16 or a Gitea release after 1.26. Neither forge
// says which job a runner holds, and Forgejo has no busy flag: Runner
// reports Busy when Forgejo calls the runner "active" (it reported work in
// the last few seconds), with JobID 0.
//
// Neither forge has an API for the fork pull request approval setting. Both
// require approval for runs from a fork pull request whose author can't
// write to the repo and hasn't been trusted or approved before, so
// CheckRepo reports "approve_first_time".
//
// The token is sent only as "Authorization: token ..." to BaseURL, and is
// never logged or put in an error.
package forgejo

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
	// HTTPTimeout bounds every request.
	HTTPTimeout = 30 * time.Second
	// Tries is how many times a request is sent before giving up.
	Tries = 3
	// InlineWait is the longest 429 wait a call sleeps through itself; a
	// longer one fails the call at once with core.ErrRateLimited, so a
	// rate limit never holds the broker's tick until the reset.
	InlineWait = 5 * time.Second
	// RunMaxAge: runs older than this are ignored.
	RunMaxAge = 24 * time.Hour
	// MaxDescription caps a commit status description.
	MaxDescription = 140
	// GrantTTL is how long Tentmaker treats a runner registration as usable.
	GrantTTL = time.Hour

	pageSize = 50
	maxPages = 20
	maxBody  = 32 << 20
)

// ErrUnsupported is returned (wrapped) when the forge has no API for a
// core.Forge method. errors.Is(err, errors.ErrUnsupported) also matches.
var ErrUnsupported = fmt.Errorf("not supported by this forge or version; use mode = \"checks\" for the repo: %w", errors.ErrUnsupported)

// Flavors.
const (
	Forgejo = "forgejo"
	Gitea   = "gitea"
)

// Options configures a Client.
type Options struct {
	// BaseURL is the instance, like https://codeberg.org. "/api/v1" is
	// added when missing.
	BaseURL string
	Token   func(ctx context.Context) (string, error)
	HTTP    *http.Client // default: 30 s timeout
	Now     func() time.Time
	Log     *slog.Logger
	// Flavor is "forgejo", "gitea", or empty to ask the server
	// (GET /api/v1/version: Forgejo reports "...+gitea-...").
	Flavor string
}

// Client talks to a Forgejo or Gitea instance. It implements core.Forge
// and core.StatusForge, and is safe for concurrent use.
type Client struct {
	api      string // .../api/v1
	instance string // the instance root, for runner registrations
	tokenFn  func(ctx context.Context) (string, error)
	http     *http.Client
	now      func() time.Time
	log      *slog.Logger
	sleep    func(ctx context.Context, d time.Duration) error
	backoff  func(try int) time.Duration
	mu       sync.Mutex
	token    string
	flavor   string
	runs     map[string]runInfo // "repo#id" -> run details
}

var (
	_ core.Forge       = (*Client)(nil)
	_ core.StatusForge = (*Client)(nil)
)

// New returns a Client.
func New(o Options) *Client {
	base := strings.TrimRight(o.BaseURL, "/")
	instance := strings.TrimSuffix(base, "/api/v1")
	c := &Client{
		api:      instance + "/api/v1",
		instance: instance,
		tokenFn:  o.Token,
		http:     o.HTTP,
		now:      o.Now,
		log:      o.Log,
		flavor:   o.Flavor,
		runs:     map[string]runInfo{},
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
		c.tokenFn = func(context.Context) (string, error) { return "", errors.New("no token source configured") }
	}
	c.sleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	c.backoff = func(try int) time.Duration {
		d := time.Second << try
		return d/2 + rand.N(d)
	}
	return c
}

// Name is "forgejo" (the config's forge kind, for Gitea too).
func (c *Client) Name() string { return "forgejo" }

// APIError is a non-success HTTP status, without the token.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("forgejo: %s %s: HTTP %d", e.Method, e.Path, e.Status)
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

func (c *Client) getToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	t := c.token
	c.mu.Unlock()
	if t != "" {
		return t, nil
	}
	t, err := c.tokenFn(ctx)
	if err != nil {
		return "", fmt.Errorf("forgejo: no token: %w", err)
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return "", errors.New("forgejo: no token: the token source returned nothing")
	}
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
	return t, nil
}

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

type response struct {
	status int
	body   []byte
}

// do sends a request with retries: 3 tries with jittered backoff on 5xx
// and network errors, a wait of at most InlineWait on 429; other 4xx are
// final. A 429 that asks for longer, or is still there after every try,
// is a *core.RateLimitError.
func (c *Client) do(ctx context.Context, method, path string, body any, accept string) (response, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return response{}, err
		}
		payload = b
	}
	var lastErr error
	for try := 0; try < Tries; try++ {
		res, wait, err := c.once(ctx, method, path, payload, accept)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if statusOf(err) == http.StatusTooManyRequests && (wait > InlineWait || try == Tries-1) {
			return response{}, &core.RateLimitError{Forge: "forgejo", Until: c.now().Add(wait), Err: err}
		}
		if ctx.Err() != nil || wait < 0 || try == Tries-1 {
			break
		}
		if wait == 0 {
			wait = c.backoff(try)
		}
		if err := c.sleep(ctx, wait); err != nil {
			break
		}
	}
	return response{}, lastErr
}

func (c *Client) once(ctx context.Context, method, path string, payload []byte, accept string) (response, time.Duration, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return response{}, -1, err
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api+path, rd)
	if err != nil {
		return response{}, -1, fmt.Errorf("forgejo: %s %s: %s", method, cleanPath(path), c.redact(err.Error()))
	}
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "tentmaker")
	req.Header.Set("Authorization", "token "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := httpsafe.Guard(c.http)
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
		return response{}, 0, fmt.Errorf("forgejo: %s %s: %s", method, cleanPath(path), c.redact(msg))
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return response{}, 0, fmt.Errorf("forgejo: %s %s: read body: %s", method, cleanPath(path), c.redact(err.Error()))
	}
	c.log.Debug("forgejo", "method", method, "path", cleanPath(path), "status", res.StatusCode)
	if res.StatusCode < 400 {
		return response{status: res.StatusCode, body: data}, 0, nil
	}
	apiErr := &APIError{Method: method, Path: cleanPath(path), Status: res.StatusCode, Message: c.redact(message(data))}
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
		return response{}, -1, apiErr
	case res.StatusCode == http.StatusTooManyRequests:
		wait := time.Minute
		if n, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil {
			wait = time.Duration(n) * time.Second
		}
		return response{}, wait, apiErr
	case res.StatusCode >= 500:
		return response{}, 0, apiErr
	}
	return response{}, -1, apiErr
}

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

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	res, err := c.do(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(res.body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(res.body, v); err != nil {
		return fmt.Errorf("forgejo: GET %s: bad JSON: %v", cleanPath(path), err)
	}
	return nil
}

// send makes a write call; ok lists the statuses that count as success,
// including 4xx ones such as 404 for a delete.
func (c *Client) send(ctx context.Context, method, path string, body any, ok ...int) (response, error) {
	res, err := c.do(ctx, method, path, body, "")
	for _, s := range ok {
		if (err == nil && res.status == s) || (err != nil && statusOf(err) == s) {
			return res, nil
		}
	}
	if err != nil {
		return res, err
	}
	return res, &APIError{Method: method, Path: cleanPath(path), Status: res.status}
}

// pages GETs path page by page (page and limit appended); each returns how
// many items its page had. A list still full after maxPages is an error.
func (c *Client) pages(ctx context.Context, path string, each func(body []byte) (int, error)) error {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= maxPages; page++ {
		res, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s%slimit=%d&page=%d", path, sep, pageSize, page), nil, "")
		if err != nil {
			return err
		}
		n, err := each(res.body)
		if err != nil {
			return fmt.Errorf("forgejo: GET %s: bad JSON: %v", cleanPath(path), err)
		}
		if n < pageSize {
			return nil
		}
	}
	return fmt.Errorf("forgejo: GET %s: more than %d items; the list is truncated", cleanPath(path), maxPages*pageSize)
}

// Flavor returns "forgejo" or "gitea", asking the server once when Options
// didn't say.
func (c *Client) Flavor(ctx context.Context) (string, error) {
	c.mu.Lock()
	f := c.flavor
	c.mu.Unlock()
	if f == Forgejo || f == Gitea {
		return f, nil
	}
	if f != "" {
		return "", fmt.Errorf("forgejo: flavor %q must be forgejo or gitea", f)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := c.getJSON(ctx, "/version", &v); err != nil {
		return "", fmt.Errorf("forgejo: can't tell Forgejo from Gitea: %w", err)
	}
	f = Gitea
	if strings.Contains(v.Version, "+gitea-") {
		f = Forgejo
	}
	c.mu.Lock()
	c.flavor = f
	c.mu.Unlock()
	return f, nil
}

func repoPath(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !safeSeg(owner) || !safeSeg(name) {
		return "", fmt.Errorf("forgejo: repo %q must be owner/name", repo)
	}
	return "/repos/" + owner + "/" + name, nil
}

func safeSeg(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func escapePath(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == "" || s == "." || s == ".." {
			return "", fmt.Errorf("forgejo: bad path %q", p)
		}
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/"), nil
}

func checkSHA(sha string) error {
	if sha == "" || len(sha) > 64 {
		return fmt.Errorf("forgejo: bad commit sha %q", truncate(sha, 70))
	}
	for _, r := range sha {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return fmt.Errorf("forgejo: bad commit sha %q", truncate(sha, 70))
		}
	}
	return nil
}
