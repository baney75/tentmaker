# Tentmaker command line (contract)

This file is the contract for `internal/cli`, `internal/daemon`, `internal/control` and
`internal/service`. README and agent docs are written from it. Change it in the same PR as
any command change.

Every command that prints takes `--json` and then prints exactly one JSON object on stdout
(errors too: `{"error": "...", "fix": "..."}`) and nothing else. Exit codes: 0 ok, 1 failed,
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
tentmaker devices add HOST [--channel stable|dev] [--memory S] [--boost-memory S] [--repo R]...
tentmaker migrate [PATH] [--platform linux|macos|windows] [--write]
tentmaker pause [--for DURATION] [REASON...]
tentmaker resume
tentmaker doctor [--json]
tentmaker logs [-f] [-n LINES]
tentmaker update [--channel stable|dev] [--check]
tentmaker rollback
tentmaker uninstall [--purge] [--yes]
tentmaker mcp                     Stdio MCP server for agents (internal/mcp over a Controller).
tentmaker version [--json]        --json prints {"version": "X", "os": "...", "arch": "...", "channel": "..."}.
tentmaker daemon                  Run by the service. Not for people.
tentmaker check-run --out DIR     Run inside a sandbox: CheckBundle JSON on stdin, writes
                                  DIR/result.json and DIR/log.txt. Not for people.
```

Commands that change the device (setup, budget set, schedule add/remove, repo add/remove,
connect, pause, resume, update, rollback, uninstall) need root or Administrator and exit 3
with the exact command to rerun (`sudo tentmaker ...`, or "open PowerShell as
Administrator"). status, pool, budget, repo list, doctor, logs, version and migrate do not.

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
```

Steps, each reported as one line on a TTY and as an entry in `steps` with `--json`:

1. Check root/Administrator, OS, arch, RAM, CPUs; write nothing yet.
2. Wizard (TTY only, skipped with --yes): device name, everyday memory, boost memory, on
   battery behavior, repos (offer the `origin` of the current directory's git repo), forge
   connection. Each prompt shows the default in brackets; Enter takes it.
3. Forge connection: with `--token-stdin` read it; else unless `--no-gh` run `gh auth token`
   (as `$SUDO_USER` when run under sudo: `sudo -u "$SUDO_USER" gh auth token`); else on a
   TTY print where to make a token (fine-grained, the needed permissions listed by mode)
   and read it without echo. Store as secret `<forge>-token` (`forgejo-token` keyed by
   host when a URL is set: `forgejo-<host>-token`). No token is fine: the device becomes a
   worker that takes work from brokers in its pool.
4. Validate and save config (config.Save), then `executor.Install` for this OS. On a Mac the
   Linux guest image is always installed when it fits; the macOS image only when free disk
   covers it with room for a job, or with `--macos-guests`. When it is skipped, the Mac offers
   only linux/arm64 jobs, setup still succeeds, and doctor says: "macOS guests are off: the
   macOS image needs N GiB, M GiB free; run sudo tentmaker setup --macos-guests when you have
   room". When not even the Linux image fits, setup downloads nothing, still installs the
   service and exits 0, and doctor reports the problem.
5. Install and start the service (internal/service), wait up to 20 s for the control API.
6. Run doctor and print its result plus the next step.

Running setup again is safe: it keeps existing values unless a flag overrides them.

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

## Control API (internal/control)

The daemon serves HTTP on 127.0.0.1:7434 (loopback only). GET /v1/status, /v1/pool,
/v1/doctor need nothing. POST /v1/pause {reason, for_s}, /v1/resume, /v1/reload need the
token in `Authorization: Bearer`, read from `<state>/control.token` (32 random bytes hex,
mode 0600 root, or SYSTEM+Administrators on Windows), created by the daemon at start. The
CLI writes config itself and then calls /v1/reload. If the daemon isn't running, read-only
commands fall back to computing what they can locally and say "service not running".

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
platform matches the host; otherwise `<cache>/bin/tentmaker_<os>_<arch>`, downloaded and
verified with internal/update for the exact running version (dev builds: the dev release).

## MCP

`tentmaker mcp` serves internal/mcp with a Controller backed by the same code as the CLI.
Register it with: `claude mcp add tentmaker -- tentmaker mcp`. Mutating tools need the
MCP server process to run as root/Administrator; otherwise they return an error saying so.
