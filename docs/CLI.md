# Tentmaker command line (contract)

This file is the contract for `internal/cli`, `internal/daemon`, `internal/control` and
`internal/service`. README and agent docs are written from it. Change it in the same PR as
any command change.

Every command that prints takes `--json` and then prints exactly one JSON object on stdout
(errors too: `{"ok": false, "error": "...", "fix": "..."}`) and nothing else. When a forge's rate
limit refused a call, the error also has `retry_after_s`: wait that many seconds and run the same
command again. The error then says so ("GitHub rate limit reached for this token; it resets at
05:18 UTC (in 12 min)") and the fix says the token is fine, never to make a new one. When stdout is not a
terminal (a pipe, a file, an agent) the object is on one line; on a terminal it is indented, unless
`TENTMAKER_JSON_ONELINE` is set (the installers set it, since an agent's terminal is often a pty). Every command and subcommand
takes `--help`, which prints its usage and exits 0. Exit codes: 0 ok, 1 failed,
2 usage error, 3 needs root/Administrator, 4 not set up yet. No command ever prompts when
stdin is not a terminal or `--yes` is given; a missing required answer is a usage error that
names the flag. Secrets are read only from stdin (`--token-stdin`) or from `gh auth token`,
never from a flag or an environment variable.

## Commands

```
tentmaker                         Status if set up; otherwise a three-line hint to run setup.
tentmaker setup [flags]           Install and configure this device (wizard on a TTY).
tentmaker status [--json] [--watch]
tentmaker pool [--json]           Every device in the pool (mesh).
tentmaker budget [--json]         Show the budget and the current decision with reasons.
tentmaker budget set [--memory S] [--boost-memory S] [--cpus N] [--boost-cpus N]
                     [--on-battery everyday|off] [--boost-needs-idle true|false]
                     [--pause-below PCT] [--stop-below PCT]
tentmaker schedule list|add|remove
    add --name N --days mon-fri|sat,sun|daily --from HH:MM --to HH:MM --profile everyday|boost|off
tentmaker repo list
tentmaker repo add OWNER/NAME [--forge github|forgejo|gitlab] [--mode runner|checks]
                   [--allow-public] [--workflow PATH]... [--author LOGIN]... [--branch NAME]...
                   [--enable]
tentmaker repo remove OWNER/NAME
tentmaker connect github|forgejo|gitlab [--url URL] [--token-stdin]
tentmaker devices [--json]        Every device in the pool: owner login, trust, repos,
                                  online or asleep, lent or not.
tentmaker devices remove NAME     Revoke a lent device at once (needs root).
tentmaker devices add HOST [--channel stable|dev] [--memory S] [--boost-memory S] [--repo R]...
tentmaker invite [--trust trusted] [--repos owner/a,owner/b] [--ttl 24h]
                                  A one-time code that lends another user's device to this
                                  pool, printed once (needs root).
tentmaker join                    Lend this device to a pool: the code is read from the
                                  terminal without echo, or from stdin (needs root).
tentmaker leave                   Drop out of the pool this device joined (needs root).
tentmaker migrate [PATH] [--platform linux|macos|windows] [--write] [--all]
tentmaker migrate --pr [PATH] [--platform linux|macos|windows] [--all]
tentmaker pause [--for DURATION] [REASON...]
tentmaker resume
tentmaker doctor [--json]        Every check; with a problem, ends with "first fix: <fix>"
                                  (`first_fix` in --json). A forge check a rate limit
                                  stopped says so and carries `retry_after_s`.
tentmaker logs [-f] [-n LINES]   The service's own log.
tentmaker logs JOB-ID [--follow] [-n LINES]
                                  One job's redacted log tail, from whichever device ran it.
tentmaker runs [--repo R] [--sha SHA] [--failed] [-n 20] [--json]
                                  The pool's run history, newest first, running jobs included.
tentmaker rerun JOB-ID|RUN-URL    Rerun the run's failed jobs through the forge (needs root).
tentmaker stats [--since 7d] [--repo R] [--json]
                                  Queue wait and duration p50/p95, minutes, estimated saving.
tentmaker alerts test             Send a test message to every [[alerts]] webhook (needs root).
tentmaker tests [JOB-ID] [--repo R] [--failed] [-n 20] [--json]
                                  A job's test cases, failed first; without an id, recent jobs with tests.
tentmaker flaky [--repo R] [--since 14d] [--json]
                                  Tests that failed and passed on the same commit, or flip on a branch.
tentmaker why JOB-ID | --repo R --sha SHA | --repo R --run N [--json]
                                  Why a job failed: step, failing tests, log excerpt, flaky verdict.
tentmaker debug JOB-ID [--exec CMD] [--timeout 60s] [--max-bytes N] [--release] [--json]
                                  Shell or one command in a failed job's kept sandbox (needs root).
tentmaker update [--channel stable|dev] [--check]
tentmaker rollback
tentmaker uninstall [--purge] [--yes]
tentmaker sandbox create [--os linux|macos|windows] [--device D] [--cpus N] [--memory S]
                         [--ttl 30m] [--copy DIR]
tentmaker sandbox exec ID [--dir D] [--timeout D] [-i] -- COMMAND [ARGS...]
tentmaker sandbox cp LOCAL ID:FOLDER | ID:PATH LOCAL_FOLDER
tentmaker sandbox ls
tentmaker sandbox rm ID...
tentmaker mcp                     Stdio MCP server for agents (internal/mcp over a Controller).
tentmaker version [--json]        --json prints {"version": "X", "os": "...", "arch": "...", "channel": "..."}.
tentmaker daemon                  Run by the service. Not for people.
tentmaker check-run --out DIR     Run inside a sandbox: CheckBundle JSON on stdin, writes
                                  DIR/result.json and DIR/log.txt. Not for people.
```

Commands that change the device (setup, budget set, schedule add/remove, repo add/remove,
connect, pause, resume, update, rollback, uninstall, rerun, alerts test, debug) need root or
Administrator and exit 3 with the exact command to rerun (`sudo tentmaker ...`, or "open
PowerShell as Administrator"). status, pool, budget, repo list, doctor, logs, runs, stats,
tests, flaky, why, version and migrate do not.

## Run history: runs, logs, rerun, stats

Every device keeps one record per job it ran in `<state>/runs` (the last `history_max`,
default 2000) and a redacted log tail of up to 512 KiB per job. With the mesh on, `runs`,
`logs JOB-ID`, `rerun` and `stats` ask every device in the pool and merge the answers; a device
that doesn't answer is listed under `errors` (`not counted:` in text), never silently dropped.
Without the service, `runs` and `logs` read this device's own history and say so.

- `runs --json` prints `{"service", "records": [Record], "queued": [...], "errors": [...]}`. A
  Record has `id` (the job id, the runner name, like `tm-box-1a2b3c`), `repo`, `workflow`,
  `job`, `run_id`, `run_url`, `sha`, `device`, `platform`, `queued_at`, `started_at`,
  `ended_at`, `conclusion` (`success`, `failure`, `cancelled`, `error`, `unknown`, `running`),
  `failed_step`, `note`, `peak_memory_bytes`, `cpu_seconds`, `cache_bytes` and `has_log`.
  `-n` is 1 to 5000.
- `logs JOB-ID` prints the tail as it is; `-n` keeps the last lines; `--follow` (or `-f`)
  polls every second until the job ends. `--json` prints `{"id", "device", "running",
  "lines"}`. The tail is redacted: tokens, JIT configs, bearer and basic credentials and
  URLs with passwords are replaced with `***` before the device stores or serves it. Any
  local user but the CI user reads job logs, so an agent's MCP server needs no root.
- `rerun` takes a job id or a run URL (`https://github.com/OWNER/NAME/actions/runs/ID`) and
  asks the broker that serves the repo to rerun the run's failed jobs. The forge token stays
  on the broker. Checks-mode jobs have no forge run and are refused with a fix.
- `stats --json` prints `{"since", "until", "repos": [...], "total", "estimate"}`, each repo
  with `jobs`, `failed`, `failure_rate`, `queue_wait_p50_s`, `queue_wait_p95_s`,
  `duration_p50_s`, `duration_p95_s`, `minutes`, `billed_minutes` and
  `estimated_saving_usd`. The saving is billed minutes (each job rounded up, as GitHub bills)
  at GitHub-hosted per-minute rates, labeled as an estimate. `--since` takes `h` and `d`
  windows up to 3650 days.
- `alerts test` sends one test message through every alert and prints how each went; it exits
  1 when any failed. Errors name the webhook's host only, never its URL.

## Test results and failures: tests, flaky, why, debug

When a runner job ends, its device reads the JUnit XML reports in the job's workspace and any
`go test -json` lines in its log, and keeps the cases (failed first, at most 500 failed and 5000
in all, messages capped and redacted) next to the record; the record carries a `tests` summary
(`total`, `passed`, `failed`, `skipped`, `failures`). Like `logs`, these commands ask the device
that ran the job, and read this device's history when the service is not running.

- `tests JOB-ID` prints the counts, then each failed test with its message and output, then the
  rest. `--failed` keeps the failed ones. `--json` prints `{"record", "results"}`, where
  `results` has `cases` (`suite`, `name`, `status`, `seconds`, `message`, `detail`, `file`,
  `source`), `files`, `problems` and `dropped`. Without a job id it lists the newest jobs that
  reported tests (`--repo`, `--failed`, `-n`) as `{"service", "runs": [...]}`.
- `flaky` lists tests that look flaky in the pool's records since `--since` (default 14 days):
  a test that failed in one run and did not fail in a later run of the same job on the same
  commit (`same-commit`), or one that changed between failing and passing at least twice in the
  job's last 10 runs on one ref (`flips`). `--json` prints `{"since", "tests": [...]}`.
- `why` explains one failed job: a job id, or `--repo` with `--sha` or `--run` (or a run URL)
  for the newest failed job there. It prints the conclusion, repo, ref, short sha, failed step,
  up to 5 failing tests with their messages, the log lines around the first line that looks
  like an error (10 before, 20 after; the last 30 lines when none does) and a flaky verdict: the
  same job passed on the same commit, or a failing test is flaky in the last 14 days.
  `kept_until` is set while the job's sandbox is kept. `--json` prints the runs.WhyAnswer.
- `debug JOB-ID` opens an interactive shell, on a raw terminal, in a failed job's sandbox kept
  on this device by `[repos.debug] keep_failed` (docs/config.md). It runs as the job's user with
  the job's limits and network fence. `--exec CMD` runs one command with `/bin/sh -c`, prints
  its output (redacted; its last `--max-bytes`, default 64 KiB, at most 1 MiB) and exits with
  the command's exit code; `--timeout` is up to 10m (default 60s). `--json` (with `--exec`)
  prints `{"id", "exit_code", "output", "truncated", "timed_out"}`. `--release` ends the grace
  now and the sandbox is cleaned up. A job kept on another device is refused with that
  device's name: debug only reaches this device, never over the mesh.

## status

`tentmaker status --json` prints the daemon's view: version, device, the budget decision and
why, power, pause, worker and broker state, mesh, warnings and problems. When the service is
running it also has a `runtime` section about the daemon process itself:

```json
"runtime": {
  "pid": 4242,
  "uptime_s": 86400,
  "goroutines": 41,
  "heap_in_use_bytes": 9437184,
  "sys_bytes": 25165824,
  "gc_cycles": 310,
  "open_fds": 17
}
```

`open_fds` counts the process's open file descriptors on Linux and macOS and is -1 on Windows.
On a device that runs for months these numbers should level off; one that only grows (heap,
goroutines or open files) is a leak worth a bug report. When the service is not running,
status is computed locally and has no `runtime` section.

`mesh.pool` is the pool this device works for, `mesh.lent` the pool it is lent to (pool, host
device, owner login, trust, repos) and `mesh.guests` the devices lent to this pool (node ID,
login, name, trust, repos, joined, and revoked when removed).

## setup

Flags (all optional; with `--yes` every missing value takes its default):

```
--yes --json
--device NAME            default: hostname, lowercased, [a-z0-9-] only
--memory S               everyday memory, default config.Default
--boost-memory S         plugged-in memory, default config.Default
--cpus N --boost-cpus N
--on-battery everyday|off   default everyday
--repo OWNER/NAME        repeatable
--forge github|forgejo|gitlab     default github
--forge-url URL          for forgejo/gitlab self-hosted
--mode runner|checks     default runner for github; forgejo, gitea and gitlab run in checks mode only (v0.1)
--token-stdin            read the forge token from stdin
--no-gh                  don't try `gh auth token`
--pool NAME              default "default"
--no-mesh                single device, no Tailscale
--channel stable|dev     update channel, default: the channel this binary came from
--no-service             write config and install executors, don't install the service
--macos-guests           macOS: pull the macOS VM image (about 80 GiB) even when free disk
                         doesn't cover it plus room for a job; it still has to fit
--repo-here              add the GitHub repo of the current directory's `origin` as --repo,
                         with no prompt; outside a checkout it is a usage error
--pr                     after doctor passes, run `tentmaker migrate --pr` in the current
                         checkout (github only)
```

Steps, each reported as one line on a TTY and as an entry in `steps` with `--json`:

1. Check root/Administrator, OS, arch, RAM, CPUs; write nothing yet.
2. Wizard (TTY only, skipped with --yes): device name, everyday memory, boost memory, on
   battery behavior, repos (offer the `origin` of the current directory's git repo), forge
   connection. Each prompt shows the default in brackets; Enter takes it.
3. Forge connection: with `--token-stdin` read it; else unless `--no-gh` run `gh auth token`
   (as `$SUDO_USER` when run under sudo: `sudo -u "$SUDO_USER" gh auth token`); else on a
   TTY print where to make a token (fine-grained, the needed permissions listed by mode)
   and read it without echo. One API call checks the token before it is stored as secret
   `<forge>-token` (`forgejo-token` keyed by host when a URL is set: `forgejo-<host>-token`).
   When that call is rate limited, setup stops here with the reset time and `retry_after_s`,
   before it saves anything; run the same command after the reset. No token is fine: the
   device becomes a worker that takes work from brokers in its pool.
4. Validate and save config (config.Save), then `executor.Install` for this OS. On a Mac the
   Linux guest image is always installed when it fits; the macOS image only when free disk
   covers it with room for a job, or with `--macos-guests`. When it is skipped, the Mac offers
   only linux/arm64 jobs, setup still succeeds, and doctor says: "macOS guests are off: the
   macOS image needs N GiB, M GiB free; run sudo tentmaker setup --macos-guests when you have
   room". When not even the Linux image fits, setup downloads nothing, still installs the
   service and exits 0, and doctor reports the problem.
5. Install and start the service (internal/service), wait up to 20 s for the control API.
6. Run doctor and print its result plus the next step. When doctor finds a problem, the doctor
   step and `next` name the first failed check's fix.
7. With `--pr`, when every step and doctor passed (warnings are fine): `migrate --pr` in the
   current checkout, reported as the step `pr` and as `pr` in `--json`. When doctor failed, no
   pull request is opened and the step says to fix doctor, then run `tentmaker migrate --pr`.

`setup --json` adds `timings`, the wall time of each phase in order, and `total_ms`, their sum:
`check`, `wizard`, `token`, `save`, the executor install as `sandbox.<phase>` (Linux:
`preflight`, `user`, `dirs`, `runner`, `files`, `net`, `user-manager`, `podman`, `podman.check`,
`runner.wait`, `baseline`, where the actions/runner download starts at `runner`, overlaps the
phases after it, and `runner.wait` is what is left of it;
macOS: `preflight`, `tart`, `softnet`, `vms`, `images`; Windows: `preflight`, `dirs`, `user`,
`net`, `files`, `runner`), `service.install`, `service.wait`, `doctor`, and `pr` with `--pr`.
The human output ends with `setup took N s`.

The one line that takes a checkout from nothing to a pull request:

```sh
curl -fsSL https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.sh | sh -s -- --yes -- --repo-here --pr
```

Running setup again is safe: it keeps existing values unless a flag overrides them.

## migrate

`tentmaker migrate` reads the workflows under `.github/workflows` of PATH (default: the current
directory) and prints how each GitHub-hosted `runs-on` would change; `--write` writes the files.
A moved job keeps fork pull requests on the hosted runner:

```yaml
runs-on: ${{ github.event.pull_request.head.repo.fork && 'ubuntu-latest' || fromJSON('["self-hosted","tentmaker","linux"]') }}
```

`--all` drops that expression and sends fork pull requests to the pool too, where they wait,
since Tentmaker never runs a fork's job. Migrate leaves a job where it is, and says why, when it
uses `services:` on macOS or Windows, `container:` on Windows, or a GPU or larger-runner label.

`--pr` makes it a pull request. It needs no root; under sudo, git runs as `$SUDO_USER`.

1. PATH must be a git checkout whose `origin` is on github.com, with a clean working tree.
2. It builds the branch `tentmaker/migrate` from `origin/<default branch>`, rewrites the
   workflows there, commits only those files, and pushes with `--force-with-lease`.
3. It opens the pull request through the GitHub API, or updates the open one from that branch.
   The token is `gh auth token` (as `$SUDO_USER` under sudo), else the service's stored GitHub
   token through POST /v1/pr. It never runs `gh` to make the pull request.
4. It puts the checkout back on the branch it was on.

The body has a before and after table of every moved `runs-on`, the fork-safe expression, a note
that the hosted runners still work as the fallback, and the jobs it left alone. `--json` prints
`{ok, repo, branch, base, commit, files, changed, pr: {number, url, created}, token_source, note}`.

## Files

| | Linux | macOS | Windows |
| --- | --- | --- | --- |
| config | /etc/tentmaker/config.toml | /Library/Application Support/Tentmaker/config.toml | %ProgramData%\Tentmaker\config.toml |
| secrets | /etc/tentmaker/secrets | /Library/Application Support/Tentmaker/secrets | %ProgramData%\Tentmaker\secrets |
| state | /var/lib/tentmaker | /Library/Application Support/Tentmaker/state | %ProgramData%\Tentmaker\state |
| check logs | /var/lib/tentmaker/checks | /Library/Application Support/Tentmaker/state/checks | %ProgramData%\Tentmaker\state\checks |
| cache | /var/cache/tentmaker | /Library/Caches/Tentmaker | %ProgramData%\Tentmaker\cache |
| logs | journald (unit tentmaker) | /Library/Logs/Tentmaker/tentmaker.log | %ProgramData%\Tentmaker\logs\tentmaker.log |
| service | systemd unit tentmaker.service | launchd /Library/LaunchDaemons/dev.tentmaker.daemon.plist | Windows service "Tentmaker" |

`state_dir` and `cache_dir` in config override the defaults. Log files rotate at 10 MiB, 5 kept.

## sandbox

A sandbox is a throwaway machine for an agent: the isolation a CI job gets, counted against
the device's budget like a job (see DESIGN.md, Sandboxes). The commands talk to the daemon,
so they need it running, and don't need root: root, an elevated Administrator and the holder
of the control token may use every sandbox; other accounts must be listed in `[sandbox] users`
and see only their own.

- `create` prints the ID (`sbx-` and 12 hex digits); `--json` prints the sandbox's details.
  `--ttl` defaults to 30m and is capped by `[sandbox] max_ttl`. `--cpus` and `--memory`
  default to one job's size. `--copy DIR` copies DIR's working tree into the sandbox's work
  folder (in a Git work tree: tracked files as they are on disk, plus untracked files Git
  doesn't ignore, no `.git`); if the copy fails the sandbox is removed. `--device` names
  this device only in this version. No room, a paused device or an OS this device can't
  run is exit 1 with the reason and a fix.
- `exec` streams the command's stdout and stderr and exits with its exit code. `-i` passes
  stdin; otherwise the command reads nothing. `--timeout` bounds the command; it never runs
  past the sandbox's TTL. With `--json` it prints `{"id", "exit_code", "stdout", "stderr", "seconds"}`
  (each stream capped at 1 MiB, with `stdout_truncated` or `stderr_truncated` set when cut).
- `cp LOCAL ID:FOLDER` copies a file or folder into FOLDER (made if missing; relative to the
  work folder). `cp ID:PATH LOCAL_FOLDER` copies a file or folder out into LOCAL_FOLDER; links
  in it are skipped and counted.
- `ls` lists the caller's sandboxes; `rm` removes them.

## Control API (internal/control)

The daemon serves HTTP on the Unix socket `/var/run/tentmaker/control.sock` (mode 0666) on Linux
and macOS, and on 127.0.0.1:7434 on Windows. The server names every caller from the connection,
never from what it sends: the socket's peer uid (SO_PEERCRED, LOCAL_PEERCRED), or on Windows the
account of the process that owns the TCP connection (GetExtendedTcpTable, then its token). A CI
job (`tentmaker-runner`, and on Linux its subordinate uids) gets 403 on every route; so does a
caller it can't name, and everyone but root and SYSTEM when the runner's ids can't be read. Any
other local user reads GET /v1/status and /v1/pool. GET /v1/doctor makes the daemon call the
forges, so it needs the token or an admin caller (root, SYSTEM or an elevated Administrator); a
`tentmaker doctor` that can't use it runs doctor locally. Run as a user, that local doctor skips the
sandbox checks, which read state only root can read, and says to run `sudo tentmaker doctor`. GET /v1/runs (the runs.Query
fields as URL parameters: repo, sha, id, run_id, failed, since, limit) answers runs.Answer;
GET /v1/runs/log?id=ID&max=BYTES answers runs.LogAnswer and is open to local users but the CI
user. GET /v1/runs/tests?id=ID (runs.TestsAnswer), GET /v1/runs/flaky?repo=&since=
({since, tests, errors}) and GET /v1/runs/why?id=ID, ?repo=&sha= or ?repo=&run_id=
(runs.WhyAnswer) are open the same way. POST /v1/rerun {repo, run_id} and POST /v1/alerts/test need the token.
POST /v1/debug/exec {id, command, timeout_s, max_bytes} ({id, exit_code, output, truncated,
timed_out}), POST /v1/debug/release {id} and POST /v1/debug/shell {id} need the token or an
admin caller; a CI job gets 403 and any other user 401. The shell answers `101 Switching
Protocols` with `Upgrade: tentmaker-debug` and then carries raw terminal bytes both ways until
the shell exits; a job with no kept sandbox gets a JSON 404 instead. The server logs the job id
and exit code, never the command or its output. POST /v1/pause {reason, for_s},
/v1/resume, /v1/reload and /v1/pr need the token in `Authorization: Bearer`, read from
`<state>/control.token` (32 random bytes hex, mode 0600 root, or SYSTEM+Administrators on
Windows), created by the daemon at start. The client sends the token on GETs too when it can
read it. The
CLI writes config itself and then calls /v1/reload. If the daemon isn't running, read-only
commands fall back to computing what they can locally and say "service not running".
POST /v1/pr {repo, head, base, title, body} opens or updates a pull request with the device's
stored GitHub token, for a repo this device serves, and returns {number, url, created}.
POST /v1/invite {trust, repos, ttl_s} answers mesh.InviteCode (the code, shown once), POST
/v1/join {code} answers mesh.Lent, POST /v1/leave answers mesh.Lent and POST /v1/devices/remove
{name} answers the revoked mesh.Guest records; all four need the token. GET /v1/devices answers
[]mesh.PoolDevice and is open like /v1/pool. An invite code travels only in a request or answer
body, never a URL, and no answer or log line names it.

The sandbox routes (`/v1/sandboxes...`, see DESIGN.md) need the `Tentmaker-Client` header on
every request, so a web page can't drive them, and refuse CI jobs like every route. The
caller's uid or SID owns what it creates; the token or an admin caller sees everything.

The source archive relay listens apart, on 127.0.0.1:7435, and serves only GET
/v1/relay/<id> (everything else is 404). CI jobs reach 7435. The caller check, not the fence, keeps them off
the control API: Windows Firewall doesn't filter loopback.

## Daemon (internal/daemon)

Composes, from config: executors (executor.ForHost), budget (budget.New with power, idle,
guards), forges from secrets, a dispatch.Broker when any repo has a token on this device
(StatusForges for checks-mode repos, Planner = checks.Plan), a mesh.Node unless mesh is off
(Broker and Claimer from it), and a dispatch.Worker whose Sources are the local broker
first, then mesh.Brokers. Reload re-reads config and rebuilds what changed without
stopping running jobs. SIGTERM or service stop calls Worker.Shutdown and Broker.Shutdown
within 20 s. Auto-update (update.auto): check every 6 h on the configured channel, apply
only when no job runs, then exit so the service manager restarts the new binary.

The binary the executors put into sandboxes (`Binary(platform)`): this executable when the
platform matches the host; otherwise `<cache>/bin/tentmaker_<os>_<arch>-<version>`, downloaded
and verified with internal/update for the exact running version (dev builds: the dev release).
The version in the name makes an update fetch a fresh copy; copies for other versions are
deleted. A hand build has no release and uses `<cache>/bin/tentmaker_<os>_<arch>` if one is put
there.

## MCP

`tentmaker mcp` serves internal/mcp with a Controller backed by the same code as the CLI.
Register it with: `claude mcp add tentmaker -- tentmaker mcp`. Mutating tools need the
MCP server process to run as root/Administrator; otherwise they return an error saying so.
The run history tools are `runs`, `logs`, `rerun` (mutating), `stats`, `wait_for_run`,
`tests`, `flaky`, `why_failed` and `debug_exec` (mutating); docs/agents.md has their arguments
and the agent loop.
The sandbox tools are the exception: they go through the control API and follow its sandbox
access rules, so they work as any account listed in `[sandbox] users`.
