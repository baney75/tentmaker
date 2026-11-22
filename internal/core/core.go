// Package core holds the types and interfaces every Tentmaker package shares.
//
// The contracts here are fixed: forges find work, executors run it in a
// sandbox, the budget decides how much of the machine CI may use right now,
// and the mesh lets devices on one tailnet act as one pool. Packages depend on
// core, never on each other's internals.
package core

import (
	"context"
	"errors"
	"io"
	"regexp"
	"time"
)

// Resources is an amount of machine: memory in bytes and CPUs (fractional).
type Resources struct {
	MemoryBytes int64   `json:"memory_bytes"`
	CPUs        float64 `json:"cpus"`
}

// Fits reports whether r fits inside limit.
func (r Resources) Fits(limit Resources) bool {
	return r.MemoryBytes <= limit.MemoryBytes && r.CPUs <= limit.CPUs+1e-9
}

// Add returns r + o.
func (r Resources) Add(o Resources) Resources {
	return Resources{MemoryBytes: r.MemoryBytes + o.MemoryBytes, CPUs: r.CPUs + o.CPUs}
}

// Platform is what a job runs on: an OS and a CPU architecture.
type Platform struct {
	OS   string `json:"os"`   // "linux", "macos", "windows"
	Arch string `json:"arch"` // "x64", "arm64" (GitHub's spelling)
}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Job is one queued CI job on a forge.
type Job struct {
	Forge      string    `json:"forge"` // "github", "forgejo"
	Repo       string    `json:"repo"`  // "owner/name"
	ID         int64     `json:"id"`
	RunID      int64     `json:"run_id"`
	Name       string    `json:"name"`
	Workflow   string    `json:"workflow"`
	Event      string    `json:"event"` // push, pull_request, schedule, workflow_dispatch
	HeadBranch string    `json:"head_branch"`
	Labels     []string  `json:"labels"`
	FromFork   bool      `json:"from_fork"` // head repo differs from base repo
	Status     string    `json:"status"`    // queued, in_progress, completed, waiting
	CreatedAt  time.Time `json:"created_at"`
	// HeadSHA is the commit a check job (Event KindCheck) tests. Check jobs
	// have no forge ID, so it and Name identify them.
	HeadSHA string `json:"head_sha,omitempty"`
	// RunnerID and RunnerName name the runner that took the job, when the
	// forge says (GitHub does on in-progress and finished jobs). They are
	// how the broker learns which job a runner took when Runner reports no
	// JobID.
	RunnerID   int64  `json:"runner_id,omitempty"`
	RunnerName string `json:"runner_name,omitempty"`
	// HTMLURL is the job's page on the forge, when the forge says (GitHub's
	// html_url). Run history links to it.
	HTMLURL string `json:"html_url,omitempty"`
}

// Key identifies a job across the pool. A check job's key is its commit and
// job name, since every check job has ID 0.
func (j Job) Key() string {
	if j.Event == KindCheck && j.HeadSHA != "" {
		return j.Forge + ":" + j.Repo + ":" + KindCheck + ":" + j.HeadSHA + ":" + j.Name
	}
	return j.Forge + ":" + j.Repo + ":" + itoa(j.ID)
}

// RepoInfo is what a forge reports about a repository's safety settings.
type RepoInfo struct {
	Repo          string   `json:"repo"`
	Public        bool     `json:"public"`
	DefaultBranch string   `json:"default_branch"`
	ForkPolicy    string   `json:"fork_policy"` // "off", "approve_all", "approve_first_time", "unknown"
	Problems      []string `json:"problems"`    // missing permissions, unreadable policy, ...
}

