# Configuration

Each device has one TOML file. Every key has a safe default, so an empty file works. `tentmaker setup`, `tentmaker budget set`, `tentmaker schedule` and `tentmaker repo` write it for you. You can also edit it by hand: run `tentmaker doctor` to check it, and restart the service so the daemon reads it.

| OS | Path |
| --- | --- |
| Linux | `/etc/tentmaker/config.toml` |
| macOS | `/Library/Application Support/Tentmaker/config.toml` |
| Windows | `%ProgramData%\Tentmaker\config.toml` |

The environment variable `TENTMAKER_CONFIG` points Tentmaker at another file.

A key Tentmaker doesn't know is an error, not a warning, so a typo can't silently do nothing.

**Sizes** are a number with a binary unit: `512MiB`, `4GiB`, `1TiB`. `4G` and `4GB` also mean GiB. A bare number other than `0` is an error.
**Durations** use Go's format: `90s`, `10m`, `3h`, `1h30m`.

Forge tokens never go in this file. They live in the OS secret store; set them with `tentmaker setup` or `tentmaker connect`.

## Top level

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `device` | string | hostname | This device's name in the pool and in its runner label `tm-<device>`. |
| `labels` | list of strings | `[]` | Extra runner labels, added to `self-hosted`, `tentmaker`, the OS, the arch and `tm-<device>`. |
| `deny_repo_patterns` | list of strings | `[]` | Repos this device's broker never grants jobs for. Shell-style patterns matched without case, like `"acme/*"`. A malformed pattern denies every repo. |
| `state_dir` | string | per OS: `/var/lib/tentmaker`, `/Library/Application Support/Tentmaker/state`, `%ProgramData%\Tentmaker\state` | Runner state, job directories and the control token. |
| `cache_dir` | string | per OS: `/var/cache/tentmaker`, `/Library/Caches/Tentmaker`, `%ProgramData%\Tentmaker\cache` | Downloads such as runner archives, VM images and binaries for other platforms. |
| `history_max` | int | `2000` | How many job records (and their log tails, up to 512 KiB each) this device keeps in `state_dir/runs`. The oldest go first. `0` means the default. |
| `metrics_listen` | string | `""` (off) | A loopback IP and port, like `"127.0.0.1:9437"`, where this device serves `GET /metrics` alone, for a Prometheus server on this device. CI jobs and callers it can't name get 403. Linux and Windows; on macOS it refuses every caller, so scrape the peer API instead. |
| `cache_max` | size | `10GiB` | The most the per-repo build caches under `cache_dir/repos` may hold (Linux). Past it, whole repos go, least recently used first, never one with a job on the device. `0` turns the caches off. |

## `[budget]`

