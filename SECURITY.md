# Security policy

Tentmaker runs other people's code on your machines, so we treat security reports as the most urgent kind of bug.

## Reporting a vulnerability

Report it privately through GitHub: open the repo's **Security** tab and choose **Report a vulnerability** ([direct link](https://github.com/baney75/tentmaker/security/advisories/new)). Please don't open a public issue, pull request or discussion for it.

Include what you can of:

- the version (`tentmaker version --json`) and OS;
- what an attacker needs to start with (push access to a served repo, a fork, a tailnet device, a local account);
- what they gain;
- steps or a proof of concept.

We will acknowledge the report, keep you updated while we work on a fix, and credit you in the advisory unless you ask us not to. Fixes ship as a stable release and a GitHub security advisory.

## Supported versions

Only the newest stable release and the current dev build get fixes. Tentmaker updates itself between jobs by default, so most devices pick a fix up within a few hours.

## Threat model in brief

The README's [security model](README.md#security-model) is the full version.

**What Tentmaker defends against:**

- **Fork pull requests.** A job from a fork never runs: the broker refuses it, cancels the run if one reaches a runner, and deletes that runner. Checks mode never reads forks.
- **Public repos by accident.** A public repo runs only with `allow_public` set and the forge set to require approval for every outside contributor.
- **One job reaching the next.** Each runner is single use, in a fresh sandbox that is deleted afterwards: an unprivileged account under a systemd unit on Linux, a Tart VM on macOS, an unprivileged account in a Job Object on Windows.
- **Leaked credentials.** Forge tokens stay on the devices you gave them to, in the OS secret store. A runner gets only a single-use grant, never through argv, an environment variable or a log.
- **Strangers on the tailnet.** The peer API serves only your own tailnet user's devices, plus logins you list. Tagged devices are refused unless you allow them.
- **Tampered downloads.** Installers and updates refuse a binary whose sha256 doesn't match the release's `SHA256SUMS`. Runner archives and VM images are pinned by hash or digest.

**What it does not defend against:**

- Code from anyone who can push to a repo you serve, or an outside contributor whose run you approve. That code runs on your device, inside the sandbox.
- Kernel or OS privilege bugs on Linux and Windows, where jobs share the host kernel. Only macOS jobs run in a VM.
- Network access from Linux and Windows jobs to your LAN and tailnet. Only the macOS VMs are fenced off.
- A compromised forge account, forge token, or tailnet account.
