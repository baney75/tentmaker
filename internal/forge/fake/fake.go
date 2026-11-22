// Package fake is an in-memory forge for tests. It implements core.Forge and
// core.StatusForge, is safe for concurrent use, and has controls to queue
// jobs, make runners take jobs (any matching job, like a GitHub JIT runner),
// serve files and statuses for checks mode, inject errors and count calls.
package fake

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// Method names for SetErr and Calls.
const (
	MQueuedJobs     = "QueuedJobs"
	MCheckRepo      = "CheckRepo"
	MGrant          = "Grant"
	MRunner         = "Runner"
	MJobByID        = "JobByID"
	MRunningJobs    = "RunningJobs"
	MDeleteRunner   = "DeleteRunner"
	MCancelRun      = "CancelRun"
	MRerunFailed    = "RerunFailed"
	MPendingCommits = "PendingCommits"
	MFileAt         = "FileAt"
	MListDir        = "ListDir"
	MArchive        = "Archive"
	MStatuses       = "Statuses"
	MPostStatus     = "PostStatus"
)

// ErrBusy is what DeleteRunner returns for a runner that is running a job
// (GitHub answers 422).
var ErrBusy = errors.New("runner is busy")

// Runner is a registered runner as the fake forge sees it.
type Runner struct {
	ID      int64
	Repo    string
	Name    string
	Labels  []string
	Secret  []byte
	Busy    bool
	JobID   int64
	HideJob bool // Runner reports JobID 0 while busy
	Online  bool
}

// StatusPost is one PostStatus call.
type StatusPost struct {
	Repo, SHA, Context, State, Description, TargetURL string
}

type errKey struct{ method, repo string }

// Forge is the fake. The zero value is not usable; call New.
type Forge struct {
	mu   sync.Mutex
	name string
	now  func() time.Time

	repos      map[string]core.RepoInfo
	jobs       map[string]map[int64]*core.Job
	runners    map[string]map[int64]*Runner
	nextRunner int64
	nextJob    int64
	errs       map[errKey]error
	errOnce    map[errKey]int
	calls      map[string]int
	repoCalls  map[errKey]int
	cancelled  map[string][]int64
	reruns     map[string][]int64
	deleted    map[string][]int64
	grants     []core.Grant

	commits  map[string][]core.Commit
	branches map[string]map[string]string // repo -> branch -> head sha
	files    map[string]map[string][]byte // repo@sha -> path -> content
	statuses map[string]map[string]string // repo@sha -> context -> state
	posts    []StatusPost
	token    string

	likeGitHub bool // Runner never reports a busy runner's JobID
}

var (
	_ core.Forge       = (*Forge)(nil)
	_ core.StatusForge = (*Forge)(nil)
)

// New returns a fake forge called name. now may be nil (time.Now).
func New(name string, now func() time.Time) *Forge {
	if now == nil {
		now = time.Now
	}
	return &Forge{
		name:       name,
		now:        now,
		repos:      map[string]core.RepoInfo{},
		jobs:       map[string]map[int64]*core.Job{},
		runners:    map[string]map[int64]*Runner{},
		nextRunner: 100,
		nextJob:    1000,
		errs:       map[errKey]error{},
		errOnce:    map[errKey]int{},
		calls:      map[string]int{},
		repoCalls:  map[errKey]int{},
		cancelled:  map[string][]int64{},
		reruns:     map[string][]int64{},
		deleted:    map[string][]int64{},
		commits:    map[string][]core.Commit{},
		branches:   map[string]map[string]string{},
		files:      map[string]map[string][]byte{},
		statuses:   map[string]map[string]string{},
		token:      "archive-token-" + name,
	}
}

// Name implements core.Forge and core.StatusForge.
func (f *Forge) Name() string { return f.name }

// ---------------------------------------------------------------- controls

