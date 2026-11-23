# Tentmaker design

Tentmaker turns spare capacity on your own machines into CI runners. Each device gives CI a set
amount of memory and CPU every day, and more when it is plugged in. Devices on one tailnet form a
pool. Jobs run in throwaway sandboxes. One binary, three operating systems.

Name: Paul, Aquila and Priscilla made tents for a living so their real work could go on (Acts
18:3). A Tentmaker device has a day job (yours) and a second one (CI) in its spare capacity.
Tagline: **Your machines have a day job. Tentmaker gives them a second one.**

This document is the contract. Every package implements exactly the API written here; change the
contract here first, in the same PR.

## Principles

1. **Reliable over clever.** Every external call has a timeout and a backoff. Every state file is
   written atomically. Every probe that fails, fails closed (no new jobs), never open. A runner
   whose state can't be read counts as running, so nothing cuts a live job by mistake.
2. **The day job wins, and CI keeps its share.** The everyday profile is reserved for CI: it
   keeps running while the user plays a game, takes a call or loads the machine. Boost is
   opportunistic and gives way to all of those. CI drains (stops taking jobs, lets running ones
   finish) when the battery, memory or disk says so. Only hard guards stop running jobs.
3. **No single point of failure.** Any device with forge credentials is a broker. Workers ask
   every broker they can reach. If the mesh is down, a device with credentials still runs its own
   jobs. If two brokers race for one job, the loser's idle runner is deleted after `idle_ttl`.
4. **Secrets stay put.** Forge tokens live only on broker devices, in the OS secret store. A
   runner gets only a single-use grant (a GitHub JIT config), on stdin or in a 0600 file it
   deletes on read. Never argv, never an environment variable, never a log.
5. **Agents are first-class users.** Every command takes `--json` and `--yes`, exits non-zero on
   failure with a stable error code, and never prompts when stdin is not a terminal.
6. **Two channels only: stable and dev.** No betas, no nightlies, no previews.

## Vocabulary

| Term | Meaning |
| --- | --- |
| device | one machine running `tentmaker daemon` |
| pool | devices on one tailnet with the same `mesh.pool` name |
| broker | a device with forge credentials: it polls the forge and grants runners |
| worker | a device with free slots: it asks brokers for work and runs it |
| everyday / boost | the two budget profiles (always / plugged in) |
| slot | room for one job: `floor(budget / job size)` |
| drain | take no new jobs; running jobs finish |
| evict | stop running jobs now (hard guard only) |

## Layout

```
cmd/tentmaker/            main
internal/core/            shared types and interfaces (core.go): the contract
internal/config/          TOML config schema, defaults, validation, atomic writes
internal/budget/          profiles, schedule, power, idle, guards -> core.Decision
internal/power/           AC/battery and user-idle per OS; host memory/CPU totals
internal/guards/          host probes: free memory, free disk, load, process list
internal/secrets/         OS secret store for forge tokens
internal/forge/github/    GitHub Actions: queued jobs, JIT runners, fork policy, migrate
internal/forge/forgejo/   Forgejo / Gitea Actions
internal/forge/gitlab/    GitLab (checks mode only)
internal/forge/fake/      in-memory core.Forge for tests
internal/checks/          checks mode: plan workflow jobs, run them, post statuses
internal/executor/linux/  ephemeral actions/runner, own user, systemd unit, rootless podman
internal/executor/darwin/ Tart VM per job (macOS and Linux guests), Softnet isolation
internal/executor/windows/ dedicated local user, Job Object limits, fresh dir per job
internal/executor/fake/   in-memory core.Executor for tests
internal/dispatch/        Broker (forge side) and Worker (device side)
internal/mesh/            tailnet peer API: discovery, WhoIs auth, remote WorkSource, claims
internal/runs/            run history: records, log tails, redaction, stats, wait
internal/alerts/          webhooks on failure, recovery and slow jobs
internal/executor/joblog/ reads a job's log from its job folder without following the job's links
internal/testresults/     finds JUnit reports in a job's workspace and go test -json in its log
internal/service/         install the daemon as a systemd / launchd / Windows service
internal/update/          stable/dev channels, verified self-update with rollback
internal/httpsafe/        redirect policy that keeps credential headers on their origin
internal/binsafe/         checks that only root (SYSTEM and Administrators) can change a binary
internal/mcp/             stdio MCP server for agents
internal/cli/             commands
scripts/install.sh        Linux and macOS installer
scripts/install.ps1       Windows installer
```

## Budget (internal/budget, internal/power, internal/guards)

```go
// budget
func New(cfg config.Config, power core.PowerSource, idle core.IdleSource, probe guards.Probe) *Engine
func (e *Engine) Decide(ctx context.Context, now time.Time, running int) core.Decision // implements core.Budgeter

// power
func New() core.PowerSource          // per-OS, build tags
func NewIdle() core.IdleSource       // per-OS, best effort
func TotalMemory() (int64, error)    // physical RAM in bytes
func CPUs() int

// guards
type Probe interface {
    FreeMemory(ctx context.Context) (int64, error)          // available to new processes
    FreeDisk(ctx context.Context, path string) (int64, error)
    LoadPerCPU(ctx context.Context) (float64, error)        // 1-min load / CPUs; Windows: CPU busy fraction
    Processes(ctx context.Context) ([]string, error)        // lowercase executable names
}
func NewHostProbe(stateDir string) Probe
type Result struct { Admit, Evict, Busy bool; Reasons []string }
func Check(ctx context.Context, g config.Guards, job config.Job, p Probe, stateDir string, running int) Result
```

Decision order (first match decides the profile, later steps can only lower it):

1. Power unknown: profile `everyday` with `Admit=false` and reason "can't read power". (Fail
   closed on new work, keep running jobs.)
2. On battery and percent < `stop_below_percent`: `off`, `Evict=true`.
3. On battery and percent < `pause_below_percent`: `off`, `Admit=false`.
4. A schedule window that covers `now` sets the profile (`off`, `everyday`, `boost`). `boost`
   from a schedule still needs AC power; on battery it falls back to `on_battery`.
5. Plugged in (or no battery) and (not `boost_needs_idle` or idle >= `idle_after`): `boost`.
6. On battery: `on_battery` (`everyday` default, or `off`).
7. Otherwise `everyday`.

