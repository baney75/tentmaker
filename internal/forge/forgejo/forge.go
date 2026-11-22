package forgejo

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

// runInfo is what a job needs from its run.
type runInfo struct {
	event    string
	ref      string
	workflow string
	created  time.Time
	fork     bool
}

// fjJob is Forgejo's ActionRunJob.
type fjJob struct {
	ID     int64    `json:"id"`
	RunID  int64    `json:"run_id"`
	Name   string   `json:"name"`
	RunsOn []string `json:"runs_on"`
	Status string   `json:"status"` // waiting, running, success, ...
}

// fjRun is Forgejo's ActionRun.
type fjRun struct {
	ID                int64  `json:"id"`
	Event             string `json:"event"`
	TriggerEvent      string `json:"trigger_event"`
	PrettyRef         string `json:"prettyref"`
	WorkflowID        string `json:"workflow_id"`
	Created           string `json:"created"`
	IsForkPullRequest bool   `json:"is_fork_pull_request"`
}

// gtJob is Gitea's GitHub-shaped ActionWorkflowJob.
type gtJob struct {
	ID         int64    `json:"id"`
	RunID      int64    `json:"run_id"`
	Name       string   `json:"name"`
	Labels     []string `json:"labels"`
	Status     string   `json:"status"` // queued, in_progress, completed, waiting
	CreatedAt  string   `json:"created_at"`
	HeadBranch string   `json:"head_branch"`
	RunnerID   int64    `json:"runner_id"`
}

type gtRepoRef struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

