// Package gitlab is GitLab's side of checks mode: it lists commits to test,
// serves workflow files and source archives, and posts commit statuses
// through the GitLab REST API (v4). It implements core.StatusForge only;
// Tentmaker doesn't register GitLab runners.
package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/httpsafe"
)

const (
	defaultBaseURL = "https://gitlab.com/api/v4"
	maxBody        = 16 << 20
	maxPages       = 100
	maxDescRunes   = 255
	maxRetryWait   = 60 * time.Second
	tries          = 3
)

// Options configure a Client.
type Options struct {
	BaseURL string                                    // default https://gitlab.com/api/v4
	Token   func(ctx context.Context) (string, error) // a personal, project or group access token; nil for public projects
	HTTP    *http.Client                              // default: 30 s timeout
	Log     *slog.Logger
}

// Client talks to one GitLab instance. Repo names are full project paths
// (group/sub/name).
type Client struct {
	base  string
	token func(ctx context.Context) (string, error)
	http  *http.Client
	log   *slog.Logger
	sleep func(ctx context.Context, d time.Duration) error
}

var _ core.StatusForge = (*Client)(nil)

// New returns a Client.
func New(o Options) *Client {
	c := &Client{
		base:  strings.TrimRight(o.BaseURL, "/"),
		token: o.Token,
		http:  o.HTTP,
		log:   o.Log,
		sleep: sleepCtx,
	}
	if c.base == "" {
		c.base = defaultBaseURL
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	return c
}

// Name is "gitlab".
func (c *Client) Name() string { return "gitlab" }

func project(repo string) string { return "projects/" + url.PathEscape(repo) }

// PendingCommits lists the default branch head, the heads of branches and
// the heads of open merge requests. A merge request whose source project
// differs from the target is marked FromFork. A missing branch is skipped.
func (c *Client) PendingCommits(ctx context.Context, repo string, branches ...string) ([]core.Commit, error) {
	var proj struct {
		ID            int64  `json:"id"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.getJSON(ctx, project(repo), nil, &proj); err != nil {
		return nil, err
	}
	var out []core.Commit
	seen := map[string]bool{}
	for _, name := range append([]string{proj.DefaultBranch}, branches...) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		var br struct {
			Commit struct {
				ID string `json:"id"`
			} `json:"commit"`
		}
		err := c.getJSON(ctx, project(repo)+"/repository/branches/"+url.PathEscape(name), nil, &br)
		switch {
		case errors.Is(err, core.ErrNotFound):
			// An empty repository has no default branch head yet, and a
			// listed branch may be gone.
		case err != nil:
			return nil, err
		case br.Commit.ID != "":
			out = append(out, core.Commit{Repo: repo, SHA: br.Commit.ID, Ref: name})
		}
	}
	type mr struct {
		IID             int    `json:"iid"`
		SHA             string `json:"sha"`
		SourceBranch    string `json:"source_branch"`
		SourceProjectID int64  `json:"source_project_id"`
		TargetProjectID int64  `json:"target_project_id"`
		Author          struct {
			Username string `json:"username"`
		} `json:"author"`
	}
	mrs, err := getAll[mr](ctx, c, project(repo)+"/merge_requests", url.Values{"state": {"opened"}})
	if err != nil {
		return nil, err
	}
	for _, m := range mrs {
		if m.SHA == "" {
			continue
		}
		out = append(out, core.Commit{
			Repo:     repo,
			SHA:      m.SHA,
			Ref:      m.SourceBranch,
			PR:       m.IID,
			Author:   m.Author.Username,
			FromFork: m.SourceProjectID != proj.ID,
		})
	}
	return out, nil
}

// FileAt reads one file at sha. A missing file is core.ErrNotFound.
func (c *Client) FileAt(ctx context.Context, repo, sha, path string) ([]byte, error) {
	p := project(repo) + "/repository/files/" + url.PathEscape(strings.TrimPrefix(path, "/")) + "/raw"
	resp, err := c.do(ctx, http.MethodGet, p, url.Values{"ref": {sha}}, nil)
	if err != nil {
		return nil, err
	}
	return resp.body, nil
}

// ListDir lists the names of the entries in a folder at sha, sorted. A
// missing folder is core.ErrNotFound.
func (c *Client) ListDir(ctx context.Context, repo, sha, path string) ([]string, error) {
	type entry struct {
		Name string `json:"name"`
	}
	q := url.Values{"ref": {sha}}
	if p := strings.Trim(path, "/"); p != "" {
		q.Set("path", p)
	}
	entries, err := getAll[entry](ctx, c, project(repo)+"/repository/tree", q)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names, nil
}

// Archive returns the tar.gz URL for sha and the PRIVATE-TOKEN header to
// fetch it with. The header is a secret.
func (c *Client) Archive(ctx context.Context, repo, sha string) (string, string, error) {
	u := c.base + "/" + project(repo) + "/repository/archive.tar.gz?" + url.Values{"sha": {sha}}.Encode()
	if c.token == nil {
		return u, "", nil
	}
	tok, err := c.token(ctx)
	if err != nil {
		return "", "", errors.New("gitlab: can't read the token")
	}
	return u, "PRIVATE-TOKEN: " + tok, nil
}

// Statuses returns the latest state of each status name on sha, in
// Tentmaker's words: success, failure, error or pending.
func (c *Client) Statuses(ctx context.Context, repo, sha string) (map[string]string, error) {
	type status struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	list, err := getAll[status](ctx, c, project(repo)+"/repository/commits/"+url.PathEscape(sha)+"/statuses", url.Values{})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, s := range list {
		out[s.Name] = fromGitLabState(s.Status)
	}
	return out, nil
}

// PostStatus sets the status named context on sha. Tentmaker's pending
// means a device is running the job, so it posts GitLab's running.
func (c *Client) PostStatus(ctx context.Context, repo, sha, context, state, description, targetURL string) error {
	glState, err := toGitLabState(state)
	if err != nil {
		return err
	}
	form := url.Values{
		"state": {glState},
		"name":  {context},
	}
	if description != "" {
		form.Set("description", truncRunes(description, maxDescRunes))
	}
	if targetURL != "" && len(targetURL) <= 255 {
		form.Set("target_url", targetURL)
	}
	_, err = c.do(ctx, http.MethodPost, project(repo)+"/statuses/"+url.PathEscape(sha), nil, form)
	var he *httpError
	if errors.As(err, &he) && he.status == http.StatusBadRequest && strings.Contains(he.message, "Cannot transition status") {
		// The status is already in this state (running twice, say).
		return nil
	}
	return err
}

func toGitLabState(s string) (string, error) {
	switch s {
	case "success":
		return "success", nil
	case "failure", "error":
		return "failed", nil
	case "pending":
		return "running", nil
	}
	return "", fmt.Errorf("gitlab: unknown status state %q", s)
}

func fromGitLabState(s string) string {
	switch s {
	case "success":
		return "success"
	case "failed":
		return "failure"
	case "canceled", "canceling", "skipped":
		return "error"
	case "running", "pending", "created", "preparing", "scheduled", "waiting_for_resource", "waiting_for_callback", "manual":
		return "pending"
	}
	return s
}

type response struct {
	status int
	header http.Header
	body   []byte
}

type httpError struct {
	method, path string
	status       int
	message      string
}

func (e *httpError) Error() string {
	msg := fmt.Sprintf("gitlab: %s %s: HTTP %d", e.method, e.path, e.status)
	if e.message != "" {
		msg += ": " + e.message
	}
	return msg
}

func (e *httpError) Unwrap() error {
	if e.status == http.StatusNotFound {
		return core.ErrNotFound
	}
	return nil
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	resp, err := c.do(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(resp.body, v); err != nil {
		return fmt.Errorf("gitlab: GET %s: unreadable answer: %v", path, err)
	}
	return nil
}

// getAll follows GitLab's page-based pagination.
func getAll[T any](ctx context.Context, c *Client, path string, q url.Values) ([]T, error) {
	if q == nil {
		q = url.Values{}
	}
	q.Set("per_page", "100")
	page := "1"
	var out []T
	for i := 0; i < maxPages; i++ {
		q.Set("page", page)
		resp, err := c.do(ctx, http.MethodGet, path, q, nil)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(resp.body, &items); err != nil {
			return nil, fmt.Errorf("gitlab: GET %s: unreadable answer: %v", path, err)
		}
		out = append(out, items...)
		next := resp.header.Get("X-Next-Page")
		if next == "" {
			next = nextFromLink(resp.header.Get("Link"))
		}
		if next == "" || len(items) == 0 {
			return out, nil
		}
		if _, err := strconv.Atoi(next); err != nil {
			return nil, fmt.Errorf("gitlab: GET %s: bad next page %q", path, next)
		}
		page = next
	}
	return nil, fmt.Errorf("gitlab: GET %s: more than %d pages", path, maxPages)
}

var linkNext = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextFromLink(h string) string {
	m := linkNext.FindStringSubmatch(h)
	if m == nil {
		return ""
	}
	u, err := url.Parse(m[1])
	if err != nil {
		return ""
	}
	return u.Query().Get("page")
}

// do sends one API request, retrying network errors, 5xx, 409 (another
// status update in progress) and 429 up to 3 tries. Errors never carry the
// token.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, form url.Values) (*response, error) {
	u := c.base + "/" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var lastErr error
	for attempt := 0; attempt < tries; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt)
			if he, ok := lastErr.(*retryAfter); ok && he.wait > 0 {
				wait = he.wait
			}
			c.log.Debug("gitlab: retrying", "method", method, "path", path, "attempt", attempt+1, "wait", wait)
			if err := c.sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
		resp, retry, err := c.once(ctx, method, u, path, form)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	if ra, ok := lastErr.(*retryAfter); ok {
		return nil, ra.err
	}
	return nil, lastErr
}

type retryAfter struct {
	err  error
	wait time.Duration
}

func (r *retryAfter) Error() string { return r.err.Error() }
func (r *retryAfter) Unwrap() error { return r.err }

func (c *Client) once(ctx context.Context, method, u, path string, form url.Values) (resp *response, retry bool, err error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, false, fmt.Errorf("gitlab: %s %s: bad request", method, path)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tentmaker")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	tok := ""
	if c.token != nil {
		if tok, err = c.token(ctx); err != nil {
			return nil, false, errors.New("gitlab: can't read the token")
		}
		if tok != "" {
			req.Header.Set("PRIVATE-TOKEN", tok)
		}
	}
	res, err := httpsafe.Guard(c.http).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, true, fmt.Errorf("gitlab: %s %s: %s", method, path, scrub(err.Error(), tok))
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return nil, true, fmt.Errorf("gitlab: %s %s: reading the answer: %s", method, path, scrub(err.Error(), tok))
	}
	if len(data) > maxBody {
		return nil, false, fmt.Errorf("gitlab: %s %s: the answer is over 16 MiB", method, path)
	}
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return &response{status: res.StatusCode, header: res.Header, body: data}, false, nil
	}
	he := &httpError{method: method, path: path, status: res.StatusCode, message: scrub(apiMessage(data), tok)}
	switch {
	case res.StatusCode == http.StatusTooManyRequests:
		return nil, true, &retryAfter{err: he, wait: retryWait(res.Header)}
	case res.StatusCode >= 500 || res.StatusCode == http.StatusConflict:
		return nil, true, he
	}
	return nil, false, he
}

// apiMessage pulls GitLab's {"message": ...} or {"error": ...} out of an
// error body.
func apiMessage(data []byte) string {
	var m struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	msg := ""
	if json.Unmarshal(data, &m) == nil {
		switch v := m.Message.(type) {
		case string:
			msg = v
		case nil:
			msg = m.Error
		default:
			b, _ := json.Marshal(v)
			msg = string(b)
		}
	}
	return truncRunes(strings.TrimSpace(msg), 200)
}

func retryWait(h http.Header) time.Duration {
	if s := h.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			return min(time.Duration(n)*time.Second, maxRetryWait)
		}
	}
	if s := h.Get("RateLimit-Reset"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			if d := time.Until(time.Unix(n, 0)); d > 0 {
				return min(d, maxRetryWait)
			}
		}
	}
	return backoff(1)
}

func backoff(attempt int) time.Duration {
	base := time.Duration(1<<attempt) * 500 * time.Millisecond
	return base + time.Duration(rand.Int64N(int64(base/2)+1))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func scrub(s, tok string) string {
	if tok != "" {
		s = strings.ReplaceAll(s, tok, "[token]")
	}
	return s
}

func truncRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
