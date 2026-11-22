package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/baney75/tentmaker/internal/core"
)

var (
	_ core.PullRequester    = (*Client)(nil)
	_ core.FirstJobReporter = (*Client)(nil)
)

// firstJobRuns is how many of the newest workflow runs FirstJob reads.
const firstJobRuns = 5

type apiPull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// validBranch is a conservative branch name check for a pull request head
// or base: it only has to pass the names Tentmaker itself makes and the
// common default branches.
func validBranch(b string) bool {
	if b == "" || len(b) > 200 || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") ||
		strings.HasSuffix(b, "/") || strings.Contains(b, "..") || strings.Contains(b, "//") {
		return false
	}
	for _, r := range b {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '/') {
			return false
		}
	}
	return true
}

// OpenOrUpdatePR opens a pull request from pr.Head into pr.Base (the repo's
// default branch when empty), or, when one from that head into that base is
// already open, replaces its title and body. Head must be a branch of the
// repo itself.
func (c *Client) OpenOrUpdatePR(ctx context.Context, pr core.PullRequest) (core.PullRequestResult, error) {
	rp, err := repoPath(pr.Repo)
	if err != nil {
		return core.PullRequestResult{}, err
	}
	if !validBranch(pr.Head) {
		return core.PullRequestResult{}, fmt.Errorf("github: head branch %q is not a branch name", pr.Head)
	}
	base := pr.Base
	if base == "" {
		var r apiRepo
		if err := c.getJSON(ctx, rp, false, &r); err != nil {
			return core.PullRequestResult{}, err
		}
		base = r.DefaultBranch
	}
	if !validBranch(base) {
		return core.PullRequestResult{}, fmt.Errorf("github: base branch %q is not a branch name", base)
	}
	owner, _, _ := strings.Cut(pr.Repo, "/")
	q := url.Values{"state": {"open"}, "head": {owner + ":" + pr.Head}, "base": {base}, "per_page": {"5"}}
	var open []apiPull
	if err := c.getJSON(ctx, rp+"/pulls?"+q.Encode(), false, &open); err != nil {
		return core.PullRequestResult{}, err
	}
	if len(open) > 0 {
		p := open[0]
		res, err := c.send(ctx, http.MethodPatch, fmt.Sprintf("%s/pulls/%d", rp, p.Number),
			map[string]string{"title": pr.Title, "body": pr.Body}, http.StatusOK)
		if err != nil {
			return core.PullRequestResult{}, err
		}
		var got apiPull
		if json.Unmarshal(res.body, &got) == nil && got.HTMLURL != "" {
			p = got
		}
		return core.PullRequestResult{Number: p.Number, URL: p.HTMLURL, Created: false}, nil
	}
	res, err := c.send(ctx, http.MethodPost, rp+"/pulls",
		map[string]any{"title": pr.Title, "head": pr.Head, "base": base, "body": pr.Body, "maintainer_can_modify": true}, http.StatusCreated)
	if err != nil {
		return core.PullRequestResult{}, err
	}
	var got apiPull
	if err := json.Unmarshal(res.body, &got); err != nil {
		return core.PullRequestResult{}, fmt.Errorf("github: POST %s/pulls: bad JSON: %v", rp, err)
	}
	return core.PullRequestResult{Number: got.Number, URL: got.HTMLURL, Created: true}, nil
}

type apiRunner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func hasTentmaker(labels []string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, "tentmaker") {
			return true
		}
	}
	return false
}

// FirstJob reports the repo's online runners labeled tentmaker and, from
// its newest firstJobRuns workflow runs, the newest job that ran on such a
// runner and the jobs for the pool still waiting for one.
func (c *Client) FirstJob(ctx context.Context, repo string) (core.FirstJob, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return core.FirstJob{}, err
	}
	out := core.FirstJob{Online: []string{}}
	var runners struct {
		Runners []apiRunner `json:"runners"`
	}
	if err := c.getJSON(ctx, rp+"/actions/runners?per_page=100", true, &runners); err != nil {
		return core.FirstJob{}, err
	}
	for _, r := range runners.Runners {
		names := make([]string, len(r.Labels))
		for i, l := range r.Labels {
			names[i] = l.Name
		}
		if r.Status == "online" && hasTentmaker(names) {
			out.Online = append(out.Online, r.Name)
		}
	}
	var runs struct {
		WorkflowRuns []apiRun `json:"workflow_runs"`
	}
	if err := c.getJSON(ctx, fmt.Sprintf("%s/actions/runs?per_page=%d", rp, firstJobRuns), true, &runs); err != nil {
		return core.FirstJob{}, err
	}
	for _, run := range runs.WorkflowRuns {
		var jobs struct {
			Jobs []apiJob `json:"jobs"`
		}
		if err := c.getJSON(ctx, fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100", rp, run.ID), true, &jobs); err != nil {
			return core.FirstJob{}, err
		}
		for _, j := range jobs.Jobs {
			if !hasTentmaker(j.Labels) {
				continue
			}
			created := parseTime(j.CreatedAt)
			if created.IsZero() {
				created = parseTime(run.CreatedAt)
			}
			switch {
			case j.RunnerName != "":
				if out.RanOn == "" || created.After(out.RanAt) {
					out.RanJob, out.RanOn, out.RanAt = j.Name, j.RunnerName, created
				}
			case j.Status == "queued" || j.Status == "waiting":
				out.Queued++
				if out.OldestQueued.IsZero() || created.Before(out.OldestQueued) {
					out.OldestQueued = created
				}
			}
		}
	}
	return out, nil
}
