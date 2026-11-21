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
2. **The day job wins.** CI drains (stops taking jobs, lets running ones finish) whenever the
   budget, a guard or the battery says so. Only hard guards stop running jobs.
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
internal/forge/fake/      in-memory core.Forge for tests
internal/executor/linux/  ephemeral actions/runner, own user, systemd unit, rootless podman
internal/executor/darwin/ Tart VM per job (macOS and Linux guests), Softnet isolation
internal/executor/windows/ dedicated local user, Job Object limits, fresh dir per job
internal/executor/fake/   in-memory core.Executor for tests
internal/dispatch/        Broker (forge side) and Worker (device side)
internal/mesh/            tailnet peer API: discovery, WhoIs auth, remote WorkSource, claims
internal/service/         install the daemon as a systemd / launchd / Windows service
internal/update/          stable/dev channels, verified self-update with rollback
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
type Result struct { Admit, Evict bool; Reasons []string }
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
job.memory), floor(Limit.CPUs / job.cpus), job.max_slots if > 0)`. Guards run last: a guard
failure sets `Admit=false` (and `Evict=true` for `stop_below_disk`). `Admit` is also false when
`running >= Slots`. Every downgrade appends a plain reason, like "on battery at 31%, below 40%" or
"steam.exe is running". A boost profile with zero memory means "same as everyday".

Power per OS: Linux reads `/sys/class/power_supply/*/{type,online,capacity}` (Mains online, or no
Battery present, means AC). macOS runs `pmset -g batt`. Windows calls `GetSystemPowerStatus`
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
func (c *Client) SetForkApproval(ctx context.Context, repo string) error       // require approval for all outside contributors (public) / disable fork PR workflows (private)
func (c *Client) SetReadOnlyToken(ctx context.Context, repo string) error      // default GITHUB_TOKEN read-only
func MigrateWorkflow(src []byte, platform string) (out []byte, report []string, changed bool, err error)
func TokenFromGH(ctx context.Context) (string, error)                          // `gh auth token`

// forgejo (also Gitea)
func New(o Options) *Client
```

GitHub details to port from fleet-ci (`fleet_ci.py` class `GitHub`): ETag conditional GETs (a 304
costs no rate limit), list runs with `status=queued` and `status=in_progress`, refetch jobs of an
in-progress run at most every 15 s unless the run changed, ignore runs older than 24 h, sleep to
the reset when fewer than 200 requests remain, `POST /repos/{o}/{r}/actions/runners/generate-jitconfig`
with `runner_group_id` 1, delete runners, cancel runs. A job's `FromFork` is true when the run's
`head_repository.full_name` differs from the repo. Retries: 3 tries with jittered backoff on 5xx
and network errors; never retry a 4xx except 429/403-rate-limit after waiting.

## Executors (internal/executor/...)

Every executor implements `core.Executor`. Constructors take an `Options` with `StateDir`,
`CacheDir`, `Log`, plus their own fields. `internal/executor/host.go` has
`func ForHost(cfg config.Config, log *slog.Logger) ([]core.Executor, []core.Problem)` choosing by
`runtime.GOOS`. Each package also has `func Install(ctx, Options) error` and `Uninstall(ctx,
Options, purge bool) error` that set up the host (run as root/Administrator by `tentmaker
setup`), idempotent and safe to rerun.

- **linux-podman** (port of `fleet-ci-job`, `install.sh`): user `tentmaker-runner` with no sudo
  and no docker group; a root-owned runner template (actions/runner pinned version and sha256);
  per job a fresh copy, own HOME, a transient systemd unit in `tentmaker.slice` with `MemoryMax`
  = job memory, `CPUQuota` = job CPUs, Nice 10, idle IO; rootless podman socket for container
  jobs with `DOCKER_HOST`; the slice's own `MemoryMax`/`CPUQuota` = the pool limit
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
  `%ProgramData%\Tentmaker\jobs\<name>` ACLed to that user; the runner starts with
  `CreateProcessAsUser`/`LogonUser` inside a Job Object with `JOB_OBJECT_LIMIT_JOB_MEMORY`,
  CPU rate control and `KILL_ON_JOB_CLOSE`. Platforms: `windows/<arch>`. Linux jobs on Windows
  (WSL2) are roadmap, not v1.

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
func (w *Worker) Status() WorkerStatus
func (w *Worker) Pause(reason string); func (w *Worker) Resume()
func (w *Worker) Shutdown(ctx context.Context)    // drain idle, leave busy ones running
```

Broker: polls queued jobs per repo with backoff (5 s up to 30 s, x1.5 when idle), keeps a cache;
on `Work`, matches queued jobs (oldest first) to the request: a job matches when it asks for
`tentmaker` and every label it asks for is in the worker's labels for one of its platforms;
refuses forks, denied repos (`deny_repo_patterns`), public repos without `allow_public`, and runs
whose head repo differs; claims through `Claimer`; grants a runner; records (runner, device, job,
time) in its state. It monitors every granted runner: busy on an unidentifiable job for 30 s,
idle past `idle_ttl`, or busy on a fork job means `Stop` to that worker on its next request (and
`CancelRun` for a fork), plus `DeleteRunner`. A worker silent for 2 minutes gets its runners
deleted. Never grants more runners for one job than one per worker.

Worker: each tick (2 s) reads `Budget.Decide`, applies `SetPoolLimit`, reports `Running` and
`Ended`, asks sources for `Free` offers, starts each with the matching executor, honors `Stop`,
evicts on `Evict`, resets the executor when nothing runs and the node is dirty, persists
`state.json` atomically, and on start reconciles: anything `List` shows that state doesn't know
is stopped and cleaned. Boot failures back off exponentially and close the worker after 5 in a
row until restart.

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
devices whose `/v1/hello` pool name matches. Endpoints: `GET /v1/hello` (device, pool, version,
broker yes/no), `GET /v1/status`, `POST /v1/work` (core.WorkRequest -> core.WorkResponse),
`POST /v1/claim`, `POST /v1/release`. Bodies are JSON, capped at 1 MiB, 10 s timeouts. Peers
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
  only between jobs.

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

## Testing

- `go vet ./...` and `go test ./...` pass on Linux, macOS and Windows.
- Cross-compile check: `GOOS=linux,darwin,windows` x `GOARCH=amd64,arm64` all build.
- No test touches the real host: OS calls go through small interfaces with fakes. Tests that
  need root, podman, tart or a real tailnet are behind the `live` build tag.
- Fakes: `forge/fake`, `executor/fake`, a fake `Tailscale`, a fake power source.