// SetErr makes method fail with err for repo ("" = every repo). A nil err
// clears it.
func (f *Forge) SetErr(method, repo string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := errKey{method, repo}
	if err == nil {
		delete(f.errs, k)
		delete(f.errOnce, k)
		return
	}
	f.errs[k] = err
}

// FailTimes makes method fail with err for repo the next n calls only.
func (f *Forge) FailTimes(method, repo string, n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := errKey{method, repo}
	f.errs[k] = err
	f.errOnce[k] = n
}

// Calls counts calls to method.
func (f *Forge) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// CallsFor counts calls to method for one repo.
func (f *Forge) CallsFor(method, repo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repoCalls[errKey{method, repo}]
}

// SetRepo sets what CheckRepo returns. Repos never set read as private.
func (f *Forge) SetRepo(info core.RepoInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[info.Repo] = info
}

// AddJob queues a job. Zero ID, RunID, Status, Forge and CreatedAt get
// defaults (a new ID, RunID = ID, "queued", this forge, now). It returns the
// stored job.
func (f *Forge) AddJob(j core.Job) core.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j.ID == 0 {
		f.nextJob++
		j.ID = f.nextJob
	}
	if j.RunID == 0 {
		j.RunID = j.ID
	}
	if j.Status == "" {
		j.Status = "queued"
	}
	if j.Forge == "" {
		j.Forge = f.name
	}
	if j.CreatedAt.IsZero() {
		j.CreatedAt = f.now()
	}
	if f.jobs[j.Repo] == nil {
		f.jobs[j.Repo] = map[int64]*core.Job{}
	}
	cp := j
	cp.Labels = append([]string(nil), j.Labels...)
	f.jobs[j.Repo][j.ID] = &cp
	return cp
}

// SetJobStatus changes a job's status ("queued", "waiting", "in_progress", "completed").
func (f *Forge) SetJobStatus(repo string, id int64, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j := f.jobs[repo][id]; j != nil {
		j.Status = status
	}
}

// Job returns a stored job.
func (f *Forge) Job(repo string, id int64) (core.Job, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[repo][id]
	if j == nil {
		return core.Job{}, false
	}
	return *j, true
}

// Take makes the named runner busy on job id (the job goes in_progress).
func (f *Forge) Take(name string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.byName(name)
	if r == nil {
		return fmt.Errorf("no runner %s", name)
	}
	j := f.jobs[r.Repo][id]
	if j == nil {
		return fmt.Errorf("no job %d", id)
	}
	r.Busy, r.JobID, r.Online = true, id, true
	j.Status = "in_progress"
	f.nameRunnerLocked(r, j)
	return nil
}

// TakeAny makes the named runner take the oldest queued job whose labels it
// has, the way GitHub hands a JIT runner any matching job, fork jobs too.
func (f *Forge) TakeAny(name string) (core.Job, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.byName(name)
	if r == nil || r.Busy {
		return core.Job{}, false
	}
	have := map[string]bool{}
	for _, l := range r.Labels {
		have[strings.ToLower(l)] = true
	}
	var best *core.Job
	for _, j := range f.sortedJobs(r.Repo) {
		if j.Status != "queued" {
			continue
		}
		ok := true
		for _, l := range j.Labels {
			if !have[strings.ToLower(l)] {
				ok = false
				break
			}
		}
		if ok {
			best = j
			break
		}
	}
	if best == nil {
		return core.Job{}, false
	}
	r.Busy, r.JobID, r.Online = true, best.ID, true
	best.Status = "in_progress"
	f.nameRunnerLocked(r, best)
	return *best, true
}

// LikeGitHub makes the fake behave like GitHub: Runner never says which job
// a busy runner has (JobID 0), and only the jobs (JobByID, RunningJobs) name
// the runner that took them.
func (f *Forge) LikeGitHub(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.likeGitHub = on
}

// HideJob makes Runner report JobID 0 for the named runner while busy, and
// the job it took name no runner: the forge never says which job it is.
func (f *Forge) HideJob(name string, hide bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.byName(name)
	if r == nil {
		return
	}
	r.HideJob = hide
	if j := f.jobs[r.Repo][r.JobID]; r.Busy && j != nil {
		f.nameRunnerLocked(r, j)
	}
}

