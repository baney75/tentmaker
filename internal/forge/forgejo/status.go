package forgejo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/baney75/tentmaker/internal/core"
)

// PendingCommits lists open PR heads (FromFork set for forks) and the
// default branch head.
func (c *Client) PendingCommits(ctx context.Context, repo string) ([]core.Commit, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var r apiRepo
	if err := c.getJSON(ctx, rp, &r); err != nil {
		return nil, err
	}
	var out []core.Commit
	err = c.pages(ctx, rp+"/pulls?state=open", func(b []byte) (int, error) {
		var prs []struct {
			Number int `json:"number"`
			User   *struct {
				Login string `json:"login"`
			} `json:"user"`
			Head struct {
				Ref  string     `json:"ref"`
				SHA  string     `json:"sha"`
				Repo *gtRepoRef `json:"repo"`
			} `json:"head"`
		}
		if err := json.Unmarshal(b, &prs); err != nil {
			return 0, err
		}
		for _, pr := range prs {
			cm := core.Commit{Repo: repo, SHA: pr.Head.SHA, Ref: pr.Head.Ref, PR: pr.Number,
				FromFork: pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, repo)}
			if pr.User != nil {
				cm.Author = pr.User.Login
			}
			out = append(out, cm)
		}
		return len(prs), nil
	})
	if err != nil {
		return nil, err
	}
	if r.DefaultBranch != "" {
		var br struct {
			Commit struct {
				ID     string `json:"id"`
				Author *struct {
					Username string `json:"username"`
				} `json:"author"`
			} `json:"commit"`
		}
		if err := c.getJSON(ctx, rp+"/branches/"+url.PathEscape(r.DefaultBranch), &br); err != nil {
			return nil, err
		}
		cm := core.Commit{Repo: repo, SHA: br.Commit.ID, Ref: r.DefaultBranch}
		if br.Commit.Author != nil {
			cm.Author = br.Commit.Author.Username
		}
		out = append(out, cm)
	}
	return out, nil
}

// FileAt returns a file's raw bytes at sha (GET /repos/{o}/{r}/raw/{path}?ref=sha).
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
		return nil, fmt.Errorf("forgejo: bad file path %q", path)
	}
	res, err := c.do(ctx, http.MethodGet, rp+"/raw/"+ep+"?ref="+sha, nil, "*/*")
	if err != nil {
		return nil, err
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
	if err := c.getJSON(ctx, rp+"/contents/"+ep+"?ref="+sha, &entries); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names, nil
}

// Archive returns the API archive URL for a tar.gz at sha and the
// Authorization header to fetch it with. Both are secrets.
func (c *Client) Archive(ctx context.Context, repo, sha string) (string, string, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return "", "", err
	}
	if err := checkSHA(sha); err != nil {
		return "", "", err
	}
	token, err := c.getToken(ctx)
	if err != nil {
		return "", "", err
	}
	return c.api + rp + "/archive/" + sha + ".tar.gz", "Authorization: token " + token, nil
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
	err = c.pages(ctx, rp+"/commits/"+sha+"/status", func(b []byte) (int, error) {
		var page struct {
			Statuses []struct {
				Context string `json:"context"`
				Status  string `json:"status"` // Forgejo and Gitea
				State   string `json:"state"`  // just in case
			} `json:"statuses"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, err
		}
		for _, s := range page.Statuses {
			st := s.Status
			if st == "" {
				st = s.State
			}
			out[s.Context] = st
		}
		return len(page.Statuses), nil
	})
	return out, err
}

// PostStatus sets a commit status (POST /repos/{o}/{r}/statuses/{sha}).
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
		return fmt.Errorf("forgejo: commit status state %q must be pending, success, failure or error", state)
	}
	body := map[string]any{"state": state, "context": context, "description": truncate(description, MaxDescription)}
	if targetURL != "" {
		body["target_url"] = targetURL
	}
	_, err = c.send(ctx, http.MethodPost, rp+"/statuses/"+sha, body, http.StatusCreated, http.StatusOK)
	return err
}
