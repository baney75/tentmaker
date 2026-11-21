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
internal/forge/gitlab/    GitLab (checks mode only)
internal/forge/fake/      in-memory core.Forge for tests
internal/checks/          checks mode: plan workflow jobs, run them, post statuses
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

**Network isolation.** Linux and Windows jobs share the host's network stack, so Install fences
the runner user off and every `Start` checks the fence first; no rules means no job (fail closed).
Jobs may reach the internet, the DNS servers the host is configured with, and the source archive
relay on 127.0.0.1:7434. Always blocked: the tailnet (100.64.0.0/10, fd7a:115c:a1e0::/48), link-local
(169.254/16, fe80::/10), unique local (fc00::/7), loopback and this host's own addresses, and port
7433 on any address. RFC 1918 (10/8, 172.16/12, 192.168/16) is blocked unless `[guards] allow_lan`.

- Linux (`netrules.go`): nftables table `inet tentmaker`, an output hook that sends every packet
  with `meta skuid tentmaker-runner` to a `runner` chain (accept relay and DNS, then reject).
  `oifname "lo"` covers the host's own addresses. With no `nft`, the same rules as an iptables and
  ip6tables `TENTMAKER` chain with an `--uid-owner` jump. Rootless podman's pasta and slirp4netns
  run as the runner user, so container traffic matches too. The rules file lives in
  `<lib>/net.nft` (or `net-iptables.sh`); `tentmaker-net.service` (oneshot, before
  `network-pre.target` and `tentmaker.service`) loads it at boot.
- Windows (`netrules.go`): Windows Firewall outbound rules in group "Tentmaker" with `LocalUser`
  `D:(A;;CC;;;<runner SID>)`, made with the NetSecurity cmdlets through `API.PowerShell`. Block
  rules beat allow rules, so the blocked ranges leave out 127.0.0.1 and any resolver inside them,
  and those addresses get block rules for every port but the allowed one. Every rule's description
  names the rules version, the runner SID and `allow_lan`; the check compares it, and fails when any
  firewall profile is off.
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
  -> windows, self-hosted label lists by their os label), `actions/checkout` (the archive is the
  checkout), cache actions (skipped), and setup actions mapped to `Tool` (setup-go, setup-node,
  setup-python, setup-java, mlugg/setup-zig, hustcer/setup-nu, dtolnay/rust-toolchain). Anything
  else (other `uses:`, `${{ }}` expressions outside env literals that can be resolved from `github.sha`,
  `github.ref_name`, `runner.os`, `matrix` with a single combination, `services`, `container`,
  job-level `if`) makes Plan skip the job with a plain reason. A skipped job gets no status.
