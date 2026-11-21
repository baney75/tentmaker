package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

type apiRepoRef struct {
	FullName string `json:"full_name"`
}

type apiRun struct {
	ID             int64       `json:"id"`
	Name           string      `json:"name"`
	Path           string      `json:"path"`
	Event          string      `json:"event"`
	Status         string      `json:"status"`
	HeadBranch     string      `json:"head_branch"`
	HeadSHA        string      `json:"head_sha"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
	RunStartedAt   string      `json:"run_started_at"`
	HeadRepository *apiRepoRef `json:"head_repository"`
	Repository     *apiRepoRef `json:"repository"`
}

type apiJob struct {
	ID         int64    `json:"id"`
	RunID      int64    `json:"run_id"`
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	Conclusion string   `json:"conclusion"`
	Labels     []string `json:"labels"`
	CreatedAt  string   `json:"created_at"`
	RunnerID   int64    `json:"runner_id"`
	RunnerName string   `json:"runner_name"`
}

type apiRepo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	Visibility    string `json:"visibility"`
	DefaultBranch string `json:"default_branch"`
	Permissions   *struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
	} `json:"permissions"`
}

func (r apiRepo) public() bool { return !r.Private && r.Visibility != "internal" }

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// fromFork: the run's head repo differs from the repo. A missing head repo
// (a deleted fork) counts as a fork.
func fromFork(run apiRun, repo string) bool {
	if run.HeadRepository == nil || run.HeadRepository.FullName == "" {
		return true
	}
	return !strings.EqualFold(run.HeadRepository.FullName, repo)
}

func toJob(repo string, run apiRun, j apiJob) core.Job {
	created := parseTime(j.CreatedAt)
	if created.IsZero() {
		created = parseTime(run.CreatedAt)
	}
	workflow := run.Path
	if workflow == "" {
		workflow = run.Name
	}
	return core.Job{
		Forge:      "github",
		Repo:       repo,
		ID:         j.ID,
		RunID:      run.ID,
		Name:       j.Name,
		Workflow:   workflow,
		Event:      run.Event,
		HeadBranch: run.HeadBranch,
		Labels:     append([]string(nil), j.Labels...),
		FromFork:   fromFork(run, repo),
		Status:     j.Status,
		CreatedAt:  created,
	}
}

// listRuns lists every run of repo with status, all pages, conditionally.
func (c *Client) listRuns(ctx context.Context, rp, status string) ([]apiRun, error) {
	var runs []apiRun
	err := c.pages(ctx, rp+"/actions/runs?status="+status, true, func(b []byte) (int, error) {
		var page struct {
			WorkflowRuns []apiRun `json:"workflow_runs"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, err
		}
		runs = append(runs, page.WorkflowRuns...)
		return len(page.WorkflowRuns), nil
	})
	return runs, err
}

