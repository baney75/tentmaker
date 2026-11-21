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

The skill tells the agent to stop and ask you before it spends money, serves a public repo, puts a forge token on a device, writes to your workflow files, or purges a device.

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
| `update` | yes | Update to the newest release on a channel, with rollback. |

Tools that change the device need the MCP server process to run as root or Administrator. Run as a normal user, they return an error saying so, and the read tools still work. For those changes, an agent can fall back to the CLI with `sudo`.
