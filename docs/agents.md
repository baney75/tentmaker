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

Tools that change the device need the MCP server process to run as root or Administrator. Run as a normal user, they return an error saying so, and the read tools still work. For those changes, an agent can fall back to the CLI with `sudo`.

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