Then: `Limit` = the profile's resources (zero for `off`). `Slots = min(floor(Limit.Memory /
job.memory), floor(Limit.CPUs / job.cpus), job.max_slots if > 0)`. Guards run before `Limit`.
`job.cpus` is what a job reserves: it counts slots and the pool's use. It is not the job's CPU
ceiling. The worker sets `ExecSpec.CPUCeiling` (and `DebugSpec.CPUCeiling`) to `Limit.CPUs` when
the job starts (never below `job.cpus`, see `core.CPUCeiling`), so a job alone on the device gets
every CPU the pool allows; jobs that start after `Limit` drops (battery, a profile change) get the
lower ceiling. An executor honors it only where its `SetPoolLimit` caps all jobs together, which
it declares with `core.PoolCPUCapper` (`CapsPoolCPU() bool`): linux-podman does; macos-tart and
windows-job have no pool-wide cap and keep each job at `job.cpus`, or two jobs could use twice the
pool, so the worker leaves their ceiling at 0. It fails closed: the worker records, per
executor, the `Limit.CPUs` its last `SetPoolLimit` applied, and when that call failed or applied
another limit the ceiling is 0 (`job.cpus`), since nothing then caps the jobs together.
`ExecSpec.TestWorkers`, exported to the job as `TENTMAKER_JOBS`, follows the CPUs the job starts
with: `floor(core.CPUCeiling(Limits, CPUCeiling))`, at least 1. Memory does not burst, so past
`floor(job.cpus)` the number is capped at one worker per 512 MiB of `job.memory`, since an OOM
kill ends the whole job. A job alone under the pool's cap is told the pool's whole CPUs, within
that cap; with a ceiling of 0 it is told `floor(job.cpus)`. Jobs that start
while others run are told the same pool-wide number and split the pool by `CPUWeight`.
Safety guards (free memory, free disk) set `Admit=false` (and `Evict=true` for
`stop_below_disk`). Busy guards (`pause_when_running`, `max_load_per_cpu`) set `Busy`, which
lowers `boost` to `everyday` and never blocks `everyday`; a busy probe that can't be read counts
as busy. `Admit` is also false when
`running >= Slots`. Every downgrade appends a plain reason, like "on battery at 31%, below 40%" or
"steam.exe is running". A boost profile with zero memory means "same as everyday".

Power per OS: Linux reads `/sys/class/power_supply/*/{type,online,capacity}` (Mains online, or no
Battery present, means AC; a supply whose `type` can't be read, or any attribute read error other
than a missing optional file, makes power unknown, not AC; `type` and `scope` are read first, and a
supply whose scope is Device, a peripheral such as a sleeping Bluetooth mouse, is skipped along with
its read errors). macOS runs `pmset -g batt`. Windows calls `GetSystemPowerStatus`
(golang.org/x/sys/windows). Idle: macOS `ioreg -c IOHIDSystem` HIDIdleTime; Linux
`loginctl show-session` IdleHint/IdleSinceHint for the active seat session; Windows: a daemon in
session 0 can't see input, so it returns an error ("not idle") unless the user-mode tray helper
reports (later). Errors always mean "not idle", never "idle".

## Forges (internal/forge/...)

Every forge implements `core.Forge`. Constructors:

```go
// github
type Options struct {
    BaseURL string                                   // default https://api.github.com
    Token   func(ctx context.Context) (string, error)
    HTTP    *http.Client                             // default: 30 s timeout
    Now     func() time.Time
    Log     *slog.Logger
}
func New(o Options) *Client
func (c *Client) CheapPolls() bool   // core.CheapPoller: limit unknown, past its reset, or a quarter (at least 400) left
func (c *Client) PollDelay() time.Duration // core.PollPacer: 0 while cheap; else the rest spread to the reset, at most 1 min
func (c *Client) APIStats() core.APIStats  // core.APICounter: requests, 304s, billed, rate limited since start
type RateLimitError = core.RateLimitError       // Forge "github"; matches core.ErrRateLimited
func RateLimitWait(status int, h http.Header, body []byte, now time.Time) (time.Duration, bool) // is a 403/429 a rate limit, and how long to wait
func (c *Client) SetForkApproval(ctx context.Context, repo string) error       // require approval for all outside contributors (public) / disable fork PR workflows (private)
func (c *Client) SetReadOnlyToken(ctx context.Context, repo string) error      // default GITHUB_TOKEN read-only
func MigrateWorkflow(src []byte, platform string) (out []byte, report []string, changed bool, err error)
func Migrate(src []byte, platform string, o MigrateOptions) (MigrateResult, error) // MigrateWorkflow plus options and Moves
type MigrateOptions struct { All bool }                                         // All: no fork-safe expression
type Move struct { Line int; Job, Before, After string }                        // one rewritten runs-on
type MigrateResult struct { Out []byte; Report []string; Changed bool; Moves []Move }
func ForkSafeRunsOn(hosted, platform string) string                            // the fork-safe runs-on for one hosted image
func (c *Client) OpenOrUpdatePR(ctx context.Context, pr core.PullRequest) (core.PullRequestResult, error) // core.PullRequester
func (c *Client) FirstJob(ctx context.Context, repo string) (core.FirstJob, error)                       // core.FirstJobReporter
func TokenFromGH(ctx context.Context) (string, error)                          // `gh auth token`

// forgejo (also Gitea)
func New(o Options) *Client

// core: rate limits, shared by every forge
type RateLimitError struct { Forge string; Until time.Time; Err error } // matches ErrRateLimited, unwraps to Err
type RateLimited struct { Message, Fix string; Until time.Time; Wait time.Duration }
func (RateLimited) RetryAfterS() int
func AsRateLimited(err error, now time.Time, again string) (RateLimited, bool)
```

Rate limit or permission (issue #23). A forge that refuses a call for its rate limit is not
refusing the token: the token works again after the reset. So no forge client, CLI command or
doctor check reports a rate limit as a token or permission problem, and no fix for one says to
make a new token. GitHub refuses with 403 or 429: `X-RateLimit-Remaining: 0` is the primary limit
(it resets at `X-RateLimit-Reset`); `Retry-After`, or only "rate limit" in the message (then at
least a minute), is a secondary limit; any other 403 is a permission refusal. GitLab and Forgejo
refuse with 429 (`Retry-After`, or GitLab's `RateLimit-Reset`). Each client returns a
`*core.RateLimitError` for these, also when a short wait is still a rate limit after every try.
`AsRateLimited` turns one into the words a person or agent reads: "GitHub rate limit reached for
this token; it resets at 05:18 UTC (in 12 min)" (to the second under 5 minutes, else to the minute
rounded up, so waiting until it is never early), a fix that says the token is fine, to wait until
then and run it again, and to use a token not shared with other workflows if it keeps happening,
and the wait in seconds. `CheckRepo` returns a rate limit as its error, never as a `Problems`
entry, since a missing permission it can't read is not one it lacks. A rate limited call adds no
work: the broker polls again after the reset (fail closed).

GitHub details ported from fleet-ci (`fleet_ci.py` class `GitHub`): ETag conditional GETs (a 304
costs no rate limit), list runs with `status=queued` and `status=in_progress` (both at once),
refetch jobs of an in-progress run at most every 15 s unless the run changed, ignore runs older
than 24 h, `POST /repos/{o}/{r}/actions/runners/generate-jitconfig` with `runner_group_id` 1,
delete runners, cancel runs. A job's `FromFork` is true when the run's `head_repository.full_name`
differs from the repo. Retries: 3 tries with jittered backoff on 5xx and network errors; never
retry a 4xx except 429/403-rate-limit. A 403 a caller expects (a rerun of a run still going) is
never matched by a rate limited 403. Forgejo sleeps through a 429 of at most 5 s
(`forgejo.InlineWait`) and fails a longer one at once with the reset, like GitHub.

Rate limit (issue #23). The client never sleeps toward a reset; that held the broker's tick and
stopped every pickup for up to an hour when five devices shared one workflow token. A rate limit
answer whose wait is at most 5 s (`InlineWait`) is slept through and retried. A longer one fails
the call at once with a `RateLimitError` (it matches `core.ErrRateLimited`), and so does every call
until the wait ends or, when the primary limit reads 0, until its reset, without a request. Below
a quarter of the limit (at least 400), `PollDelay` paces polls: what is left above `RateFloor`
(200, kept for status posts, grants and deletes), spread over the time to the reset at 4 billed
requests a poll, at most `PacedMax` (1 min) and never past the reset. `APIStats` counts every
request, the 304s and the rate limited calls; `tentmaker status --json` shows them as
`broker.api.<forge>`.

`PendingCommits` reads the repo object for its default branch at most every 10 minutes
(`DefaultBranchTTL`): its ETag changes on every push to any branch, so reading it each poll billed
one request a poll in a busy repo. The PR list and every branch head are read at once.

Optional forge interfaces in core, which only the GitHub client implements:

```go
type PullRequest struct { Repo, Head, Base, Title, Body string }   // Base "" = default branch
type PullRequestResult struct { Number int; URL string; Created bool }
type PullRequester interface {
    OpenOrUpdatePR(ctx context.Context, pr PullRequest) (PullRequestResult, error)
}
type FirstJob struct {
    Online       []string  // online runners labeled tentmaker
    RanJob, RanOn string   // the newest job that ran on one, and its runner
    RanAt        time.Time
    Queued       int       // jobs labeled tentmaker with no runner yet
    OldestQueued time.Time
}
type FirstJobReporter interface {
    FirstJob(ctx context.Context, repo string) (FirstJob, error)
}
```

`OpenOrUpdatePR` lists open pulls with `head=<owner>:<Head>` and `base`; it PATCHes the first one's
title and body, or POSTs a new one. `FirstJob` reads `GET /actions/runners` and the jobs of the
newest five runs.

Migrate also leaves alone, with a reason in the report: a job with a job-level `services:` on
macOS or Windows (no Docker there), a job-level `container:` on Windows, and a runs-on label that
asks for a GPU or a larger runner (`gpu`, `cuda`, `nvidia`, `large`, `xlarge`, `N-cores`) unless
the list already says `self-hosted`. With `All`, a moved job gets `[self-hosted, tentmaker, <os>]`
(a matrix keeps its other legs) and fork pull requests go to the pool, where they wait, since the
broker never runs a fork's job.

## Executors (internal/executor/...)

Every executor implements `core.Executor`. Constructors take an `Options` with `StateDir`,
`CacheDir`, `Log`, plus their own fields. `internal/executor/host.go` has
`func ForHost(cfg config.Config, log *slog.Logger) ([]core.Executor, []core.Problem)` choosing by
`runtime.GOOS`. Each package also has `func Install(ctx, Options) error` and `Uninstall(ctx,
Options, purge bool) error` that set up the host (run as root/Administrator by `tentmaker
setup`), idempotent and safe to rerun. `Options.Phase func(name string)`, when set, is called as
`Install` starts each of its phases; setup uses it for the `sandbox.<phase>` rows of its
`timings`. On Linux the actions/runner download and extract run beside the phases after
`runner`, and `Install` waits for them in `runner.wait`, before it returns; an error there stops
`Install` with the same error as before. Linux `Install` proves rootless podman works with
`podman ps --all --quiet` as the runner (phase `podman.check`), which sets up podman's runtime
(the pause process's user namespace, the store, the state database, conmon and the OCI runtime)
and fails `Install` when any of it fails. Not `podman info`, which also runs `rpm -q -f` and
`dpkg -S` for each helper binary only to print its package, about 5 s on GitHub's runners.

- **linux-podman** (port of `fleet-ci-job`, `install.sh`): user `tentmaker-runner` with no sudo
  and no docker group; a root-owned runner template (actions/runner pinned version and sha256);
  per job a fresh copy (`externals` a symlink and `bin/` hard links to the template, never
  chowned; top-level files and any file the CI user could write are copied), own HOME, a transient systemd unit in `tentmaker.slice` with `MemoryMax`
  = job memory, `CPUQuota` = `ExecSpec.CPUCeiling` (the pool's CPU limit at start), `CPUWeight`
  100 on every job so jobs that share the pool split it evenly, Nice 10, idle IO; rootless podman socket for container
  jobs with `DOCKER_HOST` and `DOCKER_BUILDKIT=0` (podman serves docker's classic build API,
  building with buildah in its own store, but not BuildKit's) (the runner's manager gets its own `XDG_CONFIG_HOME` and
  `XDG_RUNTIME_DIR` from a root-owned `environment.d/zz-tentmaker.conf`, which sorts after
  `/etc/environment`'s `99-environment.conf`: GitHub's runners name uid 1001's dirs there, and
  podman's service then fails to start); the slice's own `MemoryMax`/`CPUQuota` = the pool limit
  (`SetPoolLimit`); `Reset` restarts the runner user's manager and wipes containers. Platforms:
  `linux/<arch>`.
- **macos-tart** (port of `TartLauncher`): `tart clone` a digest-pinned image per job, `tart set
  --memory --cpu` = job limits, `tart run --no-graphics --net-softnet` with the Softnet block
  rules from fleet-ci, `tart exec -i` hands the JIT config on stdin, always `tart stop` + `tart
  delete`. Images: `macos/arm64` (cirruslabs macos-*-xcode) and `linux/arm64` (cirruslabs
  ubuntu runner image), each pinned by digest in config defaults. `MaxSlots` 2 (Apple's limit on
  macOS guests). Boot failures back off 1, 2, 4 min up to 1 h.
- **windows-job**: local standard user `tentmaker-runner` (random password kept with DPAPI,
  machine scope, file ACL SYSTEM and Administrators only); per job a fresh runner copy under
  `%ProgramData%\Tentmaker\jobs\<name>` ACLed to that user. Setup gives `%ProgramData%\Tentmaker`
  and the state folder a DACL of their own (SYSTEM and Administrators full, Users read): under
  ProgramData's inherited one any user, a job too, could add files to the state, checks and logs
  folders. The runner starts with
  `CreateProcessAsUser`/`LogonUser` inside a Job Object with `JOB_OBJECT_LIMIT_JOB_MEMORY`,
  CPU rate control and `KILL_ON_JOB_CLOSE`. Platforms: `windows/<arch>`. Linux jobs on Windows
  (WSL2) are roadmap, not v1.

**Repo caches (Linux, `internal/repocache`, `internal/executor/linux/cache.go`).** A job whose
`ExecSpec.Cache` names a repo gets that repo's persistent cache and a stable folder; a job without
one runs cold at `jobs/<name>` as before.

- Store: `<cache_dir>/repos/<key>/`, where key is the first 128 bits of the SHA-256 of the
  lowercased forge and repo, so names never nest. `repos/` is the daemon's (root) with mode 0700,
  checked on every use (a root owned by anyone else is refused); a repo's folder belongs to the CI
  user. `index.json` (written with `config.WriteAtomic`) keeps each repo's size and last use.
- Mount: the job's unit gets `BindPaths=<store>/<key>:<state>/linux/repo-cache`, so only its own
  mount namespace sees it, and `GOCACHE`, `GOMODCACHE`, `npm_config_cache`, `PIP_CACHE_DIR`,
  `CARGO_HOME` and `XDG_CACHE_HOME` point below that one fixed path. On the host `repo-cache` is
  an empty root-owned folder. Container steps run in podman.service's mount namespace and don't
  see the bind, so `RUNNER_TOOL_CACHE` and `AGENT_TOOLSDIRECTORY`, which the runner binds into job
  containers, stay at the slot's `toolcache`, a host path that is warm because the slot is.
- Container store (`store.go`): podman's images, layers and build cache for the repo, so
  `docker build` and container steps start warm and base image pulls hit the disk. It lives at
  `<cache_dir>/repos/.containers/<key>/storage`, which belongs to the CI user; `.containers/` and
  `<key>/` are root's with mode 0700, checked on every use, so a job can't reach another repo's
  store by path or swap a folder above it for a symlink (a runner-owned parent would let a
  leftover process point the next bind at another repo's store). Rootless podman is one per
  device (the podman namespace below), so the store is bound to the device, not to a job: when
  a job with a repo cache starts on an idle device (no other job starting or running), `Start`
  writes `<state>/linux-private/container-store.json` (the repo, with `config.WriteAtomic`),
  restarts podman's namespaces with `BindPaths=<store>:<state>/linux/container-store` in the
  user manager, `tentmaker-podns.service` and the job unit, and writes the runner's
  `~/.config/containers/storage.conf` (root's folder) naming that path as podman's graphroot,
  with a runroot of its own. Each switch, bind or unbind, also deletes podman's `alive` and
  `pause.pid` files in the runtime dir, as a reboot would: podman 4.9 with no `alive` file joins
  the pid in `pause.pid` without checking it and fails on a stale one. Then it checks that `podman info` reports that graphroot, both from podman's CLI and from
  the service behind `DOCKER_HOST` (`podman --remote` on the socket), and removes
  every container, pod, volume and network a past job left in the store; images stay. Any of
  those steps failing, a prune included, counts as a failed bind. Only then is the record
  rewritten as verified, with the boot id (`/proc/sys/kernel/random/boot_id`). A record that is
  not verified in this boot (a bind in progress, a bind whose way back failed, a reboot, an
  unreadable record) means podman's state is unknown: every `Start`, cold jobs too, first starts
  podman unbound when the device is idle and fails when it can't, so no job runs until an
  unbind or a reset succeeds. The job
  gets `CONTAINERS_STORAGE_CONF` so its own `podman` uses the same store. Later jobs of the repo
  use the bound store; `Reset` (when nothing runs) starts podman unbound again and only then
  wipes the default store. A job that finds the device busy, or a bind that fails, runs without
  it (a warning); a bind whose podman can't be brought back unbound fails the job's start. On
  the host `container-store` is an empty root-owned folder.
- Container store threat model. Who can write it: only jobs vouched safe for the repo (the same
  rule as the repo cache), and while it is bound any process of the CI user can reach it through
  `/proc/<pause pid>/root`, so the worker keeps the device to that repo's trusted jobs from the
  first cached job until the next reset (`WorkRequest.OnlyRepo`, `state.json` `held_cache`). A
  poisoned image or layer (a tampered base image, a build step that writes a backdoor into a
  cached layer) can therefore affect only later trusted jobs of the same repo, which could
  already push code to it and so poison its builds anyway. Forks, other repos and untrusted jobs
  never run while it is bound and never bind it. The store holds no secret Tentmaker put there;
  a build secret a workflow bakes into a layer stays in that repo's store, as it would on any
  builder with a layer cache. A digest in `FROM` doesn't protect against that writer: podman
  trusts its own store and doesn't hash a cached image's layers again on use. Rejected: a per-job store copy (copying gigabytes per job loses the speed), a
  store shared across repos (one repo could poison another's base images), and a store that
  stays bound across other repos' jobs (their processes could read it through `/proc`).
- Slots: the job folder is `jobs/<key[:12]>-<n>`: the slot the same job name (`CacheRepo.Job`)
  used last time when it is free (jobs of a commit start together and would race for slot 0;
  `<state>/linux-private/slot-affinity/` remembers), else the lowest free one no other job of the
  repo last used, else the lowest free one. Go
  keys build cache entries by source folder and test results by the env tests read (`TMPDIR`,
  `HOME`), so a new folder per job rebuilt the repo's own packages every time (measured: `go test
  -race` of this repo, 0.3 s at the same folder, 48 s at a new one). The runner name to slot record
  is `<state>/linux-private/slots/<name>.json` (root only), written before the folder exists and
  removed after it is gone; `List` reports jobs by name, and a slot folder no record claims by its
  own name so it gets cleaned. `Reset` drops every slot and keeps the caches.
- Cap: `cache_max` (default 10 GiB, 0 turns caches off). After each job's `Clean` the store
  measures that repo (its cache folder and its container store), so images count against the
  cap like the cache folders do. Until the total fits it deletes container stores first, least
  recently used first (images can be pulled again, so a repo's own store never pushes its build
  cache out), then whole repos, both folders, least recently used first; a repo with a slot record or a bound
  container store is never deleted, and when either can't be read nothing is. `Reset` evicts
  again once the store is unbound.
- A cache that can't be prepared leaves the job cold (a warning), never failed.
- Who gets one: see `WorkOffer.Cache` under Dispatch. The cache is per repo, not per branch: any
  job of the repo may write what a later job of it reads.
- One repo per device while a cache is held: every job runs as one user, so a job beside a cached
  job could reach its cache through `/proc/<pid>/root`, `cwd` or `fd`. The worker gives a job its
  cache only when every job already on the device is vouched safe for that same repo, and from
  then until the next reset (the container store may stay bound that long) it declines any
  other offer. See `WorkRequest.OnlyRepo` under Dispatch. A job can
  also leave a process behind (`systemd-run --user`, a detached container) that only the node
  reset kills, so the worker keeps `dirty_trust` in `state.json`: the trust key every job since the
  last reset had. A cache goes only to that repo, or to any repo on a clean node; after another
  repo's job, an untrusted job, a restart or a failed cleanup, jobs run cold until the reset.
- macOS (tart) jobs get no cache yet: the executor has no folder share into a guest; `tart run
  --dir` is the next step. Windows jobs get none: every job runs as the same user with no mount
  namespace, so one repo's cache folder can't be kept from another repo's job.

**Actions cache (Linux, `internal/actionscache`, `internal/executor/linux/patch.go`).** A runner
job from github.com that may use its repo cache (Dispatch) also gets a GitHub Actions cache on the
device, so `actions/cache@v4` and the setup-* actions' `cache:` option save and restore at local
disk speed with no workflow change. Any step of it that fails leaves the job on GitHub's cache.

- Protocol: the toolkit's cache v2, JSON Twirp at
  `/twirp/github.actions.results.api.v1.CacheService/` (`CreateCacheEntry`,
  `FinalizeCacheEntryUpload`, `GetCacheEntryDownloadURL`), plus the subset of Azure Blob the
  toolkit's SDK uses: a whole `PUT`, or `comp=block` blocks then `comp=blocklist`, and a plain
  `GET` or `HEAD` with `Range` or `x-ms-range` for the download. Every other `/twirp/` path (the
  artifact and results services that read `ACTIONS_RESULTS_URL` too) is reverse proxied to
  `results-receiver.actions.githubusercontent.com` with the job's own headers, under a timeout.
- Listener: `daemon.Options.CacheAddr`, default 127.0.0.1:7436 (`control.DefaultCacheAddr`, `-`
  serves none). It answers only a loopback `Host`, and has its own listener, apart from the
  control API and the relay. Linux only: the other executors cannot name a job by its connection.
- Pointing the runner at it: the worker sets `ExecSpec.ActionsCache` to the service URL, and the
  executor puts `ACTIONS_RESULTS_URL`, `ACTIONS_CACHE_URL` (both that URL, checked to be plain
  http to a loopback address) and `ACTIONS_CACHE_SERVICE_V2=True` in the job's `.env`. The
  runner's worker would overwrite the first two from the job message, so the job runs a patched
  `Runner.Worker.dll` that sets `ACTIONS_RESULTS_ORL` and `ACTIONS_CACHE_ORL` instead: two
  same-length renames in the UTF-16 string heap, each name present exactly once and its new name
  absent. The executor patches only a dll whose sha256 is the pinned original for the pinned
  runner and arch, checks the result against the pinned patched sha256, and keeps the copy at
  `<lib>/runner-cache/<version>-<arch>/`; the template stays as shipped. A new runner without new
  pins, a different dll or a failed patch logs once and every job keeps GitHub's cache. The docker
  shim gives containers the real URLs back (`-e ACTIONS_RESULTS_URL` from `ACTIONS_RESULTS_ORL`,
  and the same for the cache), so container actions keep working.
- Who is calling: no secret goes in `.env`. The service names the job behind each connection: it
  finds the client socket's inode in `/proc/net/tcp` (and tcp6), and the process holding it must
  be in the job's uplink unit, `tentmaker.slice/tentmaker-uplink-<name>.service` (`cgroup.procs`,
  then `/proc/<pid>/fd`). A job's connections all leave through its own pasta, so its unit is the
  job's identity; a connection no job's uplink holds (the host, the podman namespace, another
  user) gets 404 on every path, the proxy included. The answer is cached per connection.
- Hold: jobs share the runner uid, so one can pass as another. A job writes only while every
  other job on the device is a confirmed writer of its repo and ref (`Service.Hold`, re-applied
  at job start, confirm, end, failed boot and restart; checked through commit).
- Capability: `Grant` mints 32 random bytes per job and the service puts them in the `sig` query
  of every upload and download URL it returns. A blob request needs a `sig` that names a live
  grant and that grant's job must be the connection's. `Revoke` at job end (and on a failed boot)
  drops it, so a URL from an ended job is 404. Neither the capability nor a URL holding it is ever
  logged or put in an error.
- Scopes: entries carry the ref they were saved from, `refs/heads/<branch>` for a push and
  `refs/pull/head/<branch>` for a pull_request event. Restore looks in the job's own ref, then the
  repo's default branch (`WorkOffer.Actions.DefaultBranch`), each by exact key then by each restore
  key as a prefix, newest first, and only for the version asked for. A save goes only to the job's
  own ref, so a PR reads main and never writes it.
- Confirmation: a JIT runner takes whichever queued job of the repo matches its labels, so the
  job it was granted for proves nothing, and the runtime token's `ac` claim can't be trusted (its
  signature can't be checked on the device, so a job can forge any claim; the service ignores the
  token). A binding starts unconfirmed: it reads only the default branch and every create and
  finalize is refused as a "cache write denied" save, which the toolkit warns about and the step
  passes. The broker already identifies the job each busy runner took (its forge token, a timeout
  per call, a retry every monitor pass; see the runner identification under Dispatch), and sends
  it with every work response while the runner runs as `WorkResponse.Confirm` (runner, the run's
  event and head branch), over the same channel as grants, so a peer device gets it too. The
  worker applies it once (`actionscache.Binding.Confirm`), records it in `jobRec.Actions`
  (`confirmed`, the narrowed ref and access) and grants again. Write survives only when the job is
  not `pull_request` or `pull_request_target` and its scope is the bound ref. Any other job keeps
  at most read, of its own scope (a PR's `refs/pull/head/<branch>`) and the default branch, for
  good (review first-F1).
- Entries are immutable: a key and version that exist, or are reserved by an upload in flight,
  are refused with `ok:false` and a "cache write denied" message, which the toolkit logs as a
  warning without failing the step. An entry is committed only at `FinalizeCacheEntryUpload`,
  after its size (and, for blocks, the assembled stream's sha256) checks out and the write right
  is checked again. Uploads are capped per entry (`MaxEntry`) and per job (`MaxJob`).
- Storage: under the repo cache, `<cache_dir>/repos/<key>/.tentmaker-actions/` (root, 0700,
  owner and mode checked on every open through `os.Root`). The job's own bind of the repo folder
  shows it as a root-owned folder it cannot open. `index.json` is written with
  `config.WriteAtomic`; a corrupt index reads as empty. Uploads stream to `u-<id>` files, entries
  are `e-<id>`; a sweep under the repo lock removes staging and orphans older than 6 hours. The
  repo cache's LRU cap covers these files too, but it drops a whole repo folder, so the entries
  have a budget, min(`cache_max`/2, 10 GiB), that caps `MaxJob` too. A commit that would pass it
  evicts the repo's least recently used entries (last save or restore, which a lookup records in
  memory and the next commit writes to `index.json`) until the new entry fits, all under the repo
  lock, so the entries never hold more than the budget. Eviction never takes an entry with a
  download in flight (counted from the index read under the repo lock until the file closes) or
  one a lookup matched in the last minute. When the rest can't make room, commit refuses the
  write (cache write denied), and create refuses up front when those pinned entries already fill
  the budget (review first-F1).
- Speed: uploads stream to disk through a pooled 1 MiB buffer with sha256 computed on the way,
  and downloads are `http.ServeContent` on the open file (sendfile, `Range`). Benchmark
  (`BenchmarkSaveRestore256MiB`, 256 MiB, i9-14900HX, tmpfs, loopback, 3 runs of 5): save
  1.15 to 1.18 GB/s, restore 4.9 to 5.5 GB/s.
- Not yet: macOS (tart) and Windows jobs keep GitHub's cache (future work: the tart guest needs a
  forwarded port and a per-VM identity, and Windows jobs share one user and the host's network
  stack, so a connection names no job). Container steps keep GitHub's cache too: their
  connections come through the podman uplink, which names no job.

**Network isolation.** Install fences the runner user off and every `Start` checks the fence
first; no rules means no job (fail closed). A Linux job also gets a network namespace of its own
(below); a Windows job shares the host's network stack. Jobs may reach the internet, the DNS
servers the host is configured with, the source archive relay on 127.0.0.1:7435 and, on Linux,
the Actions cache on 127.0.0.1:7436. Always blocked: the tailnet (100.64.0.0/10, fd7a:115c:a1e0::/48), link-local
(169.254/16, fe80::/10), unique local (fc00::/7), loopback and this host's own addresses, and port
7433 on any address. RFC 1918 (10/8, 172.16/12, 192.168/16) and, on Linux, the host's on-link
global IPv6 prefixes are the LAN, blocked unless `[guards] allow_lan`; `allow_lan` means the
physical LAN only, never what the tailnet routes.

- Linux job namespace (`netns.go`): each job runs in a network namespace of its own, so its own
  127.0.0.1 and ::1 work (httptest, a test database) and the host's loopback does not. Per job,
  two transient units in `tentmaker.slice` run as the runner user: `tentmaker-netns-<name>`
  (`unshare --user --map-root-user --net`, holding a user namespace the runner owns and a network
  namespace in it; the unit grants it CAP_SYS_ADMIN alone, because where AppArmor restricts
  unprivileged user namespaces, as Ubuntu does by default, unshare cannot write its uid_map
  without it) and `tentmaker-uplink-<name>` (pasta, `Type=forking`, joining the holder's
  namespaces by pid). pasta must run as the runner: it opens every outside socket, and the per-uid
  rules below match them; pasta started as root and dropping privileges cannot join a namespace
  owned by the initial user namespace, hence the runner-owned holder. pasta forwards only the
  relay and the Actions cache (`-T 7435,7436`: the job's own 127.0.0.1:7435 and :7436 reach the
  host's), maps no other host loopback
  (`--no-map-gw`) and publishes no job port (`-t none -u none`). pasta copies the host's addresses
  and routes, so a host with no IPv6 default route gives its jobs no IPv6 at all. `Start` waits until the holder's
  process has left the host's namespace (`/proc/<pid>/ns/net` differs from the daemon's), starts
  pasta, then the job unit with `NetworkNamespacePath=/proc/<pid>/ns/net` and
  `BindReadOnlyPaths=<private>/resolv.conf:/etc/resolv.conf`, which names the host's resolvers or,
  behind the 127.0.0.53 stub, `/run/systemd/resolve/resolv.conf`'s upstream servers (also added
  to the DNS holes). Any step that fails stops both units and refuses the job; setup and
  `Preflight` fail without pasta or unshare, or with no DNS server a job can reach. `Stop`,
  `Clean` and `Reset` stop the units. The namespace is isolation; the rules below are the policy.
  A job owns its namespace's user namespace, so it can reconfigure its own namespace; everything
  still leaves through pasta as the runner.
- Linux podman namespace (`podns.go`): rootless podman runs every command, a job's included, in
  the user and mount namespaces of its pause process, not the caller's. It never joins the pause
  process's network namespace: a host-network container (`docker run --network=host` through
  `DOCKER_HOST`) runs in podman.service's, which it gets from the runner's user manager. So podman
  gets a network namespace of its own, made the way a job's is: `tentmaker-podnet.service` (the
  holder) and `tentmaker-podnet-uplink.service` (pasta, the same argv as a job's, so the per-uid
  rules match all it sends and only the relay and the Actions cache port are forwarded to the
  host; the cache answers a container's connection 404, since it comes from no job's uplink). `Install` and `Reset`
  stop the user manager and its runtime dir, start those two, write the runtime drop-in
  `/run/systemd/system/user@<uid>.service.d/60-tentmaker-netns.conf` with
  `NetworkNamespacePath=/proc/<holder>/ns/net` (under `/run`, so no pid outlives a reboot), run
  `daemon-reload` and start the user manager in it. Then, before any podman command, they start
  `tentmaker-podns.service` (the runner, `podman unshare sleep infinity`, the same
  `NetworkNamespacePath`, the job unit's mounts: its resolv.conf and every `InaccessiblePaths`; no
  `NoNewPrivileges`, which newuidmap needs) and wait until `$XDG_RUNTIME_DIR/libpod/tmp/pause.pid`
  names a process outside the host's user namespace. So the pause process is born with a job's
  view: no user bus or manager socket (a process started through them would run outside the job's
  unit and network namespace), no tailscaled socket, and DNS servers the job's namespace can
  reach. A host-network container can then reach no host service's abstract Unix socket and
  listen on none of the host's addresses. This namespace is one per device, not one per job: the
  containers every job starts through the service share it (and its loopback), as they share
  the service. Its three units run as the runner from `Install` on, between jobs too, so they
  are not a job's leftovers; the failures scenario tells them apart by unit cgroup, never by
  argv (`sleep infinity` is also what a leak looks like). A job's own `podman` CLI keeps the
  job's namespace. A repo's container store (Repo caches above) is bound by the same restart,
  with one more `BindPaths` in the manager drop-in, `tentmaker-podns.service` and the job unit. `Reset` fails unless the pause
  process and the user manager are both outside the host's network namespace
  (`/proc/<pid>/ns/net` differs from the daemon's), then checks the mounts through `podman
  unshare` and fails on any difference. Doctor (`Preflight`) reports either one in the host's
  namespace, as after a reboot until the daemon's first reset. Without the bus the systemd cgroup manager cannot work
  (crun would ask the system bus, which refuses), so setup writes the runner's
  `~/.config/containers/containers.conf` (root-owned) with `cgroup_manager = "cgroupfs"`, and jobs
  load it through `CONTAINERS_CONF_OVERRIDE`. A job's containers then stay in the job's cgroup.
- Linux (`netrules.go`): nftables table `inet tentmaker`, an output hook that sends every packet
  with `meta skuid { <runner uid>, <its subuid ranges> }` to a `runner` chain (accept the relay,
  the Actions cache on 127.0.0.1:7436 and DNS, then reject; rules without the cache port are stale
  and reload). The subordinate uids are a second wall: a rootless container on the host network
  that runs as a non-root user opens its sockets as one of them, and none should share the host's
  namespace (the podman namespace above). The ranges are the runner's `/etc/subuid` entries keyed
  by its name or its numeric uid (newuidmap takes either). With none, or a file that can't be
  read, no rules load: setup fails, `Start` refuses the job and doctor says why. `Reset` also
  fails when the pause process's `/proc/<pid>/uid_map` maps a uid outside the runner's uid and
  those ranges (newuidmap can take ranges from an nsswitch `subid` source the rules never read). `oifname "lo"` covers the host's own addresses. Always rejected besides the ranges above: RFC 1918, CGNAT and ULA out a Tailscale
  interface (other devices' subnet routes; public destinations stay open for an exit node), IPv4
  multicast and broadcast (224/4, 255.255.255.255) and ff00::/8. Tailscale interfaces are
  `tailscale0` plus any interface holding a tailnet IPv6 address (`/proc/net/if_inet6`); the
  on-link prefixes are `/proc/net/ipv6_route` routes in 2000::/3 with no gateway, outside loopback
  and Tailscale, leaving out the host's own and anycast addresses (`RTF_LOCAL`, `RTF_ANYCAST`; the
  kernel lists them on the interface that holds them) and any prefix inside another (nft merges
  it, so the listing would never show it). With no `nft`, the same rules as an iptables and ip6tables `TENTMAKER` chain with
  one `--uid-owner` jump per uid range. Rootless podman's pasta and slirp4netns run as the runner
  user, so container traffic matches too. The rules file lives in `<lib>/net.nft` (or
  `net-iptables.sh`); `tentmaker-net.service` (oneshot, before `network-pre.target` and
  `tentmaker.service`) loads it at boot. `checkNet` compares the loaded rules element by element
  (nft lists sets sorted and single addresses without a length). A missing table, chain or jump
  for the runner's uid refuses the job. Loaded rules that lack a piece (a new subuid range,
  Tailscale interface or on-link prefix, or a rule an older version did not write) are stale, and
  so are rules whose DNS servers are not exactly the host's current ones (a new DHCP resolver):
  `Start` rewrites the file and the job resolv.conf from the host's current facts, reloads the
  rules and checks again.
- Linux job units also get `InaccessiblePaths=-` for tailscaled's `/run/tailscale`,
  `/var/run/tailscale` and `/var/lib/tailscale` (its LocalAPI socket is mode 0666, and firewall
  rules don't see Unix sockets), and for `/home` and `/root` unless the job dir is under them.
  Not `ProtectHome=yes`: it also hides `/run/user`, the job's runtime dir and podman socket. The
  runner's `user@<uid>.service` drop-in hides the same tailscaled paths from the podman service and
  its containers and orders the manager `After=tailscaled.service`.
- Windows (`netrules.go`, `wfp.go`): two layers, both scoped to the runner SID.
  - Windows Firewall outbound rules in group "Tentmaker" with `LocalUser` `D:(A;;CC;;;<runner
    SID>)`, made with the NetSecurity cmdlets through `API.PowerShell`, for traffic to other hosts.
    Block rules beat allow rules, so the blocked ranges leave out 127.0.0.1 and any resolver inside
    them, and those addresses get block rules for every port but the allowed one. Windows Firewall
    refuses an IPv6 loopback, unspecified or multicast remote address (and with it the whole
    rule), so the rules leave out ::1 and any such resolver. Every rule's description names the
    rules version, the runner SID and `allow_lan`, and every rule's name ends in a hash of its
    content and that description. Setup adds each wanted rule that isn't there by name and only
    then removes the group's other rules, so a rerun never unblocks anything in between.
  - Windows Firewall does not filter traffic to the host itself: a live run reached decoys on
    127.0.0.1, ::1 and the host's own addresses through loaded, enabled block rules. So Tentmaker
    adds persistent WFP filters (`API.SetWFP`, fwpuclnt.dll through `x/sys/windows`, no cgo) in its
    own provider and a sublayer of weight 0xffff, at `FWPM_LAYER_ALE_AUTH_CONNECT_V4` and `_V6`.
    Every filter matches `FWPM_CONDITION_ALE_USER_ID` (the same SDDL) and the
    `FWP_CONDITION_FLAG_IS_LOOPBACK` flag, which Windows sets on traffic to any of its own
    addresses: permit TCP 127.0.0.1:7435 (the relay) and port 53 to the configured DNS servers,
    then block the rest with `FWPM_FILTER_FLAG_CLEAR_ACTION_RIGHT`. Two more hard blocks drop the
    loopback flag and take multicast and broadcast to any host (224.0.0.0/4 and 255.255.255.255
    as `FWP_V4_ADDR_MASK`, ff00::/8 as `FWP_V6_ADDR_MASK`), which the firewall rules don't, and
    which a live run reached. A live run proved the loopback blocks (127.0.0.1, ::1, wildcard
    listeners, the mesh port, the host's own addresses). The block also stops a job from reaching
    a server it starts itself on 127.0.0.1 or ::1, and WFP's static filters can't do better: at
    connect only the connecting user is known, and at `FWPM_LAYER_ALE_AUTH_RECV_ACCEPT_V4`/`_V6`
    `FWPM_CONDITION_ALE_REMOTE_USER_ID` never matches loopback traffic (Microsoft's ALE docs: the
    remote user comes from IPsec credentials). A live probe showed it, even with an Everyone
    descriptor, while the loopback flag and the listener's `ALE_USER_ID` did match
    (`wfp_live_windows_test.go`, review r2 partS second-S2-F6). Letting a job reach its own
    listeners needs a callout driver or an AppContainer, not filters; until then the executor's `Notes` makes doctor warn about it on every Windows device. The filters have fixed keys; `SetWFP`
    deletes every Tentmaker key and adds the wanted filters in one transaction, and
    reads them back by key. Each filter's description spells out all of its conditions.
  - `checkNet` reads the profiles, the group's rules and the DNS servers (`Get-DnsClientServerAddress`,
    the command setup used) in one PowerShell call, plus the WFP filters. A firewall profile that
    is off, no rules, no filters, or a rule or filter for another SID, `allow_lan` or version
    refuses the job. The right ones with a rule or filter missing or extra (a new DNS server, or
    one deleted by hand) are stale: `Start` writes the rules and filters again and checks once
    more. When WFP refuses the filters, setup fails and so does `Start`.
- macOS: the Softnet rules already cover all of it (Softnet's default drops non-global IPv4 and all
  IPv6); `allow_lan` does nothing there.

The runner's labels: `self-hosted`, `tentmaker`, `<os>` (`linux`, `macOS`, `Windows`), `<arch>`
(`X64`, `ARM64`), `tm-<device>`, plus `config.Labels`. Workflows use
`runs-on: [self-hosted, tentmaker, linux]`.

## Dispatch (internal/dispatch)

```go
type BrokerOptions struct {
    Config  config.Config
    Forges  map[string]core.Forge            // by forge name
    Claimer core.Claimer                     // nil = no cross-broker claims
    StateDir string
    Log     *slog.Logger
    Now     func() time.Time
    OnWork  func()                           // after a poll finds work; the daemon kicks its worker
}
func NewBroker(o BrokerOptions) *Broker      // implements core.WorkSource
func (b *Broker) Run(ctx context.Context) error   // poll loop: queued jobs cache, runner monitoring
func (b *Broker) Status() BrokerStatus

type WorkerOptions struct {
    Config    config.Config
    Device    string
    Executors []core.Executor
    Budget    core.Budgeter
    Sources   func() []core.WorkSource         // local broker first, then mesh brokers
    StateDir  string
    Log       *slog.Logger
    Now       func() time.Time
}
func NewWorker(o WorkerOptions) *Worker
func (w *Worker) Run(ctx context.Context) error   // tick loop
func (w *Worker) Tick(ctx context.Context) error
func (w *Worker) Kick()                           // tick now, not at the next second; never blocks
func (w *Worker) Status() WorkerStatus
func (w *Worker) Pause(reason string); func (w *Worker) Resume()
func (w *Worker) Shutdown(ctx context.Context)    // drain idle, leave busy ones running
```

Broker: polls queued jobs per repo, keeps a cache. A forge that implements `core.CheapPoller` and
says `CheapPolls()` (GitHub: every poll is an ETag GET whose 304 costs no primary rate limit, and
a quarter of the limit, at least 400 requests, is left) is polled every second while the repo is
active (`pollHot`: a new or changed head or queued job, work waiting, or a grant of the repo not
ended, in the last 5 minutes) and every 2 s when it is idle, or 250 ms and 500 ms per configured
repo when that is more, to stay under GitHub's secondary limit of 900 requests a minute. A
`core.PollPacer` forge's `PollDelay` is the least gap in every case; when the forge says its polls
aren't cheap, the gap is `PollDelay`, at least 2 s, so a low limit slows pickups to at most a
minute and lasts until the reset instead of running out. Any other forge, a failed poll and a
refused repo back off (5 s up to 30 s, x1.5 when idle), and never poll before `PollDelay` ends.
A failed poll after a paced one starts its backoff over at 5 s: the pace (up to the hour to the
reset) is not a backoff, so a poll refused just before the reset doesn't wait out the pace again.
The broker ticks every second and a repo is due 100 ms early, so a 1 s poll doesn't slip to 2 s.
`BrokerStatus.API` holds each `core.APICounter` forge's `APIStats` by forge name.
on `Work`, matches queued jobs (oldest first) to the request: a job matches when it asks for
`tentmaker` and every label it asks for is in the worker's labels for one of its platforms;
refuses forks, denied repos (`deny_repo_patterns`), public repos without `allow_public`, and runs
whose head repo differs; claims through `Claimer`; grants a runner; records (runner, device, job,
time) in its state. It monitors every granted runner: busy on an unidentifiable job for 30 s,
idle past `idle_ttl`, or busy on a fork job means `Stop` to that worker on its next request (and
`CancelRun` for a fork), plus `DeleteRunner`. A worker silent for 2 minutes gets its runners
deleted. Never grants more runners for one job than one per worker.

Each grant records the tailnet node that asked for it: `core.CallerNode(ctx)`, the WhoIs stable
node ID the mesh puts in the context with `core.WithCallerNode`, or "" for the broker's own
device. While any grant of a device is on record, a `Work` call for that device from another node
returns `core.ErrOtherNode` and changes nothing, not even the device's silence clock, so the
holder's grants still end on schedule if it is gone. The node is saved with the grant, so the
binding survives a restart. Once the device has no grant on record, its name is free for any
node (a reinstalled device waits until its old grants end, about 2 minutes after it went
silent).

The broker identifies a busy runner's job by the `JobID` that `Runner` reports, or, when it is 0
(GitHub never reports one), by the `RunningJobs` entry whose `RunnerID` (else `RunnerName`) is
this grant's runner. A runner that ended before it was seen busy is identified by `RunningJobs`
or by the job it was granted for when that job names it; one still unknown 30 s after it ended
quarantines its device. Tests run every runner-mode case again against `fakeforge.LikeGitHub`,
since the default fake reports `JobID` and once hid that every real GitHub job was stopped or
quarantined its device (review r1 partA first-F1, sol-F1).

`WorkOffer.Cache` is the broker's word that the job may use its repo's persistent cache. Check
jobs get it: the broker picks the commit and skips forks. A runner job gets it only when the repo
is private and its fork PR workflows are off (`RepoInfo.ForkPolicy == "off"`, read at most an hour
ago): a JIT runner takes whichever queued job matches its labels, so the granted job's own
`FromFork` proves nothing about the job it runs. The worker sets `ExecSpec.Cache` (forge and repo)
only from an offer with the flag and a job that isn't from a fork; an older broker never sets it.

`WorkOffer.Actions` marks a runner offer from github.com (the forge kind is `github` and its API is
api.github.com) and carries the repo's default branch. The worker grants such a job the Actions
cache (`WorkerOptions.ActionsCache`, an `ActionsCache` interface) only when the service is
listening and the executor is a `core.ActionsCacher` whose `ActionsCacheReady()` is true:
write when the job got its repo cache (honored only once confirmed, above), and no grant otherwise,
so the job keeps GitHub's cache. A drained job gets none: the broker drains a device to hand it to
another repo, and the repo's entries are opened only to jobs that got its cache (review
second-S1). When a job of another trust starts beside one
with a grant, the grant is lowered to none (`Limit`): the cache is as private as the least trusted
job on the device, the same rule as the repo cache. The binding (refs and access, no secret) is
kept in `state.json` as `jobRec.Actions`, and a restart grants the recorded jobs again with new
capabilities. A grant that fails leaves `ExecSpec.ActionsCache` empty.

A cache is only as private as the least trusted job beside it (one user, `/proc`), so the worker
decides under its admission lock, from the jobs on the device (`jobRec.Trust`, the repo the job
was vouched safe for, and `jobRec.Cached`, both in state.json):
- While the device holds repo R's cache, an offer not vouched safe for R (another repo, a fork,
  an untrusted job of R) is declined. The device holds it from the start of a job that got it
  until the next successful reset (`held_cache` in state.json), not only while that job runs:
  R's container store stays bound to podman until the reset. A reset that fails after every
  executor keeping repo caches released them returns a `core.CacheFreedError` (Linux: after
  podman came back unbound); the hold ends then though the device stays dirty, so a later
  step that keeps failing doesn't keep the device to R. A reset that fails earlier keeps it.
- A job gets R's cache only if it is vouched safe for R, its executor is a `core.RepoCacher` that
  `CachesRepos()`, the offer isn't `Drain`, and every job on the device is vouched safe for R.
- One response with offers of more than one repo (or trusted and untrusted ones) runs them all
  cold, so none is declined for another's cache.
The worker sends `WorkRequest.OnlyRepo` while it holds a cache, and `OnlyRepoIdle` once no job
holding it runs. The broker then picks only that repo's cache-safe jobs (check jobs of the repo;
runner jobs of the repo while it is cache safe); when it skipped another repo's job that the
device could run, it marks the picks `Drain`, so they run cold, and with `OnlyRepoIdle` it picks
nothing at all, so the device empties, resets and the waiting repo gets in.

Worker: each tick (1 s, or at once after `Kick`) reads `Budget.Decide`, applies `SetPoolLimit`, reports `Running` and
`Ended`, asks sources for `Free` offers, starts each with the matching executor, honors `Stop`,
evicts on `Evict`, resets the executor when nothing runs and the node is dirty, persists
`state.json` atomically, and on start reconciles: anything `List` shows that state doesn't know
is stopped and cleaned. Boot failures back off exponentially and close the worker after 5 in a
row until restart.

A job leaves the worker's count only when its executor confirms it no longer runs: `Stop`
worked, or `State` reads exited or missing. A job whose stop failed stays in `state.json`
(counted against the slots and the memory budget, `stopping` set), and each tick retries the
stop. A runner whose stop or `Clean` failed blocks its executor: it takes nothing new until a
retry or a reset works, `status` lists it under the executor's `stuck`, and `doctor` reports it
as a `worker` problem (review r1 partA sol-F2).

`Start` runs off the tick, since a macOS VM boot or a cold image pull can take minutes
(`startTimeout`, 10 min) while the broker sweeps a worker silent for 2 minutes. The job is
written to `state.json` and holds its slot and memory from the moment it is accepted; it is
reported in `Running` only once `Start` returns, so the broker's start grace still covers it.
A stop asked for while it starts waits for `Start` to return (review r1 partA sol-F4).

## Sandboxes (internal/dispatch, internal/executor/linux)

A sandbox is a job with no forge: an agent asks for one, runs commands in it, copies files in and
out, and removes it. It reuses everything a CI job gets, so it adds no new isolation to trust.

```go
// core
const KindSandbox = "sandbox"
type SandboxExec struct { Argv []string; Dir string; Timeout time.Duration }
func (r SandboxExec) WorkDir() (string, error) // Dir cleaned; absolute or outside the work folder: error
type Sandboxer interface { // an Executor that can hold sandboxes
    Exec(ctx, name string, req SandboxExec, stdin io.Reader, stdout, stderr io.Writer) (exitCode int, err error)
}

// dispatch
func (w *Worker) StartSandbox(ctx, SandboxSpec) (SandboxInfo, error) // *NoRoomError: nothing started
func (w *Worker) ExecSandbox(ctx, name string, SandboxExec, stdin, stdout, stderr) (int, error)
func (w *Worker) StopSandbox(ctx, name, why string) error
func (w *Worker) Sandbox(name string) (SandboxInfo, error); func (w *Worker) Sandboxes() []SandboxInfo
```

**Placement.** `StartSandbox` admits a sandbox under the worker's lock with the same checks as a
job, each failing closed with the reason: not shutting down, not paused, no update hold, no reset
under way, no repo cache held, the budget decided at least once and admitting, free slots, the
memory and CPU budget, and an executor that runs the asked platform, implements
`core.Sandboxer`, is usable and has capacity. A sandbox is a `jobRec` with `Kind` "sandbox" and
`Source` "sandbox"; `exchange` skips `KindSandbox` records, so no broker is told one runs or can
stop one, whatever a source is named.

A sandbox runs whatever an agent sends, as the user every job runs as, so it can reach anything
beside it through `/proc`. While the device holds a repo's cache (`cachedRepoLocked`: a job holds
it, or one did since the last reset) no sandbox is admitted, and reserving one marks the node
dirty for no repo (`markDirtyLocked`), so no job gets a cache again until the next reset
(review r1 first-F1, sol-F1). `maybeReset` sets `resetting` under the worker's lock in the same
critical section that decides a reset is due and clears it when the reset is done; admission
refuses while it is set, so a reset never stops a sandbox it didn't know about (sol-F2). The
admission, the record and `starts.Add` share one critical section, and `starts.Done` waits until
the start's state and any cleanup are settled, so `Shutdown` never returns while a sandbox start
is in flight (sol-F4). It takes `ceil(max(memory / job memory, CPUs / job CPUs))` slots, at
least one, so a sandbox twice a job's size takes two; jobs count one each. Free slots are
`budget slots - used slots`, so CI jobs see the room sandboxes hold. The default size is one
job's. The control API, CLI and MCP tools that hand sandboxes to agents come in the changes
stacked on this one.

**Lifetime.** Every sandbox has a TTL (default 30 min, at most `[sandbox] max_ttl`, default 4 h,
at least 1 min). Each tick stops a sandbox whose `Expires` has passed. `Shutdown` stops every
sandbox, and a worker that loads a sandbox from `state.json` (a crash, a restart, or a config
reload that rebuilds the worker) stops it: a sandbox never outlives the daemon that made it.
`ExecSandbox` checks `Dir` (`WorkDir`) and caps each command's timeout at the time the sandbox
has left. A sandbox whose stop or clean fails stays counted, with its slots and memory, and each
tick stops and cleans it again until `Clean` works; it never becomes a leftover that frees them. A sandbox is no CI job: it never enters run
history (records, `runs`, stats) and is never kept for debugging.

**Linux.** `Start` with `KindSandbox` (name exactly `sbx-` and 12 hex digits, which `Start` and
`Exec` check: a dash in a slice's name nests it, so any other shape could put one sandbox's slice
under another's; no grant) makes the job dir
(no runner copy), the network namespace and uplink, and a holder unit `tentmaker-job-<name>` that
runs `sleep infinity` with the job unit's properties (user, `MemoryMax`, `CPUQuota`,
`RuntimeMaxSec` = TTL, the namespace, the job mounts) in a slice of its own,
`tentmaker-<name>.slice` under `tentmaker.slice`. `systemctl set-property --runtime` gives that
slice the sandbox's `MemoryMax`, `MemorySwapMax=0`, `CPUQuota` and `TasksMax`, so all its commands
together stay in its limits; without that nothing starts. The limits are also kept root-only at
`<state>/linux-private/sandboxes/<name>.limits` (`config.WriteAtomic`), and `Exec` refuses when
the file is missing or malformed. Each command is `systemd-run --pipe --wait --collect` as unit
`tentmaker-exec-<name>-<hex>.service` in that slice with the same properties and environment,
working directory `<job dir>/work`, or `Dir` under it (an absolute `Dir` or one that climbs out is
refused). Its argv reaches the unit base64-encoded behind
`/bin/sh -c 'eval "$(echo B64 | base64 -d)"'`: systemd before 254 expands `$VAR` in a command's
arguments and later versions don't, and the wrapper has nothing either would expand. `Exec` checks
the fence (`ensureNet`) and the namespace holder first and fails closed. If `systemd-run` ends
before the command (a cut connection, a timeout), the command's unit is stopped. A command systemd
ended for it, at its `RuntimeMaxSec` or because the sandbox stopped under it, is an error, not an
exit code. The holder can end before its commands (its `RuntimeMaxSec`, a kill from inside), so
`Stop` and `Clean` stop the whole slice and confirm with `systemctl list-units` that no
`tentmaker-exec-<name>-*` unit or the slice still runs (a list that fails counts as running)
before anything else; only then does `Clean` revert the slice's runtime drop-in, remove the limits
file, the namespace and the folder. `Reset` runs only with no job, so it stops any
`tentmaker-exec-*` unit and `tentmaker-sbx-*.slice` left, refuses while any still runs, and
reverts their drop-ins (sol-F3, first-F2).

**macOS and Windows** executors don't implement `core.Sandboxer` yet, so a request for those
OSes is refused with "can't hold sandboxes yet".

## Checks mode (internal/checks, internal/forge/gitlab)

Runner mode needs the forge's own runner (GitHub actions/runner, Forgejo act_runner). Checks mode
needs only a commit status API, so it works with GitHub (including repos whose hosted Actions
never start), Forgejo, Gitea and GitLab. It is the generalized `local-ci.nu` from Local Studio.

```go
// checks
func Plan(workflowPath string, src []byte, forge string) (jobs []core.CheckJob, skipped []Skip, err error)
type Skip struct{ Job, Reason string }
func RunBundle(ctx context.Context, b core.CheckBundle, outDir string, log io.Writer) core.CheckResult // what `tentmaker check-run` calls
func Context(job core.CheckJob) string                 // "tentmaker/<job name>"
// gitlab
func New(o Options) *Client                            // implements core.StatusForge only
```

- **Plan** reads GitHub/Forgejo workflow YAML (and `.gitlab-ci.yml` for GitLab: script jobs with
  `image:` ignored or mapped to linux) into `CheckJob`s. Supported: `run` steps, `shell` bash/sh/pwsh,
  `env` at workflow/job/step level with literal values, `working-directory`, `continue-on-error`,
  `timeout-minutes`, `runs-on` mapped to a platform (ubuntu-* -> linux, macos-* -> macos, windows-*
  -> windows, self-hosted label lists by their os label; a hosted label sets only the OS, and
  an arch comes only from an `-arm` or `-arm64` suffix or an `arm64`/`x64` label), `actions/checkout` (the archive is the
  checkout), cache actions (skipped), and setup actions mapped to `Tool` (setup-go, setup-node,
  setup-python, setup-java, mlugg/setup-zig, hustcer/setup-nu, dtolnay/rust-toolchain). Anything
  else (other `uses:`, `${{ }}` expressions outside env literals that can be resolved from `github.sha`,
  `github.ref_name`, `runner.os` and `matrix`, `services`, `container`, job-level `if`) makes Plan
  skip the job with a plain reason. A skipped job gets no status; the daemon logs the reason.
  A matrix job becomes one check job per combination, as on GitHub.
- **RunBundle** downloads the archive from the bundle's `ArchiveURL` (the worker's loopback relay,
  or a `file://` absolute path when the darwin executor copied it into the VM; no other scheme)
  into a fresh dir, strips the top-level folder, runs each step with `bash --noprofile --norc -eo
  pipefail` (or the step's shell) in its working directory with env `CI=true`, `GITHUB_SHA`,
  `GITHUB_WORKSPACE`, `RUNNER_TEMP`, `RUNNER_OS`, `TENTMAKER=1`, finds tools for `Tool` steps on PATH
  or under `<state>/tools/<tool>-<version>/` (the executor passes that folder as `TENTMAKER_TOOLS`;
  it does not download toolchains in v1; a missing tool
  fails the job with the fix), stops at the first failing step unless continue-on-error, enforces
  the job timeout, writes `result.json` and `log.txt`.
- **Broker in checks mode** polls `PendingCommits` per checks-mode repo (same-repo PR heads whose
  author is in `authors`, default the owner, plus the default-branch head and the heads of the
  repo's `branches`; never forks), plans the
  workflows at that sha via `FileAt`/`ListDir`, and offers one check job per planned job that has
  no `tentmaker/<job>` status yet. Heads are planned and read at once (at most 4 at a time), and a
  new head's statuses are read while it is planned. A head with no planned job never has its
  statuses read; a head whose planned jobs all have a status reuses the broker's copy (which its
  own posts keep current) for 15 s (`statusRecheck`), so a status another broker posts is seen
  within that; a head with a job still waiting is read every poll. It posts `pending` ("running on <device>") when granting and
  the final state from `WorkRequest.Results`. A worker sends at most 512 KiB of results (JSON) per
  request, and always one, so a backlog of 64 KiB log tails never passes the mesh's 1 MiB cap; the rest
  go in the next exchanges. Until its result goes, an ended check job is listed in `Running`, not
  `Ended`, so the broker keeps its grant and posts its real state. It also writes the result's log tail, with repo,
  sha, check, state, device and runner, to `<state>/checks/<owner>_<repo>-<sha12>-<job>.log` (mode
  0600, newest 200 kept), since the worker deletes the job dir when the job ends. A worker that goes silent for 2 minutes gets its
  pending statuses set to `error` ("device went offline") so they can be retried by a re-run.
  A check grant its worker has not reported running 15 minutes (`startGrace`, longer than the
  worker's 10-minute `startTimeout`) after it was granted never reached the worker (a lost
  response) or never started: the broker posts `error` ("never started on <device>"), drops the
  grant, which revokes its archive ref, releases its claim, and offers the check again, at most
  twice per check (memory only). A worker that starts it late runs a runner the broker doesn't
  know, which it stops (review r1 partA sol-F5). A check grant the worker reports ended without
  ever reporting it running was declined (no room, no executor, no relay yet) or failed to start;
  the worker sends no result for it, and the broker treats it the same way ("did not start on
  <device>"), from the same two retries. A check that was seen running and ends without a result
  gets a final `error` ("the job ended without a result"). A check that runs and ends between two
  exchanges still sends its result, which the broker posts first.
- **Source archive relay.** Forge tokens never leave the broker. The bundle the broker sends has an
  `ArchiveRef` (32 random bytes, hex) and no URL or header. The broker keeps, in memory only, ref
  -> (runner, granted device, forge, repo, sha) while that check is active and drops it when the
  check ends. `Broker.FetchArchive(ctx, ref, device, w)` (core.ArchiveSource) serves it only to
  the granted device on the granted tailnet node (`core.CallerNode(ctx)`), at most 3 times, by calling `StatusForge.Archive` itself and streaming the
  download (redirects followed, the header sent only to the archive's own host, 10 min, 2 GiB).
  A remote broker is reached over the mesh (`GET /v1/archive/<ref>`). The worker, for each check
  offer, mints a relay id (32 random bytes, hex) bound to (source, ref, runner), expiring with
  the job and dropped when the job ends or the worker restarts, and rewrites the bundle's
  `ArchiveURL` to `http://127.0.0.1:7435/v1/relay/<id>` (the relay listener). The daemon serves
  `GET /v1/relay/<id>` there, on loopback without a token, and nothing else (404); an unknown or
  expired id is 404. The control API serves no relay.
  Linux and Windows jobs run on the host and fetch that URL directly. A Tart VM can't reach the
  host, so the darwin executor downloads it on the host, pipes it in with `tart exec -i <vm> sh -c
  'cat > /tmp/tentmaker-src.tar.gz'`, and rewrites `ArchiveURL` to
  `file:///tmp/tentmaker-src.tar.gz`. A worker refuses a bundle that carries a header or no ref
  (an older broker that sends the forge's URL), since the job would see its credentials. The
  bundle is still a secret like a JIT config: the relay URL works for the job's life.

## Mesh (internal/mesh)

```go
type Tailscale interface {
    Self(ctx context.Context) (Peer, error)
    Peers(ctx context.Context) ([]Peer, error)
    WhoIs(ctx context.Context, remoteAddr string) (Identity, error)
}
func NewTailscaleCLI() Tailscale                 // shells out to `tailscale status --json`, `tailscale whois --json`
type Options struct {
    Device, Pool string; Port int; Members []string
    Tailscale Tailscale
    Broker    core.WorkSource                    // nil if this device has no credentials
    Status    func() any                         // this device's status for pool views
    Log       *slog.Logger
}
func New(o Options) *Node
func (n *Node) Run(ctx context.Context) error    // serve the peer API on this device's tailnet IPs only
func (n *Node) Brokers(ctx context.Context) []core.WorkSource   // remote brokers in the pool, healthy first
func (n *Node) Claim(...) / Release(...)          // implements core.Claimer across brokers
func (n *Node) Pool(ctx context.Context) ([]DeviceStatus, error)
func RemoteInstall(ctx context.Context, host string, args InstallArgs) error  // over `tailscale ssh`
```

Peer API (HTTP on the tailnet address, `mesh.port`, default 7433). Every request's remote
address goes through `WhoIs`; only the same tailnet user (or `members`) is served, and only
devices whose `/v1/hello` pool name matches. A request from this device itself is refused (403)
before `WhoIs`: a loopback source, one of this device's tailnet IPs (IPv4-mapped too), or an
address `WhoIs` names with one of those IPs. A device never calls its own peer API over the
network, and such a caller is a local process, such as a CI job on Windows, where the firewall
does not filter loopback, or wherever the job fence leaks. Endpoints: `GET /v1/hello` (device, pool, version,
broker yes/no), `GET /v1/status`, `POST /v1/work` (core.WorkRequest -> core.WorkResponse),
`POST /v1/claim`, `POST /v1/release`, `GET /v1/archive/<ref>` (a check's source archive, streamed
from the broker to the device named in the `Tentmaker-Device` header; the broker checks that it
granted the check to that device; 404 otherwise). The device in a work body or the
`Tentmaker-Device` header must be the name the caller's tailnet node announced in its
`/v1/hello` (403 otherwise; an unknown node runs discovery at most every 5 s). The caller is the
peer whose stable node ID `WhoIs` returned (`Identity.ID`, the same ID as `Peer.ID`), at an
address that peer has; a `WhoIs` answer without one acts as no device. A name two nodes announce,
or this device's own name, is nobody's. The work and archive handlers pass the caller's node to
the broker (`core.WithCallerNode`), which serves a grant's reports and archive only to the node it
was granted to; `core.ErrOtherNode` from `Work` is a 403. So a node that announces the name of a
device that dropped out of discovery inherits none of its grants. Claim and release bodies name the
grantee, not the caller, so they are not bound. Run history endpoints (`Options.Runs`, a `RunsSource`; absent when nil): `GET /v1/runs` (runs.Query
as URL parameters -> runs.Answer, this device's records, running jobs and, on a broker, the jobs it
holds queued), `GET /v1/runs/log?id=&max=` (runs.LogAnswer, 404 when the device doesn't have the
job) and `POST /v1/rerun {device, repo, run_id}` (only a broker answers; the token never leaves
it). `Node.PoolRuns`, `PoolRunLog` and `PoolRerun` ask this device and every peer, merge, and
report a peer that failed in `Answer.Errors`. Answers are capped at 8 MiB (runs) and 2 MiB (logs).
Bodies are JSON, capped at 1 MiB, 10 s
timeouts, except the archive stream (10 min, 2 GiB) and the run history answers. Peers
are rediscovered every 30 s; a peer failing 3 calls in a row is skipped for a minute.

## Updates and channels (internal/update, scripts/)

- **stable**: GitHub Releases tagged `vX.Y.Z`, not marked prerelease.
- **dev**: one rolling prerelease tagged `dev`, rebuilt on every push to `main`. Version
  `X.Y.Z-dev.<UTC yyyymmddHHMM>.<sha7>`.
- Each release has `tentmaker_<os>_<arch>.tar.gz` (`.zip` on Windows) and `SHA256SUMS`.
  Installers and `tentmaker update` verify the sha256 before anything runs.
- Update: download, verify, write next to the binary, run `<new> version --json` as a health
  check, swap atomically (Windows: rename the running exe aside, then move the new one in), restart
  the service, and roll back to the old binary if the daemon isn't healthy within 60 s. Auto-update
  only between jobs: after the download it takes `Worker.Hold`, which closes admission and counts
  jobs under one lock, and keeps it through the swap and exit; a worker checks admission again under
  the lock that adds a job's record and declines the offer if it closed, so no job starts between
  the last zero-jobs check and the restart. A hold is memory only; a restart drops it.
- `tentmaker update` (CLI and the MCP tool) pauses new jobs through the control API and waits up
  to 30 min for running ones, because the restart ends them (on Windows the daemon's job object
  kills them). If they don't finish it resumes and changes nothing; `--force` (CLI only) skips the
  wait. It resumes after the new version reports healthy, or after a rollback, unless the device
  was paused before the update (review r1 partB first-F5). It skips the drain only when the
  service manager positively reports the service not installed or stopped; if the manager's status
  call fails, or the service runs but its control API doesn't answer, update changes nothing and
  says to retry or pass `--force` (review r2 partB sol-F1).
- Auto-update has no one watching, so the daemon guards itself: before it swaps the binary it
  writes `<state>/update-pending.json` (from, to, time), and if that write fails it does not
  update at all; a failed swap deletes the marker. The new binary deletes it once
  the control API serves and the worker has ticked. If the marker is older than 2 minutes when a
  daemon starts, or the grace ends before the binary gets healthy, the daemon rolls back to
  `<bin>.prev`, adds the version to `<state>/update-bad.json` (auto-update skips those), deletes
  the marker and exits, so the service manager starts the previous binary. A marker for another
  version is deleted.
- The service runs only a binary that root alone can change. Setup walks the running binary and
  every parent folder (Unix: owned by uid 0, no write bit for others, group write only for gid 0,
  no symlinks; Windows: owner and every allow ACE that can write, delete or change the DACL belong
  to SYSTEM, Administrators or TrustedInstaller). If the walk fails, for example a binary in
  `~/.local/bin`, setup copies it to `/usr/local/bin/tentmaker` when that folder passes, else
  `/usr/local/libexec/tentmaker/tentmaker`, and on Windows to
  `%ProgramFiles%\Tentmaker\tentmaker.exe`, checks the copy, points the service there and says so
  in one line that names the old copy to remove. With no safe folder setup refuses. `update` and
  `rollback` work only on that root-owned path, and `doctor` fails a service binary that a
  non-root user can change. A root service that runs a user-writable binary would let that user
  run code as root (review r1 second-F7). Every check runs on the canonical path, and that same
  resolved path is the one copied to, put in the service spec, updated and rolled back:
  `binsafe.Problem` refuses a path that resolves through a symlink, because whoever can write the
  folder holding the link (a user-writable `/usr/local` with `bin` pointing at a root-owned
  folder) could repoint it after the check (review r2 partB sol-second-F7). (internal/binsafe,
  internal/cli)
- On Windows an elevated user owns what it creates, and an owner can always rewrite the DACL, so
  a folder or binary the elevated user owns fails the walk. Before setup makes the service's folder
  and copy, `binsafe.OwnNewFiles` sets its token's default owner to Administrators, so nothing it
  creates is ever owned by the user (setting the owner after the create would leave a moment in
  which it is); without elevation setup makes no copy. `update` and `rollback` do the same before
  they write the new binary or the `.prev` copy; before, an elevated `update` left them owned by
  the user and the next `rollback` refused them. `install.ps1`, run as Administrator into a
  system folder, installs only into a folder named `Tentmaker` (the default
  `%ProgramFiles%\Tentmaker`) and never lets the user own it or the binary, even for a moment,
  because an owner can always rewrite a DACL and a handle opened while the user owned an object
  keeps working after the owner changes. A new folder is created with Administrators as owner and
  a protected DACL: SYSTEM and Administrators full, Users read and execute, and OWNER RIGHTS read
  and execute, which takes the owner's implicit rights away from every file made in it. An
  existing folder is kept only when SYSTEM or Administrators own it and everything in it, and no
  one else can change any of them; it then gets that DACL in place of its own. Any other folder
  is moved aside to `Tentmaker.untrusted-<ticks>`, never repaired in place, since its other owner
  or writer may hold a handle a new DACL can't revoke or have left a DLL the service would load;
  the install fails, naming `Stop-Service tentmaker`, if it can't be moved. A `tentmaker.exe.prev`
  anyone else owns is deleted, since rollback would run it. The archive is read once and checked
  and extracted from the same bytes, since the temp folder is the user's, and the staged binary is
  always a new file. Before, every Windows install failed setup with "owned by S-1-5-21-..., not
  SYSTEM or Administrators".
- `setup --json` reports `binary`, the path the service runs. After a copy it is not the binary
  on PATH, and `update` and `rollback` change only it.

Install:

```sh
curl -fsSL https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.sh | sh
curl -fsSL .../install.sh | sh -s -- --channel dev
```
```powershell
irm https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1 | iex
```

Then `sudo tentmaker setup` (wizard), or for agents:
`sudo tentmaker setup --yes --json --memory 4GiB --boost-memory 12GiB --repo owner/name`.

For agents the whole quick start is one command with one JSON line on stdout:
`install.sh --yes --json -- SETUP ARGS` (`$env:TENTMAKER_JSON = '1'` or `-Json` for install.ps1).
The installer's own messages go to stderr (PowerShell: the host). With `--yes` the line is
setup's result (`ok`, `next`, `timings`); without it, `{"ok":true,"installed","version","next"}`;
an installer failure before setup is `{"ok":false,"error","fix"}`. `--json` among the setup
arguments turns the mode on too. The e2e workflow runs exactly this command on Linux, macOS and
Windows and times it (`go run ./scripts/bench install`, docs/BENCHMARKS.md "Install").

## Run history and alerts (internal/runs, internal/alerts)

```go
// core
type JobLog struct { Data []byte; Size int64 }
type Usage struct { PeakMemoryBytes int64; CPUSeconds float64; CacheBytes int64 }
type JobInspector interface {                 // an Executor that can show a job
    JobLog(ctx context.Context, name string, max int64) (JobLog, error)
    JobUsage(ctx context.Context, name string) (Usage, error)
}
type Rerunner interface { RerunFailed(ctx context.Context, repo string, runID int64) error }
type JobReport struct { Conclusion, FailedStep string; Log []byte; LogErr error }
type JobReporter interface {                  // a Forge that reports a finished job (GitHub)
    JobReport(ctx context.Context, repo string, jobID, maxLog int64) (JobReport, error)
}

type DebugSpec struct { Limits Resources; CPUCeiling float64; Timeout time.Duration; MaxOutput int64 }
type PoolCPUCapper interface { CapsPoolCPU() bool } // an Executor whose pool cap holds every job's CPU
type DebugResult struct { ExitCode int; Output []byte; Truncated, TimedOut bool }
type Debugger interface {                     // an Executor that can keep a failed job's sandbox
    Keep(ctx context.Context, name string) error
    DebugExec(ctx context.Context, name, command string, spec DebugSpec) (DebugResult, error)
    DebugShell(ctx context.Context, name string, spec DebugSpec, stdin io.Reader, stdout io.Writer) error
}
// WorkRequest gains Kept []CacheRepo: repos the worker keeps a sandbox of.

// dispatch
type RunRecorder interface {                  // WorkerOptions.Runs
    JobEnded(rec runs.Record, log []byte, tests *runs.TestResults)
}
type TestCollector interface {                // an Executor that can read a job's test reports
    TestResults(ctx context.Context, name string) (runs.TestResults, error)
}
func (w *Worker) KeptJobs() []KeptJob
func (w *Worker) DebugExec(ctx context.Context, name, command string, timeout time.Duration, maxOut int64) (core.DebugResult, error)
func (w *Worker) DebugShell(ctx context.Context, name string, stdin io.Reader, stdout io.Writer) error
func (w *Worker) ReleaseKept(name string) error
type ReportRecorder interface { ForgeReport(rep runs.ForgeReport) }    // BrokerOptions.Reports
func (w *Worker) RunningRecords() []runs.Record
func (w *Worker) JobLog(ctx context.Context, name string, max int64) (core.JobLog, bool, error)
func (b *Broker) Queued(repo, sha string, runID int64) []string

// runs
type Record struct { ID, Kind, Forge, Repo, Workflow, Job string; JobID, RunID int64; RunURL, Ref,
    SHA, Device, Executor, Platform string; Slot int; QueuedAt, StartedAt, EndedAt time.Time;
    Conclusion, FailedStep, Note string; PeakMemoryBytes int64; CPUSeconds float64;
    CacheBytes int64; HasLog bool; LogSource string;   // LogSource: "forge" or "runner-diag"
    Tests *TestSummary; KeptUntil time.Time }
type ForgeReport struct { ID, Device, Forge, Repo, Job string; JobID, RunID int64; RunURL string;
    EndedAt time.Time; Conclusion, FailedStep string; Log []byte }
type Query struct { Repo, SHA, ID string; RunID int64; Failed bool; Since time.Time; Limit int }
func Open(dir string, max int) *Store         // Add, List, Log, Err
func Redact(data []byte, extra ...[]byte) []byte
func Stats(recs []Record, since, now time.Time) Report
func Wait(ctx context.Context, look func(context.Context) (Snapshot, error), o WaitOptions) (WaitResult, error)
type TestCase struct { Suite, Name, Status string; Seconds float64; Message, Detail, File, Source string }
type TestResults struct { ID, Device string; Cases []TestCase; Files, Problems []string; Dropped int }
type TestSummary struct { Total, Passed, Failed, Skipped int; Failures []string; Truncated bool; Sources, Suites []string }
func (s *Store) PutTests(id string, t TestResults) error   // and Tests(id)
func Flaky(recs []Record) []FlakyTest
func Why(rec Record, tests *TestResults, log []byte, history []Record) WhyAnswer

// testresults
func Collect(dir string, subdirs []string, lim Limits) runs.TestResults
func ParseJUnit(r io.Reader, file string) ([]runs.TestCase, error)
func ParseGoTestJSON(log []byte) []runs.TestCase

// alerts
func New(o Options) *Notifier                 // Run, JobEnded, Test, Counts, Enabled
```

- A record is the job on the device that ran it. Its `ID` is the runner name the device gave the
  job (`core.NamePattern`), so it is unique in the pool and safe in a path. The worker builds it
  when a job ends, before `Clean`: the executor's `JobLog` and `JobUsage` are read while the job
  folder and cgroup still exist. Usage is also sampled every 5 s while the job runs and the
  highest reading of each number kept, because a cgroup is gone once its unit exits.
- A runner job's conclusion, failed step and log come from the forge. The runner's `_diag` logs
  hold only trace lines: actions/runner writes step output to `_diag/pages` and deletes the pages
  once it uploads them. So when a runner job ends identified, its broker, which holds the forge
  token, reads the job (`GET /repos/{repo}/actions/jobs/{id}`: conclusion, and the first step
  whose conclusion is `failure` or `timed_out`) and its log (`.../jobs/{id}/logs`, a redirect to a
  signed URL fetched without the token, at most 64 MiB read, the last 512 KiB kept). It tries
  every 10 s, 12 times, while the job is not completed or the read fails, and 3 times while the
  log is not up yet. This works for every executor, the darwin one too, which can show no log.
- The worker writes the record first, from the executor's diag log (`Job result` lines and the
  failed step, `LogSource` `runner-diag`; `unknown` where the executor has no log). The broker's
  result then replaces the conclusion and failed step, and, when the log was read, the log tail
  (`LogSource` `forge`); a conclusion that changes fires the alerts then. A result that comes
  before the worker's record waits in memory for it. For a job that ran on a peer, the broker
  keeps a copy of the record (`LogSource` `forge`, its device the peer's) with the forge log;
  `runs.Merge` puts the copy's conclusion on the peer's record and `PoolRunLog` prefers a `forge`
  log to a `runner-diag` one. If the forge can't be read, the record keeps the diag conclusion
  and tail and says `runner-diag`. A check job's conclusion comes from its check result. A job
  Tentmaker stopped (budget, guards, shutdown) is `cancelled` with the reason in `Note`.
- The store is `<state>/runs`: `index.json` with the last `history_max` records (default 2000)
  and one log file per job, the last 512 KiB, redacted before it is written. Every file goes
  through `config.WriteAtomic`, mode 0600 root. The oldest records and their logs go first.
- Redaction replaces GitHub and GitLab tokens, JWTs, a JIT config line and any base64 run of 120
  or more characters (a JIT config), `Authorization` and `Private-Token` values, passwords in
  URLs, AWS key ids and Slack and Discord webhook paths with `***`, plus any exact secret bytes
  the caller passes. The runner masks the secrets it knows before it writes a log; these catch
  the rest. A live job's log passes through `Redact` whenever it is served.
- Job logs are readable by any local user except the CI user (GET /v1/runs/log on the control
  API), like status and pool. Decision: an agent's MCP server runs as the user, and making it
  root to read a redacted tail would be worse than the tail itself.
- `wait_for_run` settles when it has seen at least one record, every record has ended and no
  broker holds a job queued for the commit, all for 20 s, because a job that needs another one
  queues only after it ends. A runner record that ended `unknown` without a forge result counts
  as not ended for `runs.ForgeGrace` (2 minutes), so the broker's result can land. A look that
  fails is retried at the next poll. `Broker.Queued`
  returns nothing unless a sha or a run id narrows it.
- Stats: p50 and p95 by nearest rank over ended jobs in the window. The saving is each job's
  minutes rounded up, as GitHub bills, times `runs.HostedRate` for the job's OS, and every answer
  says it is an estimate and when the rates were checked.
- Test results. Before `Clean`, the worker asks an executor that is a `TestCollector` for the
  JUnit reports the job left in its workspace (Linux: `runner/_work` and a check job's `work`).
  `testresults.Collect` reads them as root through an `os.Root`, so nothing leaves the job
  folder; it never follows a symlink, opens only regular files with `O_NONBLOCK` (a planted FIFO
  can't hang the daemon), sniffs the first 4 KiB for a JUnit root element, skips `.git`,
  `node_modules` and the like, and stops at 200 reports, 16 MiB a report, 64 MiB in all, 50000
  entries walked and depth 16, saying what it skipped in `Problems` (relative paths only). The
  history adds `go test -json` events from the log (raw or behind the forge's timestamps); a
  JUnit case wins over a go test one for the same test. A broker that reads a forge log later
  adds its go test events too. The device keeps each run's cases in `runs/tests/<id>.json`
  (`config.WriteAtomic`, 0600): failures first, at most 500 failed and 5000 in all, each message
  1 KiB and detail 4 KiB, redacted. The record carries a `TestSummary` with at most 50 failing
  test ids, so the pool view and the flaky rules need no device's file. `GET /v1/runs/tests` and
  the mesh's `/v1/runs/tests` serve the cases from the device that ran the job.
- Flaky tests (`runs.Flaky`), from the summaries of one job (repo, workflow, job): a test is
  flaky when it failed and a later run on the same commit reported tests without it failing
  (`same-commit`), or when it changed between failing and passing at least twice in the last 10
  runs on the same ref (`flips`). A run counts as a pass only when its summary is whole and names
  the test's suite in `Suites` (a go test package seen from `start` to its end; a package cut from
  the log tail is `Partial`), so a test missing from a cut log or a run that stopped early is not
  called flaky. A test that broke once and stayed broken is not
  flaky.
- `why_failed` (`runs.Why`, `GET /v1/runs/why`, `tentmaker why`): the run's commit, ref, run,
  conclusion and failed step; its first 5 failing tests with a 300 byte message and the first 8
  lines of detail (and a count of the rest); 10 lines before and 20 after the first log line
  that looks like an error (`##[error]`, `--- FAIL`, `panic:`, `FAIL`, `Error:`, `npm ERR!`,
  `error:`, a non-zero exit code), with the forge's timestamps cut, or the last 30 lines when no
  line does; and whether it looks flaky: the same job passed on the same commit, or a failing
  test is flaky by the rules above. Everything is redacted. With a repo and sha or run id it
  explains the newest failed run of them; its flaky history is the pool's last 14 days of that
  repo.
- Alerts run on the device that ran the job, and on the broker for a peer's job whose forge
  result changed its conclusion, so a pool with alerts on both may send one twice. `JobEnded` never blocks: a full queue (64) drops
  the alert and counts it, with one warning a minute. Each send has a 10 s timeout, each alert a
  15 minute cooldown per repo and job, and `recovered` compares with the last conclusion of the
  same repo and job on this device (cancelled and unknown don't reset it). Errors name only the
  webhook's host, because Slack and Discord URLs are credentials; `url_secret` keeps them in the
  secret store.

## Debugging a failed job (internal/dispatch/debug.go, internal/executor/linux/debug.go)

A repo can ask for a failed job's sandbox to be kept: `[repos.debug] keep_failed = "10m"`, off by
default, at most 1h. Then `tentmaker debug JOB-ID` opens a shell in it and the MCP tool
`debug_exec` runs one command there.

- What is kept. When a job ends failed on its own (not stopped by Tentmaker, not during
  shutdown) in such a repo, on an executor that is a `core.Debugger`, the worker calls `Keep`
  instead of `Clean`. The job is reported ended to its broker at once, so its runner is deleted
  at the forge and its relays and Actions cache grant are revoked; the record gets `KeptUntil`.
  The job stays in `state.json` with `KeptUntil` set, so it still holds its slot and its memory
  (not its repo cache: other repos' jobs may then start), it blocks the node reset that would wipe it, and it counts against the
  budget. The worker sends `WorkRequest.Kept` and the broker offers that device no job of the
  repo (the worker declines one too), so a new run can't land on a half-debugged device. When
  the grace ends, when the owner releases it (`tentmaker debug JOB-ID --release`), when a hard
  guard evicts, or when its executor is gone, the worker cleans it up exactly as it cleans any
  job. `KeptUntil` lives in `state.json`, so a restart keeps the deadline.
- Linux keeps the job folder, the slot, the namespace holder and the pasta uplink, and removes
  the runner's `.runner`, `.credentials` and `.credentials_rsaparams` through an `os.Root`. A
  debug command is a transient unit `tentmaker-debug-<job>-<random>` built like the job's: the
  runner's uid and gid, the job's `MemoryMax` and `CPUQuota`, `NoNewPrivileges`, `PrivateTmp`,
  the job's mounts (not its repo cache; its container store when bound), its environment (no secrets), `RuntimeMaxSec` of the timeout,
  and `NetworkNamespacePath` of the job's own namespace holder. If the holder is gone it refuses:
  no debug command runs outside the job's namespace. `debug_exec` runs `systemd-run --wait
  --pipe ... /bin/sh -s` with the command on stdin, so the command is in no argv on the host;
  it keeps the last `max_bytes` of output (64 KiB by default, 1 MiB at most) within a timeout
  (1 minute by default, 10 at most). The shell is `systemd-run --wait --pty`, bounded by the
  time left in the grace and by 1 hour. Every debug unit is stopped when its command or shell
  ends, and `Stop` and `Clean` stop all of a job's debug units before they take its namespace
  and folder. macOS and Windows have no `Debugger` yet: their failed jobs are cleaned up as
  today and the log says why.
- Threat model. The kept sandbox is the job's: whatever ran in it is untrusted.
  - Who may debug. Only local callers the control API trusts with pause and resume: the token
    (readable only by root or Administrators) or an admin peer (root, an elevated
    Administrator). `POST /v1/debug/exec`, `/v1/debug/shell` and `/v1/debug/release` use
    `tokenOrAdmin`; the runner user gets 403 and other local users 401. There is no mesh route:
    a peer, even one in the pool, can't debug another device's job, and a caller asking the
    wrong device is told which device ran it. The MCP server offers `debug_exec` only when it
    runs as root, like `rerun`.
  - What a debug command can reach: what the job could, no more. Same uid, so the uid-matched
    nftables fence (no tailnet, LAN, link-local, host addresses or mesh port) applies; same
    network namespace and uplink; same limits; `NoNewPrivileges`; other homes and the Tailscale
    paths hidden. The Actions cache grant is revoked, so the cache service answers 404, and the
    runner is deleted at the forge, so the runner credentials and the JIT config can't be
    reused. GitHub ends a job's `GITHUB_TOKEN` when the job completes, so a token the job left
    in `.git/config` is dead. Secrets the workflow itself wrote into the workspace stay there
    for the grace, and other repos' jobs on the device run as the same runner uid, so they can
    read the kept folder until it is cleaned up. Don't set `keep_failed` on a repo whose
    workflow writes secrets to disk if the device runs other repos' jobs.
  - What leaks to logs. The command text is never logged, not by the control server, the worker
    nor the executor (it travels on stdin). The output is redacted with `runs.Redact`, capped,
    returned to the caller and never logged; the logs say only the job id and the exit code.
  - What it costs. The grace is at most 1 hour and counts against the budget; a hard guard that
    evicts jobs still evicts it, which cleans up a kept sandbox at once. A device with a kept sandbox takes no job of that repo until it is gone, and other
    repos only within its remaining slots. If a cached job of that repo ran on the device since
    its last reset, the repo cache hold (Dispatch) also keeps other repos off until the reset,
    which the kept sandbox delays: the device then idles for the rest of the grace.
  - Containment if the debugger is careless. Every process of a debug command lives in its
    unit's cgroup and ends with it: the unit is stopped when the command ends, `RuntimeMaxSec`
    bounds it, and `Clean` stops every debug unit of the job. Containers it starts through the
    job's podman socket are the same as containers the job starts, and are cleaned up the same
    way.

## Testing

- `go vet ./...` and `go test ./...` pass on Linux, macOS and Windows.
- Cross-compile check: `GOOS=linux,darwin,windows` x `GOARCH=amd64,arm64` all build.
- No test touches the real host: OS calls go through small interfaces with fakes. Tests that
  need root, podman, tart or a real tailnet are behind the `live` build tag.
- Fakes: `forge/fake` (`Reruns`, `MRerunFailed`), `executor/fake` (`SetLog`, `SetUsage`,
  `SetTests`, `SetKeepErr`, `SetDebugExec`, `DebugCalls`, `Kept`), a fake
  `Tailscale`, a fake power source.

## Decisions made while building (v0.1)

Recorded as built. Where a line and an earlier section disagree, this section wins until the earlier
section is rewritten.

**budget**

- `(*Engine).WithLocation(loc)` sets the time zone schedule windows are read in.
- `Decision.Profile` is always `everyday`, `boost` or `off`; a schedule window's name goes in `Reasons`.
- A schedule window set to `everyday` runs even on battery when `on_battery = "off"`; only a `boost` window falls back to `on_battery`.
- A schedule `boost` window skips `boost_needs_idle`.
- A boost profile with zero memory reports `everyday`.
- Linux power: an online USB or USB-C supply counts as AC; peripheral batteries (mouse, keyboard) are ignored; multiple batteries are combined by energy; with no Mains entry, the battery status decides.
- macOS power: `pmset` "UPS Power" counts as battery.
- Headless Linux idle is an error, so a device with `boost_needs_idle` and no desktop session never boosts.
- Free disk is measured at the nearest existing parent of the path.

**forges**

- Gitea `Grant` returns `ErrUnsupported` (Gitea has only reusable registration tokens); Gitea repos use `mode = "checks"`.
- The Forgejo grant secret is the forgejo-runner `.runner` file as JSON.
- `forgejo.Options.Flavor` is `forgejo`, `gitea` or empty for auto (ask the server once).
- Forgejo can't say which job a runner holds, so its runners report `JobID` 0.
- `migrate`'s added concurrency group starts with `${{ github.workflow }}`.
- Grants expire after 1 h (`github.GrantTTL`).
- v0.1 runs Forgejo, Gitea and GitLab repos in checks mode only.
- Every request that carries a credential header (`Authorization`, `PRIVATE-TOKEN`, `JOB-TOKEN`,
  `Proxy-Authorization`, `Cookie`) goes through `httpsafe.Guard`: a redirect to another origin
  (scheme, host and port) is refused. Go copies `PRIVATE-TOKEN` to every redirect target and
  `Authorization` to any port of the same host, so a forge or proxy that redirected elsewhere got
  the token (review r1 sol-F2). Covers the GitHub, Forgejo and GitLab clients, `connect`'s token
  check and the control client. Archive downloads still follow the redirect (GitHub's goes to
  codeload) but drop the header unless the target has the archive URL's origin.

**executors**

- `ForHost` takes a `binary func(core.Platform) (string, error)` that returns the tentmaker binary for a sandbox's platform.
- VM images are package constants; `Options.Images` overrides them.
- macOS guests are optional. darwin `Install` pulls the Linux image whenever it fits (one job's room plus the image), and the macOS image only when free disk covers everything plus a job, or with `setup --macos-guests` (`Options.MacOSGuests`: skips the job margin, but the image itself still has to fit). It records what it pulled in `<state>/macos-tart/guests.json`; the executor offers only those platforms (all pinned ones when the file is missing). When not even the Linux image fits, Install downloads nothing and returns nil, so setup exits 0 and installs the service; `Preflight` then reports the missing image as a problem, and `forHost` keeps the executor on Apple silicon so doctor can say so. An executor's optional `Notes(ctx) []core.Problem` shows in doctor as warnings: "macOS guests are off: the macOS image needs N GiB, M GiB free; run sudo tentmaker setup --macos-guests when you have room".
- Linux `Reset` wipes podman images unless `KeepImages`: the default store's. A repo's container store keeps its images (Repo caches above).
- Linux jobs keep a cache per repo between jobs (Repo caches above); macOS and Windows jobs start cold. `cache_max` caps those caches and their container stores, not all of `cache_dir`.
- The JIT config reaches the runner as `ACTIONS_RUNNER_INPUT_JITCONFIG`, set inside the sandbox by the job script from its stdin hand-off (the runner reads and clears it); never in the daemon's environment or argv.
- `SetPoolLimit` is a no-op on macOS and Windows.
- Linux and Windows keep a copy of the tentmaker binary for check jobs. Setup writes it, and each check `Start` compares it with `Binary(platform)` (size, then sha256; equal pairs are remembered by size and mtime) and replaces it atomically when they differ, so an update never leaves check jobs on the old binary. Linux writes it root-owned 0755; Windows renames the old exe aside first (a running check may hold it) and deletes it once free. (internal/executor/guestbin)
- A guest binary for another platform (a Linux Tart guest on a Mac) is cached as
  `<cache>/bin/tentmaker_<os>_<arch>-<version>`, so after an update `Binary(platform)` downloads the
  new version instead of serving the old one; copies for other versions are deleted (review r1
  partB first-F2, sol-F3). The dev release's tag moves, so a dev host can still get a newer dev
  build than its own until it updates.
- The Windows runner password uses machine-scope DPAPI, protected by the file ACL (SYSTEM and Administrators).
- Every folder Windows setup protects (jobs, lib, runner, secret, dist, tools, and the secrets
  store) gets Administrators as owner in the same call that sets its DACL, and Install reads the
  owner back and refuses a folder still owned by anyone but SYSTEM or Administrators. ProgramData
  lets any user create a folder first, and an owner can always rewrite the DACL and read the
  runner password (review r1 partS first-F3).
- Network rules are written at setup. A changed `allow_lan` takes effect only after `sudo tentmaker setup` runs again; until then the loaded rules don't match the config: Linux treats rules missing the LAN as stale and reloads them stricter at the next `Start`, and Windows (description mismatch) refuses jobs. Rules stricter than the config pass. Host facts the rules name (Linux subuid ranges, Tailscale interfaces, on-link IPv6 prefixes and DNS servers; Windows DNS servers) are read again at every `Start`. A reload that changes `allow_lan` rebuilds the worker and its executors (the daemon's worker key includes it), so the next job is checked against the new policy; before, the old executors kept the old `allow_lan` and jobs ran under the weaker policy (review r1 sol-F1).
- Linux job units have no `--collect` and set `OOMPolicy=stop`: a unit the OOM killer ended stays loaded with `Result=oom-kill` until `Clean` runs `systemctl reset-failed`, so `CheckResult` can say "ran out of memory" instead of "cancelled" or "no result.json".
- A Windows job object's memory limit fails the allocation instead of killing the job, so check-run reports an ordinary step failure. The job object posts `JOB_OBJECT_MSG_JOB_MEMORY_LIMIT` to a completion port the process reads; `CheckResult` then says "ran out of memory (the job's limit is N GiB)" ahead of check-run's words, unless the job passed anyway. .NET caps its own heap at 75% of a job object's limit and throws first, so a .NET program that runs out reads as an ordinary failure. The daemon remembers this only while it tracks the job.

**mesh**

- `Options` has `Version` and `Platforms` for `/v1/hello` and pool views.
- Served callers are the same tailnet user OR a login in `members`.
- Tagged devices are served only when `members` contains `tagged-devices`.
- Every endpoint but `/v1/hello` needs a matching `Tentmaker-Pool` header.
- A member's device acts only as itself: binding the device name to the WhoIs-checked source
  address stops one device reporting results or fetching archives as another (review r1 partS
  first-F4).
- `Identity` has `ID`, the stable node ID. The broker binds each grant to the caller's node, so a
  node that announces the name of one that dropped out of discovery can't report for or fetch
  the archives of that device's grants (review r2 partS sol-S2-F2).
- `Claim` walks devices in name order, so exactly one racing broker wins.
- `RemoteInstall` saves install.sh to a temp file and runs the installer itself as root (`sudo -n sh "$f" --channel <c> --yes -- <setup flags>`, or plain `sh` when already root), so install.sh puts the binary on root's PATH and runs setup. It used to run install.sh as the login user and then `sudo -n tentmaker setup`, which failed when the binary landed in `~/.local/bin`.
- `Run` restarts its listener when the tailnet IPs change.

**dispatch**

- Check offers are marked with `Job.Event = "check"`.
- A quarantine lifts on the first request after delivery that asks for work with none of that broker's runners running.
- Boot backoff is per executor.
- A public runner-mode repo with `allow_public` also needs fork policy `approve_all` or `off`. `dispatch.PublicRepoRefusal` is that rule, and `doctor` calls it for runner-mode repos, so doctor fails exactly the repos the broker refuses. Forgejo reports `approve_first_time` (it has no setting for more), so a public Forgejo repo runs in checks mode, and doctor's fix says so.
- `Broker.Tick` runs one pass (poll due repos, watch runners, handle silent workers, post statuses, save state); `Broker.Shutdown` stops granting and deletes runners that haven't taken a job.

**checks**

- `StatusForge.PendingCommits(ctx, repo, branches...)` also returns the heads of `branches` (config `[[repos]] branches`, `repo add --branch`); a branch that doesn't exist is skipped. Branch heads, like the default branch, need no `authors` match: only people with push access move them.
- The broker keeps each finished check's log tail under `<state>/checks` (newest 200). Before this, a failed `tentmaker/<job>` status pointed at nothing: the worker deletes the job dir and the tail was dropped after posting the status.
- `CheckBundle.Ref` carries the branch.
- A broker claims each check through its `Claimer` before it grants it, so two brokers with the same repo run a check once. Check jobs have no forge ID, so `Job.HeadSHA` (set on check offers) and the job name key the claim (`Job.Key`). The granting broker holds the claim, not the worker: a worker asks every broker, and a claim held by the worker let both brokers hand it the same check (found by the team test, two brokers).
- The relay has its own loopback listener, 127.0.0.1:7435 (`control.DefaultRelayAddr`,
  `daemon.Options.RelayAddr`, `control.NewRelayServer`), and the fence opens that port, not the
  control API's 7434. Before, both shared 7434, so a job could read `/v1/status` and `/v1/pool`
  and loop `/v1/doctor`, which calls the forge with its token for every repo and drains the rate
  limit (review r1 first-F1). A host whose rules predate this keeps the old fence until `sudo
  tentmaker setup` runs again.
- The Actions cache names its caller by connection (the job's pasta uplink unit) instead of a
  secret in the job's environment. A capability in `.env` would be readable by every step and
  every action, and would have to be kept out of logs the runner writes; the uplink unit is
  something a step cannot forge. The `sig` capability in returned URLs still binds a URL to one
  job's grant, so a URL that leaks to another job or outlives its job is 404.
  The uplink unit names a job of the device, not which one: jobs share the runner uid (see Hold).
- The control API names its caller instead of trusting the network (fence verification, item
  18): Windows Firewall doesn't filter loopback, so a job reached 7434 whatever the rules said.
  Linux and macOS serve it on the Unix socket `control.DefaultAddr`
  (`unix:/var/run/tentmaker/control.sock`, mode 0666) and read the peer uid; Windows keeps
  `control.TCPAddr` and maps the connection to its owning PID (GetExtendedTcpTable), then to that
  process's SID and elevation. `control.HostIdentify(daemon.RunnerUser)` marks the runner user
  (and on Linux its `/etc/subuid` ranges, which rootless containers run as) as a CI job, and the
  server answers it 403 on every route. A caller it can't name gets 403, and when the runner's
  ids can't be read only root or SYSTEM is served. Status and pool stay open to other local
  users (and so to agents through `tentmaker mcp`); doctor needs the token or an admin caller.
  `control.NewServer` takes the `Identify`; a nil one refuses everyone.
- A device refuses peer API requests from itself (fence verification, item 19): loopback, its
  own tailnet IPs, or an address `WhoIs` ties to them. WhoIs called such a caller this device,
  the most trusted peer.
- `CheckBundle.ArchiveRef` replaces the forge URL and header the broker used to send (review finding: a check job could read the forge token from its bundle). `Header` stays in the struct only so a worker can recognize and refuse an older broker's bundle.
- An empty `Platform.Arch` means any arch. Hosted `runs-on` labels (ubuntu-latest, macos-15,
  windows-2022) name an OS, not an arch, so Plan leaves Arch empty for them; only `-arm`/`-arm64`
  images or an explicit `arm64`/`x64` label pin one. Check matching compares arches through
  `ArchLabel`, so x64, amd64 and x86_64 are the same arch. Before, ubuntu-latest pinned x64 and
  never ran on an arm64 worker, with no status (review r1 partA second-F4).
- `ListDir` returns sorted entry names, and `ErrNotFound` for a missing folder.
- GitLab `after_script` runs as a final continue-on-error step, not as an always-run step.
- A matrix job plans one check job per combination (at most 256), each with its own status
  `tentmaker/<name>`. The name is the job's `name:` resolved for that combination, or
  `<id> (<values>)`; a `name:` that doesn't use the matrix gets ` (<values>)` added so every
  combination has its own context. A combination that can't run is skipped on its own. Before,
  a matrix with more than one combination skipped the whole job, so a repo's usual
  `test (${{ matrix.os }})` job never ran.
- The daemon logs every job Plan skips ("check job skipped", workflow, job, why), since a skipped
  job gets no status and otherwise left no trace.
- The tools folder is `<state>/tools` (Linux `/var/lib/tentmaker/tools`, Windows
  `%ProgramData%\Tentmaker\state\tools`). Setup creates it, root-owned and readable by the CI
  user (Windows: Administrators write, the CI user reads), and the Linux and Windows executors
  pass it to check jobs as `TENTMAKER_TOOLS`. Before, check-run fell back to a folder inside the
  jobs directory that nothing created and that a reset would wipe.

**onboarding**

- `migrate --pr` owns the branch `tentmaker/migrate`: it builds it from `origin/<default branch>`
  (read with `git ls-remote --symref origin HEAD`), runs migrate there, commits only the files it
  changed, and pushes with `--force-with-lease` set to what origin had a moment before. It refuses
  a dirty working tree and always puts the checkout back on the branch it was on.
- Under sudo every git command runs as `$SUDO_USER` (`sudo -n -H -u`), with
  `GIT_TERMINAL_PROMPT=0`, so the user's own git credentials push and no file in the checkout ends
  up owned by root; the workflow files migrate rewrote as root are given back to their folder's
  owner. Each git call has a timeout (30 s, 2 min for fetch, push and ls-remote), and git's stderr
  in an error has URL credentials removed.
- The pull request token is gh's (as `$SUDO_USER` under sudo), else the service's: `POST /v1/pr`
  (control token) makes the daemon open it with its stored GitHub token, for a repo it serves. The
  token never leaves the daemon.
- `setup --repo-here` reads origin from `.git/config`, or for a linked worktree from the owning
  repo's config (`gitdir:` then `commondir`). `setup --pr` runs `migrate --pr` only when every
  setup step and doctor passed (warnings are fine).
- Doctor's `first-job` check passes when GitHub lists an online runner labeled `tentmaker` for the
  repo, or a recent job ran on one. Before any job targets the pool it warns, so `setup --pr`
  still runs; a job for the pool queued longer than 3 minutes with no runner fails.
- install.sh, piped with no terminal on stdin, runs `sudo -v </dev/tty` first when `/dev/tty`
  opens, so a person can type the sudo password for the one-liner; agents (no terminal) keep
  `sudo -n`.

**release**

- `tentmaker version --json` prints version, os, arch and channel.
- The dev release title is the exact dev version; `internal/update` reads it from there.
- `update.New(Options)` builds an `Updater`; package-level `Check`, `Download`, `Apply` and `Rollback` wrap a default one.
- install.sh test hooks: `TENTMAKER_DOWNLOAD_BASE` replaces the release URL (a URL or a local directory), `TENTMAKER_SUDO` replaces the sudo command.
- Update test hooks, read by `tentmaker update` and the daemon (set them in the service's
  environment): `TENTMAKER_UPDATE_API` replaces `https://api.github.com` as the release API
  (`update.FromEnv`; assets are still verified against the release's `SHA256SUMS`), and
  `TENTMAKER_UPDATE_EVERY` (a Go duration, at least 10 s) replaces the 6 h auto-update interval.
  A build with `-tags tentmaker_badstart` has a daemon that exits right after the update guard,
  for the upgrade scenario's rollback test. test/scenarios uses all three.
