---
name: tentmaker
description: >-
  Tentmaker self-hosted CI on the user's own machines. Use when asked to "set up
  self-hosted CI", "add my desktop to the CI pool", run "runners on my own
  machines", run GitHub Actions on a laptop or desktop, or install, fix, extend,
  update or remove tentmaker on Linux, macOS or Windows devices.
---
# Tentmaker

Tentmaker runs CI jobs in throwaway sandboxes on the user's own devices, pooled
over Tailscale. You drive it with the `tentmaker` CLI. The command contract is
https://github.com/baney75/tentmaker/blob/main/docs/CLI.md; use only commands
and flags it defines (`tentmaker <command> --help` lists them).

## Contract
- Pass `--json` to every command that prints: one JSON object on stdout, errors
  as `{"error": "...", "fix": "..."}`. Do what `fix` says.
- Exit codes: 0 ok, 1 failed, 2 usage error (names the flag), 3 needs root or
  Administrator, 4 not set up yet.
- Pass `--yes` to `setup` and `uninstall` when you run them; nothing prompts
  without a terminal. A command you hand the human may skip it, but
  `--token-stdin` always needs `--yes`.
- Commands that change a device run under `sudo`. Windows has no sudo: run them
  in a PowerShell running as Administrator. status, pool, budget, repo list,
  doctor, logs, version and migrate need neither.
- A forge token goes in only through `gh auth token` (setup runs it for you)
  or a pipe into `--token-stdin`. Never put a token in argv, an environment
  variable, a file or your reply.

## Ask the human first
Get a yes for each, naming the exact repo, device or cost, even when their
request already names the action:
- **Spending:** anything that costs money (a paid plan, hosted minutes, hardware).
- **Public repos:** `--allow-public` (a `repo add` flag; setup has none). Fork
  jobs never run, but code merged from outsiders, and workflows they can
  trigger on the repo itself (like issue comments), would run on their machines.
- **A forge token on a device:** setup with `--repo`, `connect`, or `devices add
  --repo`. That device becomes a broker. Name the token they must make (Tokens).
- **Writing their files:** `migrate --write`, commits, pushes and PRs.
- **Removing:** `uninstall --purge` deletes config, tokens, state, cache and logs.

## Steps

### 1. Check what is there
`tentmaker version --json; tentmaker status --json`. Done when you know: not
installed, not set up (exit 4), or running (exit 0, skip to step 5).

### 2. Install the binary
```sh
curl -fsSL https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.sh | sh -s -- --no-setup
```
Windows, in a PowerShell running as Administrator (if yours isn't, hand the
human these lines to run in one):
```powershell
$env:TENTMAKER_NO_SETUP = '1'; irm https://raw.githubusercontent.com/baney75/tentmaker/main/scripts/install.ps1 | iex
```
Tell the human a Windows device runs only Windows jobs (`runs-on: [self-hosted,
tentmaker, Windows]`); v0.1 runs no Linux jobs there. Use `--channel dev` (or
`$env:TENTMAKER_CHANNEL = 'dev'`) only when asked. Done when `tentmaker version
--json` exits 0; if the installer printed a full path, use it, also after sudo.

### 3. Get root
`sudo -n true` exits 0 (Windows: `net session`). If not, you can't answer a
password prompt: hand the human the exact step 4 command for their own
terminal, wait, then go to step 5.

### 4. Set up
GitHub, token from `gh` (check `gh auth status` first):
```sh
sudo tentmaker setup --yes --json --repo OWNER/NAME --memory 4GiB --boost-memory 12GiB
```
`--memory` is the everyday budget; `--boost-memory` applies only on AC power
(defaults: a quarter and half of RAM). Also: `--device NAME`, `--cpus N`,
`--boost-cpus N`, `--on-battery everyday|off`, `--pool NAME`, `--no-mesh`,
`--channel stable|dev`. A worker that never holds a token: `--no-gh` and no
`--repo`; it takes jobs from brokers with the same tailnet user and pool name.
Forgejo, Gitea, Codeberg (`--forge-url https://codeberg.org`) and GitLab
(`--forge gitlab`) run in checks mode; the human pastes the token, Enter, Ctrl-D:
```sh
sudo tentmaker setup --yes --json --forge forgejo --forge-url URL --repo OWNER/NAME --token-stdin
```
Done when setup exits 0 and every entry in its `steps` succeeded.