How much of this machine CI may use, and when. See the [budget section of the README](../README.md#the-budget) for how a profile is chosen.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `budget.everyday.memory` | size | a quarter of RAM, at least 2 GiB, at most 8 GiB, rounded down to a whole GiB | Memory CI may use at any time. |
| `budget.everyday.cpus` | number | a quarter of the CPUs, at least 1 | CPUs CI may use at any time. Fractions are fine. |
| `budget.boost.memory` | size | half of RAM, at most 32 GiB, never below everyday | Memory CI may use while plugged in. `0` means the same as everyday. |
| `budget.boost.cpus` | number | half of the CPUs, never below everyday | CPUs CI may use while plugged in. |
| `budget.boost_needs_idle` | bool | `false` | Boost only after you have been away for `idle_after`. A device that can't tell (a Linux machine with no desktop session) then never boosts. |
| `budget.idle_after` | duration | `10m` | How long without keyboard or mouse input counts as away. |
| `budget.on_battery` | string | `"everyday"` | The profile on battery: `"everyday"` or `"off"`. |
| `budget.pause_below_percent` | int | `40` | On battery below this charge, take no new jobs. |
| `budget.stop_below_percent` | int | `15` | On battery below this charge, stop running jobs. |
| `budget.schedule` | list of windows | `[]` | Times that set the profile. See below. |

Rules: memory can't be negative; `boost.memory` is `0` or at least `everyday.memory`; `everyday.memory` is `0` or at least one job's `job.memory`; `stop_below_percent` is not above `pause_below_percent`.

A desktop with no battery counts as plugged in. On Linux an online USB or USB-C supply counts as AC, and batteries in a mouse or keyboard are ignored. On macOS, running from a UPS counts as battery.

### `[[budget.schedule]]`

Each window sets the profile on some days and hours, in local time. The first window that covers the current time wins.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `name` | string | `""` | Shown in `tentmaker budget` as the reason. |
| `days` | list of strings | `[]` (every day) | `mon`, `tue`, `wed`, `thu`, `fri`, `sat`, `sun`. |
| `from` | string | required | Start, `"HH:MM"` in 24-hour time. |
| `to` | string | required | End, `"HH:MM"`. A `to` earlier than `from` spans midnight. |
| `profile` | string | required | `"everyday"`, `"boost"` or `"off"`. |

A `boost` window still needs AC power. On battery it falls back to `on_battery`, and it ignores `boost_needs_idle`. An `everyday` window runs even on battery when `on_battery = "off"`. Low battery (`pause_below_percent`, `stop_below_percent`) always wins over a window.

## `[job]`

How big one job is. A device runs as many jobs at once as fit in the current profile.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `job.memory` | size | `2GiB` | Memory each job reserves, and its hard limit. Must be above zero. |
| `job.cpus` | number | `2` | CPUs each job reserves; slots count it. Must be above zero. On Linux a job may use up to the current profile's CPUs, shared evenly with any other running jobs, so a job alone gets them all. On macOS and Windows it is also the job's hard limit. |
| `job.max_duration` | duration | `3h` | The sandbox kills a job that runs longer. |
| `job.idle_ttl` | duration | `90s` | A runner that hasn't picked up a job by then is deleted. |
| `job.max_slots` | int | `0` (no cap) | The most jobs at once, on top of what the budget allows. |

## `[guards]`

Guards keep CI out of the way of what the machine is for. A failing guard stops new jobs; running jobs finish, except under `stop_below_disk`.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `guards.pause_when_running` | list of strings | `[]` | Process names, matched without case, that drop boost to the reserved everyday budget while they run, like `"steam.exe"` or `"zoom.us"`. Everyday keeps running. |
| `guards.min_free_disk` | size | `20GiB` | Take no new jobs with less free disk than this. |
| `guards.stop_below_disk` | size | `5GiB` | Stop running jobs with less free disk than this. |
| `guards.min_free_memory` | size | `2GiB` | Take a job only if this much memory stays free for you after the job's own memory. |
| `guards.max_load_per_cpu` | number | `0` (off) | Drop boost to the reserved everyday budget while the 1-minute load average divided by the CPUs is above this. On Windows it is the fraction of CPU time busy. |
| `guards.allow_lan` | bool | `false` | Let CI jobs reach private LAN addresses (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16). The tailnet (100.64.0.0/10, fd7a:115c:a1e0::/48), link-local, unique local IPv6, this device's own addresses and the mesh port 7433 stay blocked either way. Linux and Windows; macOS guests never reach the LAN. Run `sudo tentmaker setup` again after changing it: the firewall rules are written at setup, and Tentmaker refuses jobs while the loaded rules don't match. |

## `[[repos]]`

One entry per repository this device serves. A device with repos and a forge token is a broker; a device without a token takes work from brokers in its pool.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `forge` | string | `"github"` | Which entry in `[forges]` hosts it: `github`, `forgejo` (also Gitea) or `gitlab`. |
| `name` | string | required | `owner/name`. GitLab subgroups: `group/sub/name`. |
| `mode` | string | `"runner"` | `"runner"` uses the forge's own runner (GitHub). `"checks"` runs workflow jobs in Tentmaker and posts commit statuses. GitLab repos must use `"checks"`. In v0.1, Forgejo and Gitea repos run in checks mode only. |
| `allow_public` | bool | `false` | Run jobs for a public repo. The repo must also require approval for every outside contributor's workflow. |
| `lend_public` | bool | `false` | No effect yet: public lending is off until issue #61. Runner mode, with `allow_public`: will let devices lent to the pool with public trust (`tentmaker invite --trust public`) run this repo's fork `pull_request` jobs. They get no cache of any kind. Set it only on a repo where no job a Tentmaker runner could take uses a secret: GitHub hands a runner any queued job its labels fit, and the broker stops a runner that took any other job only after it started. |
| `workflows` | list of strings | `[]` | Checks mode: which workflow files to run. Empty means every workflow under `.github/workflows` or `.forgejo/workflows` triggered by `push` or `pull_request`. |
| `authors` | list of strings | `[]` (the repo owner) | Checks mode: whose same-repo pull requests run. |
| `branches` | list of strings | `[]` | Checks mode: branches whose head commit runs besides the default branch, such as a release branch (`tentmaker repo add --branch NAME`). Only people who can push to the repo move a branch, so no author list applies. A listed branch that doesn't exist is skipped. |

### `[repos.debug]`

Keeps a failed job's sandbox for a while so its owner (or an agent) can look inside with
`tentmaker debug JOB-ID` or the `debug_exec` MCP tool. Off by default.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `keep_failed` | duration | `"0s"` (off) | How long a failed job's sandbox stays after the job ends, like `"15m"`. At most `"1h"`. |

Only runner-mode jobs that failed on their own are kept (not cancelled, evicted or timed out
ones), and only on Linux for now; other executors clean up as usual. The job is reported ended
to its broker at once: its runner registration and credentials are deleted from the sandbox and
its relay and Actions cache grant are revoked. The kept sandbox still counts against the budget
(its slot and memory) until the grace ends, `tentmaker debug JOB-ID --release` ends it, or a
guard evicts it. While a repo's job is kept on a device, that device takes no other job of the
repo. `tentmaker status` lists the kept sandboxes. Other repos' jobs on the device run as the
same user and can read the kept workspace until it is cleaned up, so don't keep a repo whose
workflow writes secrets to disk on a device that runs other repos' jobs.

```toml
[[repos]]
name = "baney75/app"

[repos.debug]
keep_failed = "15m"
```

## `[forges.<name>]`

How to reach each forge. A fresh config has one entry, `[forges.github]`.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `kind` | string | `"github"` | `"github"`, `"forgejo"` (Forgejo or Gitea) or `"gitlab"`. |
| `url` | string | `"https://api.github.com"` | The forge's address. GitHub uses its API base; for Forgejo, Gitea and GitLab use the instance you pass to `tentmaker connect --url`. |

## `[mesh]`

How devices on one tailnet form a pool.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `mesh.enabled` | bool | `true` | Join a pool over Tailscale. `false` (`setup --no-mesh`) runs this device alone. |
| `mesh.pool` | string | `"default"` | Devices with the same pool name share work. |
| `mesh.port` | int | `7433` | The peer API port, served on tailnet addresses only. 0 to 65535. |
| `mesh.members` | list of strings | `[]` | Tailnet logins served besides your own. Devices owned by the same tailnet user are always served. Add `"tagged-devices"` to serve tagged devices. |
| `mesh.viewers` | list of strings | `[]` | Tailnet nodes that may read this device's `/metrics` and status page (`/` and `/status`) on the peer API and nothing else: `"tag:monitoring"`, a MagicDNS name like `"grafana"`, or a stable node ID. They get no work, runs, logs or control. |
| `mesh.wake` | bool | `true` | Let the pool wake this device with Wake-on-LAN when a queued job fits it and no awake device can take it. A device on battery or paused (`tentmaker pause`) is never woken. Wake-on-LAN must also be on in the firmware and, on Linux, the network card (`ethtool -s <if> wol g`); `status --json` shows under `wake` whether the pool can wake it and why not. `false` opts out. |

## `[[alerts]]`

Webhooks the device that ran a job calls when it ends. Each device sends alerts for its own jobs,
so put the same `[[alerts]]` on every device that should report.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `url` | string | | The webhook, `http://` or `https://`. Set this or `url_secret`, not both. |
| `url_secret` | string | | The name of a secret that holds the webhook URL, for URLs that are themselves credentials (Slack, Discord). The config is readable by local users; the secrets folder is not. Same name rules as a device. |
| `on` | string | required | `"failure"` (failed or errored jobs), `"recovered"` (a success after a failure of the same repo and job on this device) or `"slow"` (any job that ran at least `min_duration`). |
| `repos` | list of strings | `[]` (all) | Only these repos. Path globs, like `"baney75/*"`, compared without case. |
| `min_duration` | duration | `0` | Only jobs that ran at least this long. Required for `slow`. |

The body fits the URL: `hooks.slack.com` gets Slack's `{"text"}`, a `/api/webhooks/` URL gets
Discord's `{"content"}` with mentions off, `ntfy.sh` or an `ntfy.` host gets a plain text body
with `Title`, `Tags`, `Priority` and `Click` headers, and anything else gets JSON:
`{"event", "repo", "workflow", "job", "device", "conclusion", "failed_step", "run_url",
"duration_s", "ended_at", "id"}`. Every field is redacted. One alert sends at most one message
per repo and job every 15 minutes, each call has a 10 s timeout, and alerts wait in a queue of
64 that drops and counts (one log line a minute) rather than slowing a job down.
`tentmaker alerts test` sends a test message through each.

To keep a Slack URL out of the config on Linux or macOS, write it as a secret file, then name it:

```sh
sudo sh -c 'umask 077; cat > /etc/tentmaker/secrets/slack-ci'   # paste the URL, then Ctrl-D
```

```toml
[[alerts]]
url_secret = "slack-ci"
on = "failure"
repos = ["baney75/*"]

[[alerts]]
url = "https://ntfy.sh/my-ci-topic"
on = "recovered"

[[alerts]]
url = "https://ntfy.sh/my-ci-topic"
on = "slow"
min_duration = "20m"
```

On Windows the secrets are DPAPI-encrypted and there is no command yet to add one by hand, so
use `url` there.

## `[sandbox]`

Sandboxes are throwaway machines agents ask for with `tentmaker sandbox` or the MCP sandbox tools.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `sandbox.users` | list of strings | `[]` | Accounts besides root and Administrators that may make sandboxes: user names, numeric uids, or Windows SIDs. Each sees only its own. |
| `sandbox.max_ttl` | duration | `"4h"` | The longest time to live a sandbox may ask for. 1m to 168h. |

## `[update]`

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `update.channel` | string | `"stable"` | `"stable"` or `"dev"`. No other value is accepted. |
| `update.auto` | bool | `true` | Check every 6 hours and update between jobs. |

## Examples

### A laptop

Modest everyday budget, a big boost only when plugged in and you have stepped away, nothing on battery, and a pause during calls.

```toml
device = "laptop"

[budget]
boost_needs_idle = true
idle_after = "15m"
on_battery = "off"

[budget.everyday]
memory = "4GiB"
cpus = 2

[budget.boost]
memory = "12GiB"
cpus = 6

[guards]
pause_when_running = ["zoom.us", "facetime"]

[[repos]]
name = "owner/app"
```

### A desktop

Always plugged in, so boost is the normal state. Overnight it gives CI more; a game drops it back to the everyday budget.

```toml
device = "desktop"
labels = ["gpu"]

[budget.everyday]
memory = "8GiB"
cpus = 4

[budget.boost]
memory = "24GiB"
cpus = 12

[[budget.schedule]]
name = "work hours"
days = ["mon", "tue", "wed", "thu", "fri"]
from = "09:00"
to = "17:00"
profile = "everyday"

[job]
memory = "4GiB"
cpus = 4

[guards]
pause_when_running = ["steam.exe", "obs64.exe"]
max_load_per_cpu = 0.8

[[repos]]
name = "owner/app"

[[repos]]
forge = "forgejo"
name = "team/service"
mode = "checks"

[forges.forgejo]
kind = "forgejo"
url = "https://forgejo.example.org"
```

### A worker-only device

No repos and no token. It takes jobs from the brokers in its pool and never holds a forge credential.

```toml
device = "spare-mini"

[budget.everyday]
memory = "6GiB"
cpus = 4

[budget.boost]
memory = "12GiB"
cpus = 8

[mesh]
pool = "default"
```