// gtRun is Gitea's ActionWorkflowRun.
type gtRun struct {
	ID             int64      `json:"id"`
	Event          string     `json:"event"`
	HeadBranch     string     `json:"head_branch"`
	Path           string     `json:"path"`
	CreatedAt      string     `json:"created_at"`
	HeadRepository *gtRepoRef `json:"head_repository"`
	Repository     *gtRepoRef `json:"repository"`
	PullRequests   []struct {
		Head struct {
			Repo *gtRepoRef `json:"repo"`
		} `json:"head"`
		Base struct {
			Repo *gtRepoRef `json:"repo"`
		} `json:"base"`
	} `json:"pull_requests"`
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// giteaFork decides whether a Gitea run came from a fork. Gitea 1.25 does
// not fill head_repository, so a pull request event counts as a fork unless
// the run's own data shows the head is the same repo. Other events (push,
// schedule, dispatch) run the repo's own code.
func giteaFork(r gtRun, repo string) bool {
	if r.HeadRepository != nil && r.HeadRepository.FullName != "" {
		return !strings.EqualFold(r.HeadRepository.FullName, repo)
	}
	if !strings.HasPrefix(r.Event, "pull_request") {
		return false
	}
	for _, pr := range r.PullRequests {
		h, b := pr.Head.Repo, pr.Base.Repo
		if h != nil && b != nil && h.ID != 0 && h.ID == b.ID {
			return false
		}
	}
	return true
}

func (c *Client) run(ctx context.Context, flavor, rp, repo string, id int64) (runInfo, error) {
	key := fmt.Sprintf("%s#%d", repo, id)
	c.mu.Lock()
	ri, ok := c.runs[key]
	c.mu.Unlock()
	if ok {
		return ri, nil
	}
	path := fmt.Sprintf("%s/actions/runs/%d", rp, id)
	if flavor == Forgejo {
		var r fjRun
		if err := c.getJSON(ctx, path, &r); err != nil {
			if statusOf(err) == http.StatusNotFound {
				return runInfo{}, fmt.Errorf("forgejo: run %d of %s: not found (Forgejo before 12 has no run API): %w", id, repo, err)
			}
			return runInfo{}, err
		}
		ev := r.TriggerEvent
		if ev == "" {
			ev = r.Event
		}
		ri = runInfo{event: ev, ref: r.PrettyRef, workflow: r.WorkflowID, created: parseTime(r.Created), fork: r.IsForkPullRequest}
	} else {
		var r gtRun
		if err := c.getJSON(ctx, path, &r); err != nil {
			return runInfo{}, err
		}
		ri = runInfo{event: r.Event, ref: r.HeadBranch, workflow: r.Path, created: parseTime(r.CreatedAt), fork: giteaFork(r, repo)}
	}
	c.mu.Lock()
	if len(c.runs) > 4096 {
		c.runs = map[string]runInfo{} // keep memory flat; runs are refetched as needed
	}
	c.runs[key] = ri
	c.mu.Unlock()
	return ri, nil
}

func (c *Client) mkJob(repo string, id int64, name string, labels []string, status string, ri runInfo, runID int64) core.Job {
	return core.Job{
		Forge: c.Name(), Repo: repo, ID: id, RunID: runID, Name: name,
		Workflow: ri.workflow, Event: ri.event, HeadBranch: ri.ref,
		Labels: append([]string(nil), labels...), FromFork: ri.fork,
		Status: status, CreatedAt: ri.created,
	}
}

// forgejoJobs lists Forgejo's waiting and running jobs of a repo
// (GET /repos/{o}/{r}/actions/runners/jobs, Forgejo 11+).
func (c *Client) forgejoJobs(ctx context.Context, rp string) ([]fjJob, error) {
	var jobs []fjJob
	if err := c.getJSON(ctx, rp+"/actions/runners/jobs", &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// giteaJobs lists Gitea's jobs with a status (Gitea 1.25+).
func (c *Client) giteaJobs(ctx context.Context, rp, status string) ([]gtJob, error) {
	var out []gtJob
	err := c.pages(ctx, rp+"/actions/jobs?status="+status, func(b []byte) (int, error) {
		var page struct {
			Jobs []gtJob `json:"jobs"`
		}
		if err := json.Unmarshal(b, &page); err != nil {
			return 0, err
		}
		out = append(out, page.Jobs...)
		return len(page.Jobs), nil
	})
	if statusOf(err) == http.StatusNotFound {
		return nil, fmt.Errorf("forgejo: listing jobs needs Gitea 1.25 or later: %w", ErrUnsupported)
	}
	return out, err
}

// jobsWith lists the repo's jobs in one state ("queued" or "in_progress"),
// oldest first, skipping runs older than 24 h for queued ones.
func (c *Client) jobsWith(ctx context.Context, repo, state string) ([]core.Job, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	flavor, err := c.Flavor(ctx)
	if err != nil {
		return nil, err
	}
	now := c.now()
	var out []core.Job
	add := func(id, runID int64, name string, labels []string) error {
		ri, err := c.run(ctx, flavor, rp, repo, runID)
		if err != nil {
			return err
		}
		if state == "queued" && !ri.created.IsZero() && now.Sub(ri.created) > RunMaxAge {
			return nil
		}
		out = append(out, c.mkJob(repo, id, name, labels, state, ri, runID))
		return nil
	}
	if flavor == Forgejo {
		want := map[string]string{"queued": "waiting", "in_progress": "running"}[state]
		jobs, err := c.forgejoJobs(ctx, rp)
		if err != nil {
			if statusOf(err) == http.StatusNotFound {
				return nil, fmt.Errorf("forgejo: listing jobs needs Forgejo 11 or later: %w", ErrUnsupported)
			}
			return nil, err
		}
		for _, j := range jobs {
			if j.Status == want {
				if err := add(j.ID, j.RunID, j.Name, j.RunsOn); err != nil {
					return nil, err
				}
			}
		}
	} else {
		jobs, err := c.giteaJobs(ctx, rp, state)
		if err != nil {
			return nil, err
		}
		for _, j := range jobs {
			if j.Status == state {
				if err := add(j.ID, j.RunID, j.Name, j.Labels); err != nil {
					return nil, err
				}
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if !out[a].CreatedAt.Equal(out[b].CreatedAt) {
			return out[a].CreatedAt.Before(out[b].CreatedAt)
		}
		return out[a].ID < out[b].ID
	})
	return out, nil
}

// QueuedJobs lists jobs waiting for a runner, oldest first.
func (c *Client) QueuedJobs(ctx context.Context, repo string) ([]core.Job, error) {
	return c.jobsWith(ctx, repo, "queued")
}

// RunningJobs lists the repo's running jobs.
func (c *Client) RunningJobs(ctx context.Context, repo string) ([]core.Job, error) {
	return c.jobsWith(ctx, repo, "in_progress")
}

type apiRepo struct {
	Private       bool   `json:"private"`
	Internal      bool   `json:"internal"`
	DefaultBranch string `json:"default_branch"`
	HasActions    *bool  `json:"has_actions"`
	Permissions   *struct {
		Admin bool `json:"admin"`
	} `json:"permissions"`
}

// CheckRepo reads visibility, the default branch and what the token and
// forge can do. Only an unreadable repo is an error.
func (c *Client) CheckRepo(ctx context.Context, repo string) (core.RepoInfo, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return core.RepoInfo{}, err
	}
	flavor, err := c.Flavor(ctx)
	if err != nil {
		return core.RepoInfo{}, err
	}
	var r apiRepo
	if err := c.getJSON(ctx, rp, &r); err != nil {
		return core.RepoInfo{}, err
	}
	info := core.RepoInfo{
		Repo:          repo,
		Public:        !r.Private && !r.Internal,
		DefaultBranch: r.DefaultBranch,
		ForkPolicy:    "approve_first_time", // built in; see the package comment
		Problems:      []string{},
	}
	if r.HasActions != nil && !*r.HasActions {
		info.Problems = append(info.Problems, fmt.Sprintf("%s: Actions is turned off for this repo; turn it on in the repo settings", repo))
	}
	if r.Permissions != nil && !r.Permissions.Admin {
		info.Problems = append(info.Problems, fmt.Sprintf("%s: the token is not a repo admin, so it can't register or delete runners", repo))
	}
	switch flavor {
	case Gitea:
		info.Problems = append(info.Problems, fmt.Sprintf("%s: Gitea has no single-use runner registration; use mode = \"checks\" for this repo", repo))
	case Forgejo:
		var discard json.RawMessage
		if err := c.getJSON(ctx, rp+"/actions/runners", &discard); err != nil {
			if statusOf(err) == http.StatusNotFound {
				info.Problems = append(info.Problems, fmt.Sprintf("%s: this Forgejo has no runner registration API (it needs Forgejo 15 or later); use mode = \"checks\" or upgrade", repo))
			} else {
				info.Problems = append(info.Problems, fmt.Sprintf("%s: cannot read the repo's runners: %v", repo, err))
			}
		}
		if _, err := c.forgejoJobs(ctx, rp); err != nil {
			info.Problems = append(info.Problems, fmt.Sprintf("%s: cannot read waiting jobs: %v", repo, err))
		}
	}
	return info, nil
}

// Registration is the forgejo-runner registration file (.runner). It is
// Grant.Secret on Forgejo: write it as the runner's .runner file, mode 0600.
type Registration struct {
	Warning string   `json:"WARNING"`
	ID      int64    `json:"id"`
	UUID    string   `json:"uuid"`
	Name    string   `json:"name"`
	Token   string   `json:"token"`
	Address string   `json:"address"`
	Labels  []string `json:"labels"`
}

const registrationWarning = "Written by Tentmaker for one ephemeral job. Do not copy it."

// hostLabels gives each plain label the ":host" scheme: the job already
// runs inside Tentmaker's sandbox, so the runner must not start Docker.
func hostLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if !strings.Contains(l, ":") {
			l += ":host"
		}
		out = append(out, l)
	}
	return out
}

// Grant registers one ephemeral runner (Forgejo 15+). Gitea: ErrUnsupported.
func (c *Client) Grant(ctx context.Context, repo, runnerName string, labels []string) (core.Grant, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return core.Grant{}, err
	}
	if !core.NamePattern.MatchString(runnerName) {
		return core.Grant{}, fmt.Errorf("forgejo: runner name %q is not allowed", runnerName)
	}
	flavor, err := c.Flavor(ctx)
	if err != nil {
		return core.Grant{}, err
	}
	if flavor == Gitea {
		return core.Grant{}, fmt.Errorf("forgejo: Gitea registration tokens are reusable, not single-use: %w", ErrUnsupported)
	}
	res, err := c.send(ctx, http.MethodPost, rp+"/actions/runners", map[string]any{
		"name":        runnerName,
		"description": "Tentmaker ephemeral runner",
		"ephemeral":   true,
	}, http.StatusCreated)
	if s := statusOf(err); s == http.StatusNotFound || s == http.StatusMethodNotAllowed {
		return core.Grant{}, fmt.Errorf("forgejo: registering a runner needs Forgejo 15 or later: %w", ErrUnsupported)
	}
	if err != nil {
		return core.Grant{}, err
	}
	var reg Registration
	if err := json.Unmarshal(res.body, &reg); err != nil || reg.ID == 0 || reg.UUID == "" || reg.Token == "" {
		return core.Grant{}, fmt.Errorf("forgejo: register runner for %s: the response has no id, uuid or token", repo)
	}
	reg.Warning = registrationWarning
	reg.Name = runnerName
	reg.Address = c.instance
	reg.Labels = hostLabels(labels)
	file, err := json.Marshal(reg)
	if err != nil {
		return core.Grant{}, err
	}
	return core.Grant{
		Forge:      c.Name(),
		Repo:       repo,
		RunnerID:   reg.ID,
		RunnerName: runnerName,
		Labels:     append([]string(nil), labels...),
		Secret:     file,
		ExpiresAt:  c.now().Add(GrantTTL),
	}, nil
}