- **RunBundle** downloads the archive from the bundle's `ArchiveURL` (the worker's loopback relay,
  or a `file://` absolute path when the darwin executor copied it into the VM; no other scheme)
  into a fresh dir, strips the top-level folder, runs each step with `bash --noprofile --norc -eo
  pipefail` (or the step's shell) in its working directory with env `CI=true`, `GITHUB_SHA`,
  `GITHUB_WORKSPACE`, `RUNNER_TEMP`, `RUNNER_OS`, `TENTMAKER=1`, finds tools for `Tool` steps on PATH
  or under `<state>/tools/<tool>-<version>/` (it does not download toolchains in v1; a missing tool
  fails the job with the fix), stops at the first failing step unless continue-on-error, enforces
  the job timeout, writes `result.json` and `log.txt`.
- **Broker in checks mode** polls `PendingCommits` per checks-mode repo (same-repo PR heads whose
  author is in `authors`, default the owner, plus the default-branch head and the heads of the
  repo's `branches`; never forks), plans the
  workflows at that sha via `FileAt`/`ListDir`, and offers one check job per planned job that has
  no `tentmaker/<job>` status yet. It posts `pending` ("running on <device>") when granting and
  the final state from `WorkRequest.Results`. It also writes the result's log tail, with repo,
  sha, check, state, device and runner, to `<state>/checks/<owner>_<repo>-<sha12>-<job>.log` (mode
  0600, newest 200 kept), since the worker deletes the job dir when the job ends. A worker that goes silent for 2 minutes gets its
  pending statuses set to `error` ("device went offline") so they can be retried by a re-run.
- **Source archive relay.** Forge tokens never leave the broker. The bundle the broker sends has an
  `ArchiveRef` (32 random bytes, hex) and no URL or header. The broker keeps, in memory only, ref
  -> (runner, granted device, forge, repo, sha) while that check is active and drops it when the
  check ends. `Broker.FetchArchive(ctx, ref, device, w)` (core.ArchiveSource) serves it only to
  the granted device, at most 3 times, by calling `StatusForge.Archive` itself and streaming the
  download (redirects followed, the header sent only to the archive's own host, 10 min, 2 GiB).
  A remote broker is reached over the mesh (`GET /v1/archive/<ref>`). The worker, for each check
  offer, mints a relay id (32 random bytes, hex) bound to (source, ref, runner), expiring with
  the job and dropped when the job ends or the worker restarts, and rewrites the bundle's
  `ArchiveURL` to `http://127.0.0.1:7434/v1/relay/<id>` (the control API's address). The control
  API serves `GET /v1/relay/<id>` on loopback without a token; an unknown or expired id is 404.
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
devices whose `/v1/hello` pool name matches. Endpoints: `GET /v1/hello` (device, pool, version,
broker yes/no), `GET /v1/status`, `POST /v1/work` (core.WorkRequest -> core.WorkResponse),
`POST /v1/claim`, `POST /v1/release`, `GET /v1/archive/<ref>` (a check's source archive, streamed
from the broker to the device named in the `Tentmaker-Device` header; the broker checks that it
granted the check to that device; 404 otherwise). Bodies are JSON, capped at 1 MiB, 10 s
timeouts, except the archive stream (10 min, 2 GiB). Peers
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
- Auto-update has no one watching, so the daemon guards itself: before it exits into the new
  binary it writes `<state>/update-pending.json` (from, to, time). The new binary deletes it once
  the control API serves and the worker has ticked. If the marker is older than 2 minutes when a
  daemon starts, or the grace ends before the binary gets healthy, the daemon rolls back to
  `<bin>.prev`, adds the version to `<state>/update-bad.json` (auto-update skips those), deletes
  the marker and exits, so the service manager starts the previous binary. A marker for another
  version is deleted.

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

**executors**

- `ForHost` takes a `binary func(core.Platform) (string, error)` that returns the tentmaker binary for a sandbox's platform.
- VM images are package constants; `Options.Images` overrides them.
- macOS guests are optional. darwin `Install` pulls the Linux image whenever it fits (one job's room plus the image), and the macOS image only when free disk covers everything plus a job, or with `setup --macos-guests` (`Options.MacOSGuests`: skips the job margin, but the image itself still has to fit). It records what it pulled in `<state>/macos-tart/guests.json`; the executor offers only those platforms (all pinned ones when the file is missing). When not even the Linux image fits, Install downloads nothing and returns nil, so setup exits 0 and installs the service; `Preflight` then reports the missing image as a problem, and `forHost` keeps the executor on Apple silicon so doctor can say so. An executor's optional `Notes(ctx) []core.Problem` shows in doctor as warnings: "macOS guests are off: the macOS image needs N GiB, M GiB free; run sudo tentmaker setup --macos-guests when you have room".
- Linux `Reset` wipes podman images unless `KeepImages`.
- No cache promotion between jobs in v1.
- The JIT config reaches the runner as `ACTIONS_RUNNER_INPUT_JITCONFIG`, set inside the sandbox by the job script from its stdin hand-off (the runner reads and clears it); never in the daemon's environment or argv.
- `SetPoolLimit` is a no-op on macOS and Windows.
- Linux and Windows keep a copy of the tentmaker binary for check jobs. Setup writes it, and each check `Start` compares it with `Binary(platform)` (size, then sha256; equal pairs are remembered by size and mtime) and replaces it atomically when they differ, so an update never leaves check jobs on the old binary. Linux writes it root-owned 0755; Windows renames the old exe aside first (a running check may hold it) and deletes it once free. (internal/executor/guestbin)
- The Windows runner password uses machine-scope DPAPI, protected by the file ACL (SYSTEM and Administrators).
- Network rules are written at setup. A changed `allow_lan` takes effect only after `sudo tentmaker setup` runs again; until then the loaded rules don't match the config, and Linux (missing LAN rules) or Windows (description mismatch) refuses jobs. Rules stricter than the config pass.
- Linux job units have no `--collect` and set `OOMPolicy=stop`: a unit the OOM killer ended stays loaded with `Result=oom-kill` until `Clean` runs `systemctl reset-failed`, so `CheckResult` can say "ran out of memory" instead of "cancelled" or "no result.json".

**mesh**

- `Options` has `Version` and `Platforms` for `/v1/hello` and pool views.
- Served callers are the same tailnet user OR a login in `members`.
- Tagged devices are served only when `members` contains `tagged-devices`.
- Every endpoint but `/v1/hello` needs a matching `Tentmaker-Pool` header.
- `Claim` walks devices in name order, so exactly one racing broker wins.
- `RemoteInstall` saves install.sh to a temp file and runs the installer itself as root (`sudo -n sh "$f" --channel <c> --yes -- <setup flags>`, or plain `sh` when already root), so install.sh puts the binary on root's PATH and runs setup. It used to run install.sh as the login user and then `sudo -n tentmaker setup`, which failed when the binary landed in `~/.local/bin`.
- `Run` restarts its listener when the tailnet IPs change.

**dispatch**

- Check offers are marked with `Job.Event = "check"`.
- A quarantine lifts on the first request after delivery that asks for work with none of that broker's runners running.
- Boot backoff is per executor.
- A public repo with `allow_public` also needs fork policy `approve_all` or `off`.
- `Broker.Tick` runs one pass (poll due repos, watch runners, handle silent workers, post statuses, save state); `Broker.Shutdown` stops granting and deletes runners that haven't taken a job.

**checks**

- `StatusForge.PendingCommits(ctx, repo, branches...)` also returns the heads of `branches` (config `[[repos]] branches`, `repo add --branch`); a branch that doesn't exist is skipped. Branch heads, like the default branch, need no `authors` match: only people with push access move them.
- The broker keeps each finished check's log tail under `<state>/checks` (newest 200). Before this, a failed `tentmaker/<job>` status pointed at nothing: the worker deletes the job dir and the tail was dropped after posting the status.
- `CheckBundle.Ref` carries the branch.
- `CheckBundle.ArchiveRef` replaces the forge URL and header the broker used to send (review finding: a check job could read the forge token from its bundle). `Header` stays in the struct only so a worker can recognize and refuse an older broker's bundle.
- An empty `Platform.Arch` means any arch.
- `ListDir` returns sorted entry names, and `ErrNotFound` for a missing folder.
- GitLab `after_script` runs as a final continue-on-error step, not as an always-run step.

**release**

- `tentmaker version --json` prints version, os, arch and channel.
- The dev release title is the exact dev version; `internal/update` reads it from there.
- `update.New(Options)` builds an `Updater`; package-level `Check`, `Download`, `Apply` and `Rollback` wrap a default one.
- install.sh test hooks: `TENTMAKER_DOWNLOAD_BASE` replaces the release URL (a URL or a local directory), `TENTMAKER_SUDO` replaces the sudo command.