### 5. Verify
`tentmaker doctor --json`. Done when it exits 0; otherwise apply each problem's
fix and rerun. Stop and report after the same problem survives two fixes.
Never call setup or a fix done before doctor exits 0.

### 6. Point workflows at the pool
In a checkout, report first: `tentmaker migrate PATH --json` (add `--platform
macos` for macOS jobs). Stop there: show the human the report and ask. Only
after their yes, run `tentmaker migrate PATH --write --json`, review `git
diff`, and commit on a branch. Tell the human both fork fences: migrated jobs
send fork pull requests to GitHub-hosted runners, and Tentmaker refuses a fork's
job whatever the workflow says. Checks-mode repos need no change. Done when the
human has the diff or the PR.

### 7. Add repos
`sudo tentmaker repo add OWNER/NAME --json`; add `--forge gitlab` or `--forge
forgejo` for those forges (checks mode). A public GitHub repo, after the
human's yes: `sudo tentmaker repo add OWNER/NAME --allow-public --enable
--json`. `--enable` makes the repo require approval for all outside
contributors; first-time-only approval is refused. Done when `tentmaker repo
list --json` shows it and doctor exits 0.

### 8. Add other devices
From a set-up device, over Tailscale SSH, without sudo:
`tentmaker devices add HOST --json` (optional `--memory S --boost-memory S`).
Each host must allow Tailscale SSH for the human and have passwordless `sudo`;
check or ask. Without `--repo` the host joins as a worker. A Windows host gets
back a PowerShell line; hand it to the human. Done when `tentmaker pool
--json` lists every new device.

## Tokens (the human makes them; you never see them)
- GitHub: `gh auth token` from a repo admin, or a fine-grained token with
  Administration and Actions: read and write, Metadata: read (checks mode:
  Commit statuses: read and write; Contents, Pull requests, Metadata: read).
- Forgejo, Gitea, Codeberg: settings > Applications, `write:repository`, `read:user`.
- GitLab: a personal access token with scope `api`.
Store one later: `sudo tentmaker connect forgejo|gitlab --url URL --token-stdin`.

## Everyday changes
- Budget: `sudo tentmaker budget set --memory S --boost-memory S --on-battery everyday|off --json`
- Schedule: `sudo tentmaker schedule add --name N --days mon-fri --from 22:00
  --to 07:00 --profile boost --json`. A `to` before `from` spans midnight. A
  boost window needs AC power and ignores `boost_needs_idle`. Check with
  `tentmaker budget --json`.
- Pause: `sudo tentmaker pause --for 4h REASON` (running jobs finish); `sudo
  tentmaker resume` ends it early.
- No jobs run: `tentmaker doctor --json`, `tentmaker budget --json`, then
  `tentmaker logs -n 200`. Guards like `min_free_disk` have no flag: free space
  (ask before deleting anything), or edit `[guards]` in the config file and
  restart the service (`sudo systemctl restart tentmaker`, `sudo launchctl
  kickstart -k system/dev.tentmaker.daemon`, `Restart-Service Tentmaker`).
- Update: `sudo tentmaker update --json`; undo with `sudo tentmaker rollback`.
- Switch channel: `sudo tentmaker setup --yes --json --channel dev` (saves it,
  keeps all else), then `sudo tentmaker update --json`. `update --channel dev`
  alone installs one dev build and leaves the saved channel.
- Remove, after the human's yes: `sudo tentmaker uninstall --purge --yes
  --json`, then delete the binary (`/usr/local/bin/tentmaker`,
  `~/.local/bin/tentmaker` or `C:\Program Files\Tentmaker\tentmaker.exe`).
  Purge removes everything else; delete nothing else by hand. Workflows
  migrated to the pool wait for a runner until `runs-on` goes back to hosted.

## MCP
`claude mcp add tentmaker -- tentmaker mcp`. Read tools (status, pool, doctor)
work as any user; tools that change the device fail unless the server runs as
root, so use the CLI with sudo for those.
