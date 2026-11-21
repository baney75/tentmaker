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
| `cache_max` | size | `20GiB` | The most `cache_dir` may hold. |

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
| `job.cpus` | number | `2` | CPUs each job reserves, and its hard limit. Must be above zero. |
| `job.max_duration` | duration | `3h` | The sandbox kills a job that runs longer. |
| `job.idle_ttl` | duration | `90s` | A runner that hasn't picked up a job by then is deleted. |
| `job.max_slots` | int | `0` (no cap) | The most jobs at once, on top of what the budget allows. |

## `[guards]`

Guards keep CI out of the way of what the machine is for. A failing guard stops new jobs; running jobs finish, except under `stop_below_disk`.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `guards.pause_when_running` | list of strings | `[]` | Process names, matched without case, that pause CI while they run, like `"steam.exe"` or `"zoom.us"`. |
| `guards.min_free_disk` | size | `20GiB` | Take no new jobs with less free disk than this. |
| `guards.stop_below_disk` | size | `5GiB` | Stop running jobs with less free disk than this. |
| `guards.min_free_memory` | size | `2GiB` | Take a job only if this much memory stays free for you after the job's own memory. |
| `guards.max_load_per_cpu` | number | `0` (off) | Pause CI while the 1-minute load average divided by the CPUs is above this. On Windows it is the fraction of CPU time busy. |

## `[[repos]]`

One entry per repository this device serves. A device with repos and a forge token is a broker; a device without a token takes work from brokers in its pool.

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `forge` | string | `"github"` | Which entry in `[forges]` hosts it: `github`, `forgejo` (also Gitea) or `gitlab`. |
| `name` | string | required | `owner/name`. GitLab subgroups: `group/sub/name`. |
| `mode` | string | `"runner"` | `"runner"` uses the forge's own runner (GitHub). `"checks"` runs workflow jobs in Tentmaker and posts commit statuses. GitLab repos must use `"checks"`. In v0.1, Forgejo and Gitea repos run in checks mode only. |
| `allow_public` | bool | `false` | Run jobs for a public repo. The repo must also require approval for every outside contributor's workflow. |
| `workflows` | list of strings | `[]` | Checks mode: which workflow files to run. Empty means every workflow under `.github/workflows` or `.forgejo/workflows` triggered by `push` or `pull_request`. |
| `authors` | list of strings | `[]` (the repo owner) | Checks mode: whose same-repo pull requests run. |

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

Always plugged in, so boost is the normal state. Overnight it gives CI more; a game pauses it.

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