// nameRunnerLocked records on j the runner that took it, unless the runner
// hides its job.
func (f *Forge) nameRunnerLocked(r *Runner, j *core.Job) {
	j.RunnerID, j.RunnerName = r.ID, r.Name
	if r.HideJob {
		j.RunnerID, j.RunnerName = 0, ""
	}
}

// Finish completes the named runner's job and removes the runner, as an
// ephemeral runner does after one job.
func (f *Forge) Finish(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.byName(name)
	if r == nil {
		return
	}
	if j := f.jobs[r.Repo][r.JobID]; j != nil {
		j.Status = "completed"
	}
	delete(f.runners[r.Repo], r.ID)
}

// Runners lists repo's registered runners.
func (f *Forge) Runners(repo string) []Runner {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Runner
	for _, r := range f.runners[repo] {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out
}

// AllRunners lists every registered runner.
func (f *Forge) AllRunners() []Runner {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Runner
	for _, m := range f.runners {
		for _, r := range m {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out
}

// RunnerByName finds a registered runner.
func (f *Forge) RunnerByName(name string) (Runner, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.byName(name)
	if r == nil {
		return Runner{}, false
	}
	return *r, true
}

// Grants lists every grant made, secrets included.
func (f *Forge) Grants() []core.Grant {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Grant(nil), f.grants...)
}

// Cancelled lists the run IDs cancelled in repo.
func (f *Forge) Cancelled(repo string) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.cancelled[repo]...)
}

// Reruns lists the run IDs whose failed jobs were rerun in repo.
func (f *Forge) Reruns(repo string) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.reruns[repo]...)
}

// RerunFailed records a rerun of a run's failed jobs (core.Rerunner).
func (f *Forge) RerunFailed(ctx context.Context, repo string, runID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MRerunFailed, repo); err != nil {
		return err
	}
	f.reruns[repo] = append(f.reruns[repo], runID)
	return nil
}

// Deleted lists the runner IDs deleted in repo.
func (f *Forge) Deleted(repo string) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.deleted[repo]...)
}

// SetCommits sets what PendingCommits returns for repo.
func (f *Forge) SetCommits(repo string, cs []core.Commit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits[repo] = append([]core.Commit(nil), cs...)
}

// SetBranch sets the head of a branch PendingCommits returns when it is
// asked for that branch.
func (f *Forge) SetBranch(repo, branch, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.branches[repo] == nil {
		f.branches[repo] = map[string]string{}
	}
	f.branches[repo][branch] = sha
}

// SetFile stores a file at repo@sha.
func (f *Forge) SetFile(repo, sha, p string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := repo + "@" + sha
	if f.files[k] == nil {
		f.files[k] = map[string][]byte{}
	}
	f.files[k][p] = append([]byte(nil), content...)
}

// SetStatus sets a commit status without counting a PostStatus call.
func (f *Forge) SetStatus(repo, sha, ctxName, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setStatus(repo, sha, ctxName, state)
}

// Status reads a commit status ("" if none).
func (f *Forge) Status(repo, sha, ctxName string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses[repo+"@"+sha][ctxName]
}

// Posts lists every PostStatus call in order.
func (f *Forge) Posts() []StatusPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StatusPost(nil), f.posts...)
}

// ArchiveToken is the secret token the fake puts in archive URLs.
func (f *Forge) ArchiveToken() string { return f.token }

// ---------------------------------------------------------------- core.Forge

func (f *Forge) call(method, repo string) error {
	f.calls[method]++
	f.repoCalls[errKey{method, repo}]++
	for _, k := range []errKey{{method, repo}, {method, ""}} {
		err, ok := f.errs[k]
		if !ok {
			continue
		}
		if n, once := f.errOnce[k]; once {
			if n <= 1 {
				delete(f.errs, k)
				delete(f.errOnce, k)
			} else {
				f.errOnce[k] = n - 1
			}
		}
		return err
	}
	return nil
}

