# Tentmaker for agents

Tentmaker is meant to be installed and run by an agent (Claude Code, Codex and others) on the user's behalf, across every device they own. This page is for the person handing it to an agent and for anyone building on it. The procedure an agent follows lives in one place: [skills/tentmaker/SKILL.md](../skills/tentmaker/SKILL.md).

## Hand it to your agent

Paste this:

```text
Read https://raw.githubusercontent.com/baney75/tentmaker/main/skills/tentmaker/SKILL.md
and follow it to set up Tentmaker on this machine for my repo OWNER/NAME.
Ask me before you touch a public repo or give any device a forge token.
```

To keep the skill around, save it where your agent loads skills. For Claude Code:

```sh
mkdir -p ~/.claude/skills/tentmaker
curl -fsSL https://raw.githubusercontent.com/baney75/tentmaker/main/skills/tentmaker/SKILL.md \
  -o ~/.claude/skills/tentmaker/SKILL.md
```

The skill tells the agent to stop and ask you before it spends money, serves a public repo, puts a forge token on a device, writes to your workflow files, or purges a device. It never handles a token itself: for Forgejo, Gitea, Codeberg or GitLab it tells you which token to make and hands you the command that reads it from stdin.

## Sudo and Administrator

Setup and every command that changes a device need root or Administrator. An agent can't type your password. Either give it a session where `sudo -n true` succeeds, or let it hand you the exact command to run and wait for you. On Windows, start the agent from an Administrator PowerShell or run the commands it gives you there.

Adding another device with `tentmaker devices add HOST` runs setup on that host over Tailscale SSH with `sudo -n`, so the host needs Tailscale SSH allowed for you and passwordless sudo. Windows hosts can't take Tailscale SSH; the command returns the PowerShell line to run on that machine.

## The CLI contract

The full contract is [docs/CLI.md](CLI.md). What an agent relies on:

- `--json` on any command that prints gives exactly one JSON object on stdout, including errors: `{"error": "...", "fix": "..."}`.
- Nothing prompts when stdin is not a terminal or `--yes` is given. A missing answer is a usage error that names the flag.
- Secrets come only from stdin (`--token-stdin`) or from `gh auth token`, never from a flag or an environment variable.
- `tentmaker version --json` prints `{"version": "X", "os": "...", "arch": "...", "channel": "..."}`.

| Exit code | Meaning | What to do |
| --- | --- | --- |
| 0 | ok | |
| 1 | failed | read `error` and `fix` |
| 2 | usage error | fix the flag the message names |
| 3 | needs root or Administrator | rerun with `sudo`, or in an Administrator PowerShell |
| 4 | not set up yet | run `sudo tentmaker setup` |

Unattended install and setup in one line:

```sh
curl -fsSL https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.sh |
  sh -s -- --yes -- --json --memory 4GiB --boost-memory 12GiB --repo owner/name
```

```powershell
$env:TENTMAKER_YES = '1'
$env:TENTMAKER_SETUP_ARGS = '--json --memory 4GiB --boost-memory 12GiB --repo owner/name'
irm https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1 | iex
```

Without a terminal, install.sh runs sudo with `-n`, so it fails fast instead of waiting on a password.

## MCP server

`tentmaker mcp` is a stdio MCP server backed by the same code as the CLI. Register it with Claude Code:

```sh
claude mcp add tentmaker -- tentmaker mcp
```

`--root DIR` names the one folder `sandbox_cp` may copy files out of a sandbox into. It defaults to the folder the server starts in, which for Claude Code is the project you opened. Pin it when that isn't what you want:

```sh
claude mcp add tentmaker -- tentmaker mcp --root /home/alice/src/myproject
```

