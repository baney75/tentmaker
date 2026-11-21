package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// MaxDescription is GitHub's limit on a commit status description.
const MaxDescription = 140

// PendingCommits lists open PR heads (FromFork set for forks, so the caller
// can refuse them) and the default branch head.
func (c *Client) PendingCommits(ctx context.Context, repo string) ([]core.Commit, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var r apiRepo
	if err := c.getJSON(ctx, rp, true, &r); err != nil {
		return nil, err
	}
	var out []core.Commit
	err = c.pages(ctx, rp+"/pulls?state=open", true, func(b []byte) (int, error) {
		var prs []struct {
			Number int `json:"number"`
			User   struct {
				Login string `json:"login"`
			} `json:"user"`
			Head struct {
				Ref  string      `json:"ref"`
				SHA  string      `json:"sha"`
				Repo *apiRepoRef `json:"repo"`
			} `json:"head"`
		}
		if err := json.Unmarshal(b, &prs); err != nil {
			return 0, err
		}
		for _, pr := range prs {
			fork := pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, repo)
			out = append(out, core.Commit{
				Repo: repo, SHA: pr.Head.SHA, Ref: pr.Head.Ref, PR: pr.Number,
				Author: pr.User.Login, FromFork: fork,
			})
		}
		return len(prs), nil
	})
	if err != nil {
		return nil, err
	}
	if r.DefaultBranch != "" {
		var br struct {
			Commit struct {
				SHA    string `json:"sha"`
				Author *struct {
					Login string `json:"login"`
				} `json:"author"`
			} `json:"commit"`
		}
		if err := c.getJSON(ctx, rp+"/branches/"+url.PathEscape(r.DefaultBranch), true, &br); err != nil {
			return nil, err
		}
		cm := core.Commit{Repo: repo, SHA: br.Commit.SHA, Ref: r.DefaultBranch}
		if br.Commit.Author != nil {
			cm.Author = br.Commit.Author.Login
		}
		out = append(out, cm)
	}
	return out, nil
}

func checkSHA(sha string) error {
	if sha == "" || len(sha) > 64 {
		return fmt.Errorf("github: bad commit sha %q", truncate(sha, 70))
	}
	for _, r := range sha {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return fmt.Errorf("github: bad commit sha %q", truncate(sha, 70))
		}
	}
	return nil
}

// FileAt returns a file's raw bytes at sha. A missing file matches
// core.ErrNotFound.
func (c *Client) FileAt(ctx context.Context, repo, sha, path string) ([]byte, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	if err := checkSHA(sha); err != nil {
		return nil, err
	}
	ep, err := escapePath(path)
	if err != nil || ep == "" {
		return nil, fmt.Errorf("github: bad file path %q", path)
	}
	res, err := c.do(ctx, request{
		method: http.MethodGet, path: rp + "/contents/" + ep + "?ref=" + sha,
		accept: "application/vnd.github.raw+json",
	})
	if err != nil {
		return nil, err
	}
	if res.status != http.StatusOK {
		return nil, &APIError{Method: http.MethodGet, Path: rp + "/contents/" + ep, Status: res.status}
	}
	return res.body, nil
}

// ListDir lists the names in a directory at sha.
func (c *Client) ListDir(ctx context.Context, repo, sha, path string) ([]string, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	if err := checkSHA(sha); err != nil {
		return nil, err
	}
	ep, err := escapePath(path)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := c.getJSON(ctx, rp+"/contents/"+ep+"?ref="+sha, false, &entries); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names, nil
}

// Archive returns a short-lived download URL for a tar.gz of repo at sha.
// GitHub answers the tarball endpoint with a redirect to codeload whose URL
// carries its own token, so header is empty and the caller's token never
// leaves this process. If no redirect comes back, it falls back to the API
// URL plus an Authorization header. Both results are secrets.
func (c *Client) Archive(ctx context.Context, repo, sha string) (string, string, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return "", "", err
	}
	if err := checkSHA(sha); err != nil {
		return "", "", err
	}
	p := rp + "/tarball/" + sha
	res, err := c.do(ctx, request{method: http.MethodGet, path: p, noRedirect: true})
	if err != nil {
		return "", "", err
	}
	if res.status >= 300 && res.status < 400 {
		loc, perr := url.Parse(res.header.Get("Location"))
		if perr == nil && loc.Scheme == "https" && loc.Host != "" {
			return loc.String(), "", nil
		}
		if perr == nil && loc.Scheme == "http" && strings.HasPrefix(c.base, "http://") {
			return loc.String(), "", nil // a test or a plain-HTTP enterprise server
		}
	}
	c.log.Debug("github tarball did not redirect; using the API URL with a token header", "repo", repo, "status", res.status)
	token, err := c.getToken(ctx)
	if err != nil {
		return "", "", err
	}
	return c.base + p, "Authorization: Bearer " + token, nil
}

// Statuses returns context -> state for sha, from the combined status.
func (c *Client) Statuses(ctx context.Context, repo, sha string) (map[string]string, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	if err := checkSHA(sha); err != nil {
		return nil, err
	}
	out := map[string]string{}
	err = c.pages(ctx, rp+"/commits/"+sha+"/status", true, func(b []byte) (int, error) {
		var page struct {
			Statuses []struct {
				Context string `json:"context"`
				State   string `json:"state"`
			} `json:"statuses"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, err
		}
		for _, s := range page.Statuses {
			out[s.Context] = s.State
		}
		return len(page.Statuses), nil
	})
	return out, err
}

// PostStatus sets a commit status. The description is cut to 140
// characters.
func (c *Client) PostStatus(ctx context.Context, repo, sha, context, state, description, targetURL string) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	if err := checkSHA(sha); err != nil {
		return err
	}
	switch state {
	case "pending", "success", "failure", "error":
	default:
		return fmt.Errorf("github: commit status state %q must be pending, success, failure or error", state)
	}
	body := map[string]any{"state": state, "context": context, "description": truncate(description, MaxDescription)}
	if targetURL != "" {
		body["target_url"] = targetURL
	}
	_, err = c.send(ctx, http.MethodPost, rp+"/statuses/"+sha, body, http.StatusCreated)
	c.forget(rp + "/commits/" + sha + "/status")
	return err
}

// TokenFromGH asks the GitHub CLI for its token (`gh auth token`). The
// token is returned only; it is never logged or put in an error.
func TokenFromGH(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	path, err := exec.LookPath("gh")
	if err != nil {
		return "", errors.New("the GitHub CLI (gh) is not installed or not on PATH")
	}
	cmd := exec.CommandContext(ctx, path, "auth", "token")
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("`gh auth token` failed; run `gh auth login` for this user")
	}
	t := strings.TrimSpace(string(out))
	if t == "" || strings.ContainsAny(t, " \n\t") {
		return "", errors.New("`gh auth token` printed no usable token; run `gh auth login`")
	}
	return t, nil
}