// QueuedJobs implements core.Forge: jobs with status queued or waiting,
// oldest first.
func (f *Forge) QueuedJobs(ctx context.Context, repo string) ([]core.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MQueuedJobs, repo); err != nil {
		return nil, err
	}
	var out []core.Job
	for _, j := range f.sortedJobs(repo) {
		if j.Status == "queued" || j.Status == "waiting" {
			cp := *j
			cp.Labels = append([]string(nil), j.Labels...)
			out = append(out, cp)
		}
	}
	return out, nil
}

// CheckRepo implements core.Forge.
func (f *Forge) CheckRepo(ctx context.Context, repo string) (core.RepoInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MCheckRepo, repo); err != nil {
		return core.RepoInfo{}, err
	}
	if info, ok := f.repos[repo]; ok {
		return info, nil
	}
	return core.RepoInfo{Repo: repo, Public: false, DefaultBranch: "main", ForkPolicy: "off"}, nil
}

// Grant implements core.Forge. The secret is "jit-<name>-<id>".
func (f *Forge) Grant(ctx context.Context, repo, runnerName string, labels []string) (core.Grant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MGrant, repo); err != nil {
		return core.Grant{}, err
	}
	if !core.NamePattern.MatchString(runnerName) {
		return core.Grant{}, fmt.Errorf("bad runner name %q", runnerName)
	}
	for _, r := range f.runners[repo] {
		if r.Name == runnerName {
			return core.Grant{}, fmt.Errorf("runner %s already exists", runnerName)
		}
	}
	f.nextRunner++
	id := f.nextRunner
	secret := []byte(fmt.Sprintf("jit-%s-%d", runnerName, id))
	if f.runners[repo] == nil {
		f.runners[repo] = map[int64]*Runner{}
	}
	f.runners[repo][id] = &Runner{ID: id, Repo: repo, Name: runnerName, Labels: append([]string(nil), labels...), Secret: secret}
	g := core.Grant{Forge: f.name, Repo: repo, RunnerID: id, RunnerName: runnerName,
		Labels: append([]string(nil), labels...), Secret: secret, ExpiresAt: f.now().Add(time.Hour)}
	f.grants = append(f.grants, g)
	return g, nil
}

// Runner implements core.Forge. A missing runner reports Exists false.
func (f *Forge) Runner(ctx context.Context, repo string, runnerID int64) (core.RunnerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MRunner, repo); err != nil {
		return core.RunnerStatus{}, err
	}
	r := f.runners[repo][runnerID]
	if r == nil {
		return core.RunnerStatus{}, nil
	}
	st := core.RunnerStatus{Exists: true, Busy: r.Busy, Status: "offline"}
	if r.Online || r.Busy {
		st.Status = "online"
	}
	if r.Busy && !r.HideJob && !f.likeGitHub {
		st.JobID = r.JobID
	}
	return st, nil
}

// JobByID implements core.Forge.
func (f *Forge) JobByID(ctx context.Context, repo string, jobID int64) (core.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MJobByID, repo); err != nil {
		return core.Job{}, err
	}
	j := f.jobs[repo][jobID]
	if j == nil {
		return core.Job{}, core.ErrNotFound
	}
	return *j, nil
}

// RunningJobs implements core.Forge.
func (f *Forge) RunningJobs(ctx context.Context, repo string) ([]core.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MRunningJobs, repo); err != nil {
		return nil, err
	}
	var out []core.Job
	for _, j := range f.sortedJobs(repo) {
		if j.Status == "in_progress" {
			out = append(out, *j)
		}
	}
	return out, nil
}