| Tool | Changes the device | What it does |
| --- | --- | --- |
| `status` | no | This device's profile and why, free slots, running jobs, power, guards, version. Call it first. |
| `pool` | no | Every device in the pool with its budget, slots, jobs, version and broker role. |
| `doctor` | no | Every health check, each problem with its fix. |
| `set_budget` | yes | Change `everyday_memory`, `boost_memory`, `everyday_cpus`, `boost_cpus`. |
| `add_repo` | yes | Serve `repo` on `forge` in `mode`; `allow_public` defaults to false. |
| `remove_repo` | yes | Stop serving a repo and remove its runners. Running jobs finish. |
| `pause` | yes | Take no new jobs until `resume`. Survives restarts. |
| `resume` | yes | Take jobs again. |
| `migrate_workflows` | yes, with `write` | Report (default) or rewrite `runs-on` lines in a local checkout. |
| `install_on_device` | yes, on the other host | Install and set up Tentmaker on a tailnet host over Tailscale SSH. |
| `update` | yes | Update to the newest release on a channel, with rollback. Passing `channel` also saves it for auto-updates (the CLI's `update --channel` does not; use `setup --yes --channel`). |

| `runs` | no | The pool's job history, newest first: `repo`, `sha`, `failed`, `limit` (default 20, at most 500). |
| `logs` | no | A job's redacted log tail: `job_id`, `lines` (default 200, at most 5000). |
| `rerun` | yes, on the forge | Rerun a run's failed jobs: `target` is a job id or a run URL. The token stays on the broker. |
| `stats` | no | p50 and p95 queue wait and duration, minutes and an estimated saving per repo: `since` (default `7d`), `repo`. |
| `wait_for_run` | no | Block until every job for `repo` at `sha` (or the run at `run_url`) has ended, or `timeout_s` (default 600, 10 to 3600) passes. Returns each job's conclusion and, for failures, the failed step and the last 80 log lines. |
| `tests` | no | One job's test cases from its JUnit reports and `go test -json` output: `id`, `failed` (default true: failed tests only). |
| `flaky` | no | Tests that failed and passed on the same commit, or flip on a branch: `repo`, `since` (default `14d`). |
| `why_failed` | no | Why a job failed, small: failed step, up to 5 failing tests with messages, the log around the first error, a flaky verdict. Pass `id`, or `repo` with `sha` or `run_id`. |
| `debug_exec` | yes, in the kept sandbox | Run one `command` in a failed job's kept sandbox on this device: `id`, `timeout_s` (default 60, at most 600), `max_bytes` (default 65536, at most 1 MiB). Returns `exit_code`, `output`, `truncated`, `timed_out`. |
| `sandbox_create` | starts a sandbox | A throwaway machine on this device: `os`, `cpus`, `memory`, `ttl` (default 30m), `copy_dir` (an absolute local folder to put in its work folder). Returns its `id`. |
| `sandbox_exec` | in the sandbox | Run `command` (an argv array) in sandbox `id`, with optional `dir`, `timeout_s` (whole seconds; omit to run until the sandbox expires) and `stdin`. Returns `exit_code`, `stdout` and `stderr`: the last 64 KiB of each, with `stdout_cut_bytes` and `stderr_cut_bytes` saying how much was dropped. |
| `sandbox_cp` | in the sandbox, or under `--root` | Copy `from` to `to`; one of them is `ID:PATH`. Out of a sandbox, `to` must be a folder under the server's `--root` (a relative `to` starts there); any other path is refused after symlinks are resolved. |
| `sandbox_ls` | no | Your sandboxes, with their size and when they expire. |
| `sandbox_rm` | ends the sandbox | Remove a sandbox now. |

Tools that change the device need the MCP server process to run as root or Administrator. Run as a normal user, they return an error saying so, and the read tools still work. For those changes, an agent can fall back to the CLI with `sudo`. The sandbox tools don't need root: they work for any account listed in `[sandbox] users` in the config (root and Administrator always may). Prefer that: run the server as your own account and add it to `[sandbox] users`.

Text inside a sandbox can steer an agent, so treat what comes out of one as untrusted. That is why `sandbox_cp` writes only under `--root`: an agent told to copy a file over `~/.bashrc` or `/etc/passwd` gets a refusal, even when the server runs as root. Inside the root, the copy replaces a symlink or an existing file instead of writing through it, so it never changes a file outside the root.

## The agent loop

An agent that pushes a fix can watch its own CI without a browser or a forge token:

1. Push the commit.
2. Call `wait_for_run` with `repo` and `sha` (7 or more characters). It waits until every
   Tentmaker job for that commit has ended and nothing more has queued for 20 s, then returns
   `conclusion` and one entry per job.
3. On `success`, stop. On `failure`, each failed job carries `failed_step` and `log_tail`, the
   last 80 lines of its redacted log, which is usually enough to see the error. For a GitHub
   runner job the log is the job's own log from GitHub, with each step's output, which the
   broker reads with its token once the job ends (the record says `log_source: "forge"`). If
   the broker can't read it, the record falls back to the runner's trace log and says
   `log_source: "runner-diag"`; that log names the failed step but holds no test output. For more, call
   `logs` with the job's `id` and a bigger `lines`.
   On a failure, `why_failed` with the job's `id` is the short answer: the failed step, the
   failing tests with their messages, the log lines around the first error and whether it looks
   flaky. When the repo sets `[repos.debug] keep_failed`, `why_failed` has `kept_until` and the
   failed job's sandbox is still on the device that ran it: `debug_exec` reruns one test or
   reads a file the job left (`go test -run TestName ./pkg`, `cat out/report.txt`) without a new
   push. It runs as the job's user under the job's limits, needs the MCP server to run as root,
   and works only on the device that ran the job.
4. Fix, push and go back to step 2. For a flaky failure with no code change, call `rerun` with
   the job id instead (it needs the MCP server to run as root).
5. `timed_out: true` means jobs were still running; call `wait_for_run` again. A `note` that
   says no job was seen means Tentmaker never got the commit's jobs: check `runs-on` with
   `migrate_workflows` and the device with `doctor`.

The same loop works from a shell: `tentmaker runs --sha SHA --json`, `tentmaker logs JOB-ID`,
`sudo tentmaker rerun JOB-ID`.

## Sandboxes

An agent can get a throwaway machine on one of your devices, run commands in it and throw it away:

```sh
id=$(tentmaker sandbox create --ttl 1h --copy .)
tentmaker sandbox exec "$id" -- go test ./...
tentmaker sandbox cp "$id":coverage.out .
tentmaker sandbox rm "$id"
```

A sandbox gets the same isolation a CI job gets and counts against the device's budget like one, so it can't take more than you gave CI. It always ends: when its time to live runs out, on `rm`, or when the service restarts. Its network is the job fence: it reaches the internet, not your LAN or tailnet. This version runs Linux sandboxes on the device you call from; macOS and Windows sandboxes and placement on other pool devices come later.

To let your own account use sandboxes without sudo, add it to the config:

```toml
[sandbox]
users = ["alice"]
```

Code in a sandbox runs as the same Linux user as CI jobs on that device, so give sandboxes only to agents you'd trust with a CI job there.