// Grant is a single-use registration for one ephemeral runner, such as a
// GitHub JIT config. Secret must never be logged, put in argv or an
// environment variable, or written anywhere but a 0600 file the runner
// deletes on read.
type Grant struct {
	Forge      string    `json:"forge"`
	Repo       string    `json:"repo"`
	RunnerID   int64     `json:"runner_id"`
	RunnerName string    `json:"runner_name"`
	Labels     []string  `json:"labels"`
	Secret     []byte    `json:"-"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// RunnerStatus is the forge's view of a runner it registered.
type RunnerStatus struct {
	Exists bool   `json:"exists"`
	Busy   bool   `json:"busy"`
	JobID  int64  `json:"job_id"` // 0 if unknown
	Status string `json:"status"` // online, offline
}

// Forge is a source of CI work (GitHub, Forgejo/Gitea).
type Forge interface {
	Name() string
	// QueuedJobs lists jobs waiting for a runner in repo, oldest first.
	QueuedJobs(ctx context.Context, repo string) ([]Job, error)
	// CheckRepo reads visibility, fork policy and token permissions.
	CheckRepo(ctx context.Context, repo string) (RepoInfo, error)
	// Grant registers one ephemeral runner with exactly these labels.
	Grant(ctx context.Context, repo, runnerName string, labels []string) (Grant, error)
	// Runner reports a registered runner's state.
	Runner(ctx context.Context, repo string, runnerID int64) (RunnerStatus, error)
	// JobByID reads one job (to identify what a runner took).
	JobByID(ctx context.Context, repo string, jobID int64) (Job, error)
	// RunningJobs lists the repo's in-progress jobs (to find a busy runner's job).
	RunningJobs(ctx context.Context, repo string) ([]Job, error)
	// DeleteRunner unregisters a runner. Deleting a missing runner is not an error.
	DeleteRunner(ctx context.Context, repo string, runnerID int64) error
	// CancelRun cancels a workflow run (used on fork jobs that slipped through).
	CancelRun(ctx context.Context, repo string, runID int64) error
}

// CheapPoller is a forge whose unchanged polls cost no rate limit (GitHub
// answers a conditional GET with 304, which its primary limit doesn't
// count). The broker polls such a forge's repos at a short fixed interval
// while CheapPolls is true; it is false when the forge says its rate limit
// runs low, and then the broker backs off as for any other forge.
type CheapPoller interface {
	CheapPolls() bool
}

// ExecState is an executor's view of one runner it started.
type ExecState string

const (
	ExecRunning ExecState = "running"
	ExecExited  ExecState = "exited"
	ExecMissing ExecState = "missing"
	ExecUnknown ExecState = "unknown" // treat as running: never cut a live job
)

// Exec kinds. A runner job runs the forge's own runner (GitHub actions/runner)
// with a single-use registration. A check job runs one workflow job's steps
// directly with `tentmaker check-run` and the broker posts a commit status;
// it works with any forge that has a commit status API.
const (
	KindRunner = "runner"
	KindCheck  = "check"
)

// ExecSpec is everything an executor needs to start one runner.
//
// For KindRunner, Grant.Secret is the forge's runner registration (a GitHub
// JIT config). For KindCheck, Grant.Secret is a JSON-encoded CheckBundle and
// the executor runs `<tentmaker binary for the guest platform> check-run
// --out <dir>` inside the sandbox with the secret on stdin; check-run writes
// <dir>/result.json (a CheckResult) and <dir>/log.txt.
type ExecSpec struct {
	Kind        string        // KindRunner (default when empty) or KindCheck
	Name        string        // runner name, matches NamePattern
	Grant       Grant         // the single-use registration
	Platform    Platform      // what the job runs on
	Limits      Resources     // hard per-job limit
	MaxDuration time.Duration // the executor kills the job after this
	TestWorkers int           // exported to the job as TENTMAKER_JOBS
	// Cache names the repo whose persistent build cache the job may use,
	// or is zero for none. The worker sets it only from an offer the broker
	// marked cache-safe (WorkOffer.Cache), so no fork job ever sees one.
	Cache CacheRepo
}

// CacheRepo names one repo's persistent cache: the forge's name in the
// config and the repo's owner/name. A job only ever sees its own repo's.
// Job, the job's name, only steers which of the repo's job folders it gets
// (the one its last run used, so path-keyed caches hit); it never picks
// the cache.
type CacheRepo struct {
	Forge string `json:"forge"`
	Repo  string `json:"repo"`
	Job   string `json:"job,omitempty"`
}

// IsZero reports whether no cache is named.
func (c CacheRepo) IsZero() bool { return c.Forge == "" || c.Repo == "" }

// NamePattern is the only shape a runner name may take. Executors refuse
// anything else, since names reach argv, unit names and paths.
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,62}$`)

// Problem is one failed preflight or doctor check, with the fix.
type Problem struct {
	Check string `json:"check"`
	What  string `json:"what"`
	Fix   string `json:"fix"`
}

// Executor runs runners in a sandbox on this machine.
type Executor interface {
	Name() string // "linux-podman", "macos-tart", "windows-job"
	// Platforms lists what this executor can run here (tart on a Mac offers
	// macos/arm64 and linux/arm64).
	Platforms() []Platform
	// MaxSlots is a hard cap from the sandbox itself (Apple allows 2 macOS VMs).
	MaxSlots() int
	// Preflight checks the host can run jobs safely. Empty means ready.
	Preflight(ctx context.Context) []Problem
	Start(ctx context.Context, spec ExecSpec) error
	State(ctx context.Context, name string) (ExecState, error)
	// Stop ends a runner and its processes. Stopping a missing runner is fine.
	Stop(ctx context.Context, name string) error
	// Clean deletes a stopped runner's files, VM or containers.
	Clean(ctx context.Context, name string) error
	// List names the runners this executor has on the host (for reconcile).
	List(ctx context.Context) ([]string, error)
	// Reset wipes all CI state between jobs, only when nothing runs.
	Reset(ctx context.Context) error
	// SetPoolLimit applies the whole-pool budget where the sandbox supports it
	// (a cgroup slice on Linux). It must never shrink below what running jobs use.
	SetPoolLimit(ctx context.Context, limit Resources, inUse Resources) error
	// CheckResult reads a finished KindCheck job's result.json and the tail
	// (last 64 KiB) of its log. Call it after the job exits, before Clean.
	CheckResult(ctx context.Context, name string) (CheckResult, error)
}

// CheckStep is one step of a planned check job.
type CheckStep struct {
	Name             string            `json:"name"`
	Run              string            `json:"run"`
	Shell            string            `json:"shell"` // "bash" (default), "sh", "pwsh"
	WorkingDirectory string            `json:"working_directory"`
	Env              map[string]string `json:"env"`
	ContinueOnError  bool              `json:"continue_on_error"`
	// Tool is set for a setup action Tentmaker maps to a local tool, such
	// as {"go": "1.25"} from actions/setup-go.
	Tool map[string]string `json:"tool,omitempty"`
}

// CheckJob is one workflow job planned for checks mode.
type CheckJob struct {
	Workflow string            `json:"workflow"` // path in the repo, ".github/workflows/ci.yml"
	ID       string            `json:"id"`       // job key in the workflow
	Name     string            `json:"name"`     // display name, used in the status context
	Platform Platform          `json:"platform"`
	Env      map[string]string `json:"env"`
	Steps    []CheckStep       `json:"steps"`
	Timeout  time.Duration     `json:"timeout"`
}

// CheckBundle is what check-run receives on stdin.
//
// A broker never puts a forge URL, a forge token or a download header in a
// bundle: it sets ArchiveRef, an opaque reference the worker that was
// granted the check redeems through the broker's ArchiveSource. Before the
// job starts, the worker swaps ArchiveRef for ArchiveURL, a loopback relay
// URL whose random id is the only capability (Darwin VMs get a file://
// URL instead). The bundle is still handled as a secret.
type CheckBundle struct {
	Repo       string   `json:"repo"`
	SHA        string   `json:"sha"`
	ArchiveURL string   `json:"archive_url,omitempty"` // tar.gz of the repo at SHA: http(s) or file://
	Header     string   `json:"header,omitempty"`      // optional "Name: value" for the download; brokers never set it
	ArchiveRef string   `json:"archive_ref,omitempty"` // opaque; the worker turns it into ArchiveURL
	Ref        string   `json:"ref,omitempty"`         // branch name, for github.ref_name
	Job        CheckJob `json:"job"`
}

// ArchiveSource streams a check's source archive. A broker implements it
// (in-process, or over the mesh's GET /v1/archive/<ref>): it serves ref only
// to the device the check was granted to, while the check is active, and
// fetches from the forge itself, so the forge token never leaves the
// broker. An unknown ref, another device or an ended check is ErrNotFound.
type ArchiveSource interface {
	FetchArchive(ctx context.Context, ref, device string, w io.Writer) error
}

// JobLog is the tail of one job's log as an executor reads it: the
// runner's diag logs (_diag/Runner_*.log, then _diag/Worker_*.log) for a
// runner job, check-run's log.txt for a check job. Size is the total size
// of the files read, so a caller polling a running job can tell what is
// new. Data is raw: the caller redacts it before it keeps or shows it.
type JobLog struct {
	Data []byte `json:"-"`
	Size int64  `json:"size"`
}

// Usage is what one job used, read from its sandbox. Zero fields are
// unknown.
type Usage struct {
	PeakMemoryBytes int64   `json:"peak_memory_bytes,omitempty"`
	CPUSeconds      float64 `json:"cpu_seconds,omitempty"`
	// CacheBytes is the size of the repo cache the job started with.
	CacheBytes int64 `json:"cache_bytes,omitempty"`
}

// JobInspector is an executor that can show a job's log and what it used,
// while it runs and after it exits, until Clean. Linux and Windows
// implement JobLog; only Linux (the job's cgroup) implements JobUsage with
// numbers. Every file is read so a job can't point the daemon elsewhere
// (no symlink leaves the job folder, regular files only).
type JobInspector interface {
	JobLog(ctx context.Context, name string, max int64) (JobLog, error)
	JobUsage(ctx context.Context, name string) (Usage, error)
}

// JobReport is how a finished runner job went, as the forge saw it.
// Conclusion is the forge's own word ("success", "failure", "cancelled",
// "timed_out", ...); it is "" while the job is not completed. Log is the
// tail of the job's own log (step output), raw; the caller redacts it.
type JobReport struct {
	Conclusion string
	FailedStep string
	Log        []byte
	LogErr     error // the log could not be read; the conclusion still holds
}

// JobReporter is a forge that can say how a job ended and hand over its
// log (GitHub). The forge token never leaves the broker that holds it.
type JobReporter interface {
	JobReport(ctx context.Context, repo string, jobID, maxLog int64) (JobReport, error)
}

// Rerunner is a forge that can rerun the failed jobs of a run (GitHub).
type Rerunner interface {
	RerunFailed(ctx context.Context, repo string, runID int64) error
}

// CheckResult is the outcome of one check job.
type CheckResult struct {
	Runner      string        `json:"runner"`
	Repo        string        `json:"repo"`
	SHA         string        `json:"sha"`
	Context     string        `json:"context"` // "tentmaker/<job name>"
	State       string        `json:"state"`   // "success", "failure", "error"
	Description string        `json:"description"`
	Duration    time.Duration `json:"duration"`
	LogTail     string        `json:"log_tail"`
}

// Commit is a commit a checks-mode broker may test.
type Commit struct {
	Repo     string `json:"repo"`
	SHA      string `json:"sha"`
	Ref      string `json:"ref"` // branch name
	PR       int    `json:"pr"`  // 0 for a branch push
	Author   string `json:"author"`
	FromFork bool   `json:"from_fork"`
}

// StatusForge is a forge usable in checks mode: it lists commits to test,
// serves workflow files and source archives, and takes commit statuses.
// GitHub, Forgejo/Gitea and GitLab implement it.
type StatusForge interface {
	Name() string
	// PendingCommits lists open PR heads (forks marked FromFork), the
	// default branch head, and the heads of branches that exist (a branch
	// that doesn't is left out, not an error).
	PendingCommits(ctx context.Context, repo string, branches ...string) ([]Commit, error)
	FileAt(ctx context.Context, repo, sha, path string) ([]byte, error)
	ListDir(ctx context.Context, repo, sha, path string) ([]string, error)
	// Archive returns a URL (and optional auth header) for a tar.gz of the repo at sha.
	Archive(ctx context.Context, repo, sha string) (url, header string, err error)
	// Statuses returns context -> state for sha.
	Statuses(ctx context.Context, repo, sha string) (map[string]string, error)
	PostStatus(ctx context.Context, repo, sha, context, state, description, targetURL string) error
}

// PowerState is the machine's power source.
type PowerState struct {
	Known          bool `json:"known"`       // false: the read failed
	OnAC           bool `json:"on_ac"`       // desktops without a battery report true
	HasBattery     bool `json:"has_battery"` //
	BatteryPercent int  `json:"battery_percent"`
}

// PowerSource reads the power state.
type PowerSource interface {
	Power(ctx context.Context) (PowerState, error)
}

// IdleSource reads how long the user has been away. Errors mean "not idle".
type IdleSource interface {
	IdleFor(ctx context.Context) (time.Duration, error)
}

// Decision is the budget engine's answer for this moment.
type Decision struct {
	Profile string    `json:"profile"` // "everyday", "boost", "off", or a schedule name
	Limit   Resources `json:"limit"`   // what CI may use in total right now
	Slots   int       `json:"slots"`   // how many jobs may run at once
	Admit   bool      `json:"admit"`   // may start new jobs
	Evict   bool      `json:"evict"`   // must stop running jobs (hard guard tripped)
	Reasons []string  `json:"reasons"` // plain-language why, for status and logs
}

// Budgeter decides the current Decision.
type Budgeter interface {
	Decide(ctx context.Context, now time.Time, running int) Decision
}

// Claimer coordinates job claims across devices in the pool, so two devices
// don't start runners for the same job. A Claimer that can't reach peers
// returns true: the pool then falls back to a bounded race, never a stall.
type Claimer interface {
	Claim(ctx context.Context, job Job, device string, ttl time.Duration) bool
	Release(ctx context.Context, job Job, device string)
}

// WorkRequest is a worker device asking a broker for jobs. A broker is any
// device holding forge credentials; a worker is any device with free slots.
// One device is usually both, and talks to its own broker in-process. Other
// devices reach brokers over the tailnet, so only brokers hold tokens.
type WorkRequest struct {
	Device    string     `json:"device"`
	Labels    []string   `json:"labels"`    // labels common to every runner this device starts
	Platforms []Platform `json:"platforms"` // what its executors can run
	Free      int        `json:"free"`      // slots it can fill right now (0 = just reporting)
	Limits    Resources  `json:"limits"`    // per-job limit it will enforce
	Running   []string   `json:"running"`   // runner names from this broker still running
	Ended     []string   `json:"ended"`     // runner names that exited since the last request
	// Results carries finished check jobs back to the broker that granted
	// them, which posts the commit statuses.
	Results []CheckResult `json:"results"`
	// OnlyRepo, when set, says a job on the device holds this repo's cache:
	// every job there runs as one user and can reach the others through
	// /proc, so the device takes only that repo's cache-safe jobs until it
	// ends. A broker offers nothing else (an older one that does gets its
	// offers declined).
	OnlyRepo *CacheRepo `json:"only_repo,omitempty"`
}

// WorkOffer is one job for the worker, with the grant to run it.
type WorkOffer struct {
	Job      Job      `json:"job"`
	Grant    Grant    `json:"grant"`
	Secret   []byte   `json:"secret"` // Grant.Secret on the wire (tailnet only, encrypted by WireGuard)
	Platform Platform `json:"platform"`
	// Cache says the broker vouches that no fork job can reach this runner,
	// so the worker may give it the repo's persistent cache: always for a
	// check job (checks mode never plans a fork's commit), and in runner
	// mode only when the forge runs no fork PR workflows for the repo. An
	// older broker never sets it, so its jobs get no cache.
	Cache bool `json:"cache,omitempty"`
	// Drain asks the worker to run this cache-safe job without the cache:
	// the broker skipped another repo's job for the device's OnlyRepo, and
	// that job gets in once the device holds no cache.
	Drain bool `json:"drain,omitempty"`
}

// RepoCacher is an executor that gives a job ExecSpec.Cache. Executors
// without it ignore the field, and the worker never counts their jobs as
// holding a cache.
type RepoCacher interface {
	CachesRepos() bool
}

// WorkResponse answers a WorkRequest.
type WorkResponse struct {
	Offers []WorkOffer `json:"offers"`
	// Stop names runners the worker must stop now (a fork job, an idle
	// runner past its TTL, a cancelled run).
	Stop []string `json:"stop"`
	// Quarantine tells the worker to take nothing from this broker until it
	// has reset (a runner took a job the broker couldn't identify).
	Quarantine bool `json:"quarantine"`
}

// WorkSource hands out work: the local broker, or a remote one over the mesh.
type WorkSource interface {
	Name() string
	Work(ctx context.Context, req WorkRequest) (WorkResponse, error)
}

// ErrNotFound is returned when a forge object does not exist.
var ErrNotFound = errors.New("not found")

// ErrOtherNode is a broker's answer to a work request for a device whose
// grants another tailnet node holds. The mesh answers it with 403.
var ErrOtherNode = errors.New("another tailnet node holds this device's grants")

type callerNodeKey struct{}

// WithCallerNode returns ctx carrying node, the tailnet stable node ID that
// WhoIs authenticated for a remote caller. The mesh sets it before it calls
// a broker's Work or FetchArchive; a broker binds each grant to the caller's
// node and serves that grant's reports and archive only to the same node, so
// another node that announces the same device name can't act for it.
func WithCallerNode(ctx context.Context, node string) context.Context {
	return context.WithValue(ctx, callerNodeKey{}, node)
}

// CallerNode is the node WithCallerNode put in ctx, or "" for an in-process
// caller (the broker's own device).
func CallerNode(ctx context.Context) string {
	s, _ := ctx.Value(callerNodeKey{}).(string)
	return s
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