// DeleteRunner implements core.Forge. A busy runner can't be deleted.
func (f *Forge) DeleteRunner(ctx context.Context, repo string, runnerID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MDeleteRunner, repo); err != nil {
		return err
	}
	r := f.runners[repo][runnerID]
	if r == nil {
		return nil
	}
	if r.Busy {
		return ErrBusy
	}
	delete(f.runners[repo], runnerID)
	f.deleted[repo] = append(f.deleted[repo], runnerID)
	return nil
}

// CancelRun implements core.Forge: every job of the run is completed.
func (f *Forge) CancelRun(ctx context.Context, repo string, runID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MCancelRun, repo); err != nil {
		return err
	}
	f.cancelled[repo] = append(f.cancelled[repo], runID)
	for _, j := range f.jobs[repo] {
		if j.RunID == runID {
			j.Status = "completed"
		}
	}
	return nil
}

// ---------------------------------------------------------------- core.StatusForge

// PendingCommits implements core.StatusForge.
// Branches set with SetBranch follow the commits from SetCommits.
func (f *Forge) PendingCommits(ctx context.Context, repo string, branches ...string) ([]core.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MPendingCommits, repo); err != nil {
		return nil, err
	}
	out := append([]core.Commit(nil), f.commits[repo]...)
	for _, b := range branches {
		if sha, ok := f.branches[repo][b]; ok {
			out = append(out, core.Commit{Repo: repo, SHA: sha, Ref: b})
		}
	}
	return out, nil
}

// FileAt implements core.StatusForge.
func (f *Forge) FileAt(ctx context.Context, repo, sha, p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MFileAt, repo); err != nil {
		return nil, err
	}
	b, ok := f.files[repo+"@"+sha][p]
	if !ok {
		return nil, core.ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

// ListDir implements core.StatusForge: the names (not paths) of the files
// directly under dir.
func (f *Forge) ListDir(ctx context.Context, repo, sha, dir string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MListDir, repo); err != nil {
		return nil, err
	}
	var out []string
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for p := range f.files[repo+"@"+sha] {
		if strings.HasPrefix(p, prefix) && !strings.Contains(p[len(prefix):], "/") {
			out = append(out, path.Base(p))
		}
	}
	if len(out) == 0 {
		return nil, core.ErrNotFound
	}
	sort.Strings(out)
	return out, nil
}

// Archive implements core.StatusForge. The URL carries a token.
func (f *Forge) Archive(ctx context.Context, repo, sha string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MArchive, repo); err != nil {
		return "", "", err
	}
	return fmt.Sprintf("https://forge.example/%s/archive/%s.tar.gz?token=%s", repo, sha, f.token),
		"Authorization: Bearer " + f.token, nil
}

// Statuses implements core.StatusForge.
func (f *Forge) Statuses(ctx context.Context, repo, sha string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MStatuses, repo); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range f.statuses[repo+"@"+sha] {
		out[k] = v
	}
	return out, nil
}

// PostStatus implements core.StatusForge.
func (f *Forge) PostStatus(ctx context.Context, repo, sha, ctxName, state, description, targetURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call(MPostStatus, repo); err != nil {
		return err
	}
	f.posts = append(f.posts, StatusPost{repo, sha, ctxName, state, description, targetURL})
	f.setStatus(repo, sha, ctxName, state)
	return nil
}

// ---------------------------------------------------------------- helpers

func (f *Forge) setStatus(repo, sha, ctxName, state string) {
	k := repo + "@" + sha
	if f.statuses[k] == nil {
		f.statuses[k] = map[string]string{}
	}
	f.statuses[k][ctxName] = state
}

func (f *Forge) byName(name string) *Runner {
	for _, m := range f.runners {
		for _, r := range m {
			if r.Name == name {
				return r
			}
		}
	}
	return nil
}

func (f *Forge) sortedJobs(repo string) []*core.Job {
	var out []*core.Job
	for _, j := range f.jobs[repo] {
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool {
		if !out[i].CreatedAt.Equal(out[k].CreatedAt) {
			return out[i].CreatedAt.Before(out[k].CreatedAt)
		}
		return out[i].ID < out[k].ID
	})
	return out
}