// Runner reports a runner's state. Missing means Exists=false.
func (c *Client) Runner(ctx context.Context, repo string, runnerID int64) (core.RunnerStatus, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return core.RunnerStatus{}, err
	}
	var r struct {
		Status string `json:"status"` // offline, idle, active
		Busy   bool   `json:"busy"`   // Gitea only
	}
	err = c.getJSON(ctx, fmt.Sprintf("%s/actions/runners/%d", rp, runnerID), &r)
	if statusOf(err) == http.StatusNotFound {
		return core.RunnerStatus{Exists: false}, nil
	}
	if err != nil {
		return core.RunnerStatus{}, err
	}
	st := core.RunnerStatus{Exists: true, Busy: r.Busy || r.Status == "active", Status: "online"}
	if r.Status == "offline" || r.Status == "" {
		st.Status = "offline"
	}
	return st, nil
}

// JobByID reads one job. On Forgejo it looks in the waiting and running
// list first, then GET /actions/jobs/{id} (Forgejo 17+).
func (c *Client) JobByID(ctx context.Context, repo string, jobID int64) (core.Job, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return core.Job{}, err
	}
	flavor, err := c.Flavor(ctx)
	if err != nil {
		return core.Job{}, err
	}
	path := fmt.Sprintf("%s/actions/jobs/%d", rp, jobID)
	if flavor == Forgejo {
		jobs, err := c.forgejoJobs(ctx, rp)
		if err != nil && statusOf(err) != http.StatusNotFound {
			return core.Job{}, err
		}
		var found *fjJob
		for i := range jobs {
			if jobs[i].ID == jobID {
				found = &jobs[i]
				break
			}
		}
		if found == nil {
			var j fjJob
			if err := c.getJSON(ctx, path, &j); err != nil {
				return core.Job{}, err
			}
			found = &j
		}
		ri, err := c.run(ctx, flavor, rp, repo, found.RunID)
		if err != nil {
			return core.Job{}, err
		}
		status := map[string]string{"waiting": "queued", "running": "in_progress", "blocked": "waiting"}[found.Status]
		if status == "" {
			status = "completed"
		}
		return c.mkJob(repo, found.ID, found.Name, found.RunsOn, status, ri, found.RunID), nil
	}
	var j gtJob
	if err := c.getJSON(ctx, path, &j); err != nil {
		return core.Job{}, err
	}
	ri, err := c.run(ctx, flavor, rp, repo, j.RunID)
	if err != nil {
		return core.Job{}, err
	}
	return c.mkJob(repo, j.ID, j.Name, j.Labels, j.Status, ri, j.RunID), nil
}

// DeleteRunner unregisters a runner; a missing one is fine.
func (c *Client) DeleteRunner(ctx context.Context, repo string, runnerID int64) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	_, err = c.send(ctx, http.MethodDelete, fmt.Sprintf("%s/actions/runners/%d", rp, runnerID), nil,
		http.StatusNoContent, http.StatusOK, http.StatusNotFound)
	return err
}

// CancelRun cancels a run (Forgejo 16+, Gitea after 1.26). A run that
// already finished is fine.
func (c *Client) CancelRun(ctx context.Context, repo string, runID int64) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	_, err = c.send(ctx, http.MethodPost, fmt.Sprintf("%s/actions/runs/%d/cancel", rp, runID), nil,
		http.StatusOK, http.StatusAccepted, http.StatusNoContent, http.StatusConflict)
	switch statusOf(err) {
	case http.StatusMethodNotAllowed:
		return fmt.Errorf("forgejo: cancelling a run needs Forgejo 16 or a Gitea release after 1.26: %w", ErrUnsupported)
	case http.StatusNotFound:
		return fmt.Errorf("forgejo: cancel run %d of %s: the run is missing or this version has no cancel API (Forgejo 16+): %w", runID, repo, err)
	}
	return err
}
