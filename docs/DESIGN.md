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
  `%ProgramData%\Tentmaker\jobs\<name>` ACLed to that user. Setup gives `%ProgramData%\Tentmaker`
  and the state folder a DACL of their own (SYSTEM and Administrators full, Users read): under
  ProgramData's inherited one any user, a job too, could add files to the state, checks and logs
  folders. The runner starts with
  `CreateProcessAsUser`/`LogonUser` inside a Job Object with `JOB_OBJECT_LIMIT_JOB_MEMORY`,
  CPU rate control and `KILL_ON_JOB_CLOSE`. Platforms: `windows/<arch>`. Linux jobs on Windows
  (WSL2) are roadmap, not v1.

**Network isolation.** Install fences the runner user off and every `Start` checks the fence
first; no rules means no job (fail closed). A Linux job also gets a network namespace of its own
(below); a Windows job shares the host's network stack. Jobs may reach the internet, the DNS
servers the host is configured with, and the source archive relay on 127.0.0.1:7435. Always blocked: the tailnet (100.64.0.0/10, fd7a:115c:a1e0::/48), link-local
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
  relay (`-T 7435`: the job's own 127.0.0.1:7435 reaches the host's), maps no other host loopback
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
  rules match all it sends and only the relay is forwarded to the host). `Install` and `Reset`
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
  job's namespace. `Reset` fails unless the pause
  process and the user manager are both outside the host's network namespace
  (`/proc/<pid>/ns/net` differs from the daemon's), then checks the mounts through `podman
  unshare` and fails on any difference. Doctor (`Preflight`) reports either one in the host's
  namespace, as after a reboot until the daemon's first reset. Without the bus the systemd cgroup manager cannot work
  (crun would ask the system bus, which refuses), so setup writes the runner's
  `~/.config/containers/containers.conf` (root-owned) with `cgroup_manager = "cgroupfs"`, and jobs
  load it through `CONTAINERS_CONF_OVERRIDE`. A job's containers then stay in the job's cgroup.
- Linux (`netrules.go`): nftables table `inet tentmaker`, an output hook that sends every packet
  with `meta skuid { <runner uid>, <its subuid ranges> }` to a `runner` chain (accept relay and DNS,
  then reject). The subordinate uids are a second wall: a rootless container on the host network
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

Worker: each tick (2 s) reads `Budget.Decide`, applies `SetPoolLimit`, reports `Running` and
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
  no `tentmaker/<job>` status yet. It posts `pending` ("running on <device>") when granting and
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
grantee, not the caller, so they are not bound. Bodies are JSON, capped at 1 MiB, 10 s
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
  system folder, gives the binary to Administrators, and a folder named `Tentmaker` (the default
  `%ProgramFiles%\Tentmaker`) gets Administrators as owner and a protected DACL (SYSTEM and
  Administrators full, Users read and execute), because the inherited CREATOR OWNER entry of
  Program Files would name the user. When that folder, or `tentmaker.exe.prev` itself, was owned
  by anyone else before, the installer deletes `tentmaker.exe.prev`, the copy rollback would
  run, which that owner could have planted. Before, every Windows install failed setup with "owned by S-1-5-21-..., not
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
- Linux `Reset` wipes podman images unless `KeepImages`.
- No cache promotion between jobs in v1.
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