// jobsOf returns the latest attempt's jobs of a run. With throttle, a
// cached list younger than InProgressRecheck is reused while the run's
// updated_at is unchanged.
func (c *Client) jobsOf(ctx context.Context, repo, rp string, run apiRun, now time.Time, throttle bool) ([]apiJob, error) {
	c.mu.Lock()
	cached, ok := c.runJobs[run.ID]
	c.mu.Unlock()
	if throttle && ok && cached.updatedAt == run.UpdatedAt && now.Sub(cached.fetched) < InProgressRecheck {
		return cached.jobs, nil
	}
	var jobs []apiJob
	err := c.pages(ctx, fmt.Sprintf("%s/actions/runs/%d/jobs?filter=latest", rp, run.ID), true, func(b []byte) (int, error) {
		var page struct {
			Jobs []apiJob `json:"jobs"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, err
		}
		jobs = append(jobs, page.Jobs...)
		return len(page.Jobs), nil
	})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.runJobs[run.ID] = runJobsEntry{repo: repo, fetched: now, updatedAt: run.UpdatedAt, jobs: jobs}
	c.mu.Unlock()
	return jobs, nil
}

// forgetRuns drops cached job lists (and their ETag bodies) of repo's runs
// that are no longer queued or in progress, so memory stays flat.
func (c *Client) forgetRuns(repo, rp string, keep map[int64]bool) {
	var drop []int64
	c.mu.Lock()
	for id, e := range c.runJobs {
		if e.repo == repo && !keep[id] {
			drop = append(drop, id)
			delete(c.runJobs, id)
		}
	}
	c.mu.Unlock()
	for _, id := range drop {
		c.forget(fmt.Sprintf("%s/actions/runs/%d/jobs?", rp, id))
	}
}

// QueuedJobs lists jobs waiting for a runner, oldest first. It reads queued
// and in-progress runs (a run is in progress while its later jobs still
// queue), skipping runs that started more than 24 h ago.
func (c *Client) QueuedJobs(ctx context.Context, repo string) ([]core.Job, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	now := c.now()
	seen := map[int64]bool{}
	jobSeen := map[int64]bool{}
	var out []core.Job
	for _, status := range []string{"queued", "in_progress"} {
		runs, err := c.listRuns(ctx, rp, status)
		if err != nil {
			return nil, err
		}
		for _, run := range runs {
			started := parseTime(run.RunStartedAt)
			if started.IsZero() {
				started = parseTime(run.CreatedAt)
			}
			if !started.IsZero() && now.Sub(started) > RunMaxAge {
				continue
			}
			if seen[run.ID] {
				continue
			}
			seen[run.ID] = true
			jobs, err := c.jobsOf(ctx, repo, rp, run, now, status == "in_progress")
			if err != nil {
				return nil, err
			}
			for _, j := range jobs {
				if j.Status != "queued" || jobSeen[j.ID] {
					continue
				}
				jobSeen[j.ID] = true
				out = append(out, toJob(repo, run, j))
			}
		}
	}
	c.forgetRuns(repo, rp, seen)
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.Before(out[b].CreatedAt)
		}
		return out[a].ID < out[b].ID
	})
	return out, nil
}

// CheckRepo reads visibility, the default branch, the fork policy and what
// the token may do. Anything it can't read becomes a Problem; only an
// unreadable repo is an error.
func (c *Client) CheckRepo(ctx context.Context, repo string) (core.RepoInfo, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return core.RepoInfo{}, err
	}
	var r apiRepo
	if err := c.getJSON(ctx, rp, true, &r); err != nil {
		return core.RepoInfo{}, err
	}
	info := core.RepoInfo{
		Repo:          repo,
		Public:        r.public(),
		DefaultBranch: r.DefaultBranch,
		Problems:      []string{},
	}
	policy, err := c.forkPolicy(ctx, rp, info.Public)
	info.ForkPolicy = policy
	switch {
	case err != nil:
		info.Problems = append(info.Problems, fmt.Sprintf("%s: cannot read the fork pull request policy (Administration: read): %v", repo, err))
	case policy == "unknown":
		info.Problems = append(info.Problems, fmt.Sprintf("%s: workflows from fork pull requests run without approval; require approval for fork pull requests in the repo's Actions settings", repo))
	}
	for _, chk := range []struct{ what, path string }{
		{"workflow runs (Actions: read)", rp + "/actions/runs?per_page=1"},
		{"self-hosted runners (Administration: read)", rp + "/actions/runners?per_page=1"},
	} {
		var discard json.RawMessage
		if err := c.getJSON(ctx, chk.path, true, &discard); err != nil {
			info.Problems = append(info.Problems, fmt.Sprintf("%s: cannot read %s: %v", repo, chk.what, err))
		}
	}
	if r.Permissions != nil && !r.Permissions.Admin {
		info.Problems = append(info.Problems, fmt.Sprintf("%s: the token is not a repo admin, so it can't register runners (Administration: write)", repo))
	}
	return info, nil
}

// forkPolicy maps GitHub's settings to core's: "off", "approve_all",
// "approve_first_time", or "unknown" when fork runs need no approval.
func (c *Client) forkPolicy(ctx context.Context, rp string, public bool) (string, error) {
	if public {
		var p struct {
			ApprovalPolicy string `json:"approval_policy"`
		}
		if err := c.getJSON(ctx, rp+"/actions/permissions/fork-pr-contributor-approval", true, &p); err != nil {
			return "unknown", err
		}
		switch p.ApprovalPolicy {
		case "all_external_contributors":
			return "approve_all", nil
		case "first_time_contributors", "first_time_contributors_new_to_github":
			return "approve_first_time", nil
		}
		return "unknown", fmt.Errorf("unrecognized approval_policy %q", truncate(p.ApprovalPolicy, 60))
	}
	var p struct {
		Run     bool `json:"run_workflows_from_fork_pull_requests"`
		Approve bool `json:"require_approval_for_fork_pr_workflows"`
	}
	if err := c.getJSON(ctx, rp+"/actions/permissions/fork-pr-workflows-private-repos", true, &p); err != nil {
		return "unknown", err
	}
	switch {
	case !p.Run:
		return "off", nil
	case p.Approve:
		return "approve_all", nil
	}
	return "unknown", nil
}

// SetForkApproval makes a public repo require approval for every outside
// contributor and a private repo never run workflows from fork PRs.
func (c *Client) SetForkApproval(ctx context.Context, repo string) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	var r apiRepo
	if err := c.getJSON(ctx, rp, true, &r); err != nil {
		return err
	}
	if r.public() {
		_, err = c.send(ctx, http.MethodPut, rp+"/actions/permissions/fork-pr-contributor-approval",
			map[string]any{"approval_policy": "all_external_contributors"}, http.StatusNoContent, http.StatusOK)
	} else {
		_, err = c.send(ctx, http.MethodPut, rp+"/actions/permissions/fork-pr-workflows-private-repos",
			map[string]any{"run_workflows_from_fork_pull_requests": false}, http.StatusNoContent, http.StatusOK)
	}
	c.forget(rp + "/actions/permissions/")
	return err
}

// SetReadOnlyToken makes the repo's default GITHUB_TOKEN read-only, so a
// job without its own permissions block carries no write token.
func (c *Client) SetReadOnlyToken(ctx context.Context, repo string) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	_, err = c.send(ctx, http.MethodPut, rp+"/actions/permissions/workflow",
		map[string]any{"default_workflow_permissions": "read", "can_approve_pull_request_reviews": false},
		http.StatusNoContent, http.StatusOK)
	return err
}

// Grant registers one JIT runner in runner group 1. Grant.Secret is the
// encoded JIT config.
func (c *Client) Grant(ctx context.Context, repo, runnerName string, labels []string) (core.Grant, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return core.Grant{}, err
	}
	if !core.NamePattern.MatchString(runnerName) {
		return core.Grant{}, fmt.Errorf("github: runner name %q is not allowed", runnerName)
	}
	res, err := c.send(ctx, http.MethodPost, rp+"/actions/runners/generate-jitconfig", map[string]any{
		"name":            runnerName,
		"runner_group_id": RunnerGroupID,
		"labels":          labels,
		"work_folder":     "_work",
	}, http.StatusCreated)
	if err != nil {
		return core.Grant{}, err
	}
	var data struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	if err := json.Unmarshal(res.body, &data); err != nil || data.Runner.ID == 0 || data.EncodedJITConfig == "" {
		return core.Grant{}, fmt.Errorf("github: generate-jitconfig for %s: the response has no runner id or config", repo)
	}
	jit := []byte(data.EncodedJITConfig)
	return core.Grant{
		Forge:      "github",
		Repo:       repo,
		RunnerID:   data.Runner.ID,
		RunnerName: runnerName,
		Labels:     append([]string(nil), labels...),
		Secret:     jit,
		ExpiresAt:  c.now().Add(GrantTTL),
	}, nil
}

// Runner reports a runner's state. A missing runner is Exists=false. GitHub
// does not say which job a busy runner has, so JobID is 0 (use RunningJobs).
func (c *Client) Runner(ctx context.Context, repo string, runnerID int64) (core.RunnerStatus, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return core.RunnerStatus{}, err
	}
	var r struct {
		Status string `json:"status"`
		Busy   bool   `json:"busy"`
	}
	err = c.getJSON(ctx, fmt.Sprintf("%s/actions/runners/%d", rp, runnerID), true, &r)
	if statusOf(err) == http.StatusNotFound {
		return core.RunnerStatus{Exists: false}, nil
	}
	if err != nil {
		return core.RunnerStatus{}, err
	}
	return core.RunnerStatus{Exists: true, Busy: r.Busy, Status: r.Status}, nil
}

// JobByID reads one job and its run. A missing job matches core.ErrNotFound.
func (c *Client) JobByID(ctx context.Context, repo string, jobID int64) (core.Job, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return core.Job{}, err
	}
	var j apiJob
	if err := c.getJSON(ctx, fmt.Sprintf("%s/actions/jobs/%d", rp, jobID), true, &j); err != nil {
		return core.Job{}, err
	}
	var run apiRun
	if err := c.getJSON(ctx, fmt.Sprintf("%s/actions/runs/%d", rp, j.RunID), true, &run); err != nil {
		return core.Job{}, err
	}
	return toJob(repo, run, j), nil
}

// RunningJobs lists every in-progress job of every in-progress run.
func (c *Client) RunningJobs(ctx context.Context, repo string) ([]core.Job, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	runs, err := c.listRuns(ctx, rp, "in_progress")
	if err != nil {
		return nil, err
	}
	var out []core.Job
	now := c.now()
	for _, run := range runs {
		jobs, err := c.jobsOf(ctx, repo, rp, run, now, false)
		if err != nil {
			return nil, err
		}
		for _, j := range jobs {
			if j.Status == "in_progress" {
				out = append(out, toJob(repo, run, j))
			}
		}
	}
	return out, nil
}

// DeleteRunner unregisters a runner; a missing one is fine.
func (c *Client) DeleteRunner(ctx context.Context, repo string, runnerID int64) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	p := fmt.Sprintf("%s/actions/runners/%d", rp, runnerID)
	_, err = c.send(ctx, http.MethodDelete, p, nil, http.StatusNoContent, http.StatusNotFound)
	c.forgetExact(p)
	return err
}

// CancelRun cancels a workflow run. A run that already finished (409) is
// fine.
func (c *Client) CancelRun(ctx context.Context, repo string, runID int64) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	_, err = c.send(ctx, http.MethodPost, fmt.Sprintf("%s/actions/runs/%d/cancel", rp, runID), nil, http.StatusAccepted, http.StatusConflict)
	return err
}
