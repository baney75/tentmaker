---
name: tentmaker
description: >-
  Tentmaker self-hosted CI on the user's own machines. Use when asked to "set up
  self-hosted CI", "add my desktop to the CI pool", run "runners on my own
  machines", run GitHub Actions on a laptop or desktop, or install, fix, extend
  or update tentmaker on Linux, macOS or Windows devices.
---
# Tentmaker

Tentmaker runs CI jobs in throwaway sandboxes on the user's own devices, pooled
over Tailscale. You drive it with the `tentmaker` CLI. The command contract is
https://github.com/baney75/tentmaker/blob/main/docs/CLI.md; use only commands
and flags it defines.

## Contract
- Pass `--json` to every command that prints: exactly one JSON object on
  stdout, errors as `{"error": "...", "fix": "..."}`. Do what `fix` says.
- Pass `--yes` to `setup` and `uninstall`. Nothing prompts without a terminal.
- Exit codes: 0 ok, 1 failed, 2 usage error (the message names the flag),
  3 needs root or Administrator, 4 not set up yet.
- A forge token goes in only through `gh auth token` (setup runs it for you)
  or a pipe into `--token-stdin`. Never put a token in argv, an environment
  variable, a file or your reply.

## Ask the human first
Get a yes for each of these, naming the exact repo, device or cost:
- **Spending:** anything that costs money (a paid plan, hosted minutes, hardware).
- **Public repos:** `--allow-public`. Approved outside contributors' code would
  run on their machines.
- **A forge token on a device:** setup, `connect`, `devices add` or the
  installer on a device where `gh` is signed in. That device becomes a broker.
- **Writing their files:** `migrate --write`, commits, pushes and PRs.
- **Removing:** `uninstall --purge` deletes config, secrets and state.

## Steps

### 1. Check what is there
```sh
tentmaker version --json; tentmaker status --json
```
Done when you know which case holds: not installed (command not found), not
set up (exit 4), or running (exit 0). If running, skip to step 5.

### 2. Install the binary
Linux and macOS:
```sh
curl -fsSL https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.sh | sh -s -- --no-setup
```
Windows, in an Administrator PowerShell:
```powershell
$env:TENTMAKER_NO_SETUP = '1'; irm https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1 | iex
```
Use `--channel dev` (or `$env:TENTMAKER_CHANNEL = 'dev'`) only when the human
asks for dev builds. Done when `tentmaker version --json` exits 0. If the
installer says the directory is not on PATH, call the binary by the full path
it printed.

### 3. Get root
Setup and every command that changes the device need root or Administrator.
Check: `sudo -n true` exits 0 (Windows: `net session` exits 0). If it fails, you
can't answer a password prompt: give the human the exact command from step 4
to run in their own terminal, wait, then go to step 5.

### 4. Set up
GitHub, token from `gh` (check `gh auth status` first):
```sh
sudo tentmaker setup --yes --json --repo OWNER/NAME --memory 4GiB --boost-memory 12GiB
```
Leave out `--memory` and `--boost-memory` to take the defaults (a quarter and
half of RAM). Pass a token from another command with a pipe:
```sh
gh auth token | sudo tentmaker setup --yes --json --repo OWNER/NAME --token-stdin
```
Worker only, no token on this device: `--no-gh` and no `--repo`.
Forgejo, Gitea or GitLab: `--forge forgejo|gitlab --forge-url URL --mode checks
--token-stdin`. The human makes the token; have them run the command and paste
it on stdin, then press Enter and Ctrl-D.
One device with no Tailscale: add `--no-mesh`.
Done when setup exits 0 and every entry in its `steps` succeeded.

### 5. Verify
```sh
tentmaker doctor --json
```
Done when it exits 0. Otherwise apply each problem's fix and rerun. Stop and
report after the same problem survives two fixes.

### 6. Point workflows at the pool
In a checkout of the repo:
```sh
tentmaker migrate PATH --json                     # report only
tentmaker migrate PATH --platform macos --json    # macOS jobs
tentmaker migrate PATH --write --json             # edit .github/workflows
```
Show the human the report, then with their yes run `--write`, review the diff,
and commit on a branch. Migrated jobs keep fork pull requests on hosted runners.
By hand, a job uses `runs-on: [self-hosted, tentmaker, linux]` (or `macOS`,
`Windows`). Checks-mode repos need no workflow change.
Done when the human has the diff or the PR.

### 7. Add repos
```sh
sudo tentmaker repo add OWNER/NAME --json
sudo tentmaker repo add team/app --forge forgejo --mode checks --json
```
GitLab repos always use checks mode. Forgejo and Gitea use checks mode in v0.1.
Add `--allow-public` only with the human's yes. Done when `tentmaker repo list
--json` shows it and `tentmaker doctor --json` exits 0.

### 8. Add other devices
From a set-up device, over Tailscale SSH:
```sh
tentmaker devices add HOST --json
tentmaker devices add HOST --memory 4GiB --boost-memory 16GiB --json
```
The host must allow Tailscale SSH and passwordless `sudo`. A Windows host gets
back the PowerShell line to run there; hand it to the human. If `gh` is signed
in on that host and the human doesn't want it to hold a token, install there
instead with `sh -s -- --yes -- --json --no-gh`. Done when `tentmaker pool
--json` lists the new device.

## Everyday changes
- Budget: `sudo tentmaker budget set --memory S --boost-memory S --on-battery everyday|off`
- Schedule: `sudo tentmaker schedule add --name N --days mon-fri --from 22:00 --to 07:00 --profile boost`
- Pause and resume: `sudo tentmaker pause --for 2h REASON`, `sudo tentmaker resume`
- Why no jobs run: `tentmaker budget --json`, then `tentmaker logs -n 200`
- Update: `sudo tentmaker update --json`; undo with `sudo tentmaker rollback`

## MCP
`claude mcp add tentmaker -- tentmaker mcp` gives you the same actions as
tools. Tools that change the device fail unless the server runs as root or
Administrator; use the CLI with sudo for those.
