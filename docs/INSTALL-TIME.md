# Install time

This page is written by hand; `scripts/bench report` rewrites [Benchmarks](BENCHMARKS.md) on every run and does not touch it.

How long a person or an agent waits from the one-line install to a green job on their own
machine, measured on every e2e run (`.github/workflows/e2e.yml`) on GitHub's hosted runners. Each
run writes the table below to its job summary and to the `install-timings-<OS>` artifact
(`go run ./scripts/bench install`).

- **install**: the one-liner itself, from its start until setup returns, which is after the
  service answers and doctor ran. It is download and sha256 check (`download_verify`) plus
  setup's own phases (`setup --json` reports them as `timings`).
- **to green**: install plus `connect` (connect and repo add), `first_job_pickup` (connect done
  until the job's status goes pending) and `first_job_green` (pending until success). Status
  times are to the second. macOS runs no real job in e2e, so it has no to-green number.

The macOS e2e host has too little disk for the macOS guest image, so only the Linux guest image
is pulled there. A Mac with room pulls both.

## Before and after

Seconds on GitHub's hosted runners. "Before" is the first instrumented run (e2e
[45105286087](https://github.com/baney75/tentmaker/actions/runs/45105286087)), on the code
before any speedup; the 20 e2e runs before it, timed by job step, agree (setup medians: Linux
15.5 s, Windows 13.5 s, macOS 78.5 s). "After" is two runs
([45106122483](https://github.com/baney75/tentmaker/actions/runs/45106122483),
[45106935775](https://github.com/baney75/tentmaker/actions/runs/45106935775)).

| OS | install before | install after | to green before | to green after |
| --- | ---: | ---: | ---: | ---: |
| Linux | 15.5 | 14.1, 12.2 | 23.1 | 20.9, 22.4 |
| Windows | 13.5 | 13.4, 13.7 (no change kept) | 21.8 | 22.1, 28.8 |
| macOS | 94.6 | 81.3, 65.1 | (no job) | (no job) |

What changed, phase by phase:

- **Linux, actions/runner download** (`sandbox.runner`): 4.0 s before, 0 after. It is
  network-bound and nothing reads it until the end, so it now runs beside the files, network,
  user manager and podman phases, and `runner.wait` (0 ms in both runs) is what was left of it.
  Every check still runs. Outside `sandbox.preflight`, whose one slow call is the first
  `podman --version` on the runner's cold disk (0.1, 4.9 and 2.2 s in the three runs), the Linux
  sandbox phases went from 14.2 s to 7.8 and 8.7 s.
- **Linux, podman**: split into `podman` (reload, environment, socket restart: 0.24 and 0.29 s)
  and `podman.info` (5.2 and 5.9 s). `podman info` is the probe that rootless podman works, so it
  stays; its first run on a fresh store is most of the time.
- **macOS, guest image pull** (`sandbox.images`): 91.5 s before, 78.5 and 62.8 s after, with
  `tart pull --concurrency 8` instead of Tart's default 4. The pull swung 32 to 144 s before, so
  two runs can't credit the change; the rest of macOS setup is 2 to 3 s.
- **Windows**: tried and reverted, since neither cut moved the total. Overlapping the runner
  download hid its 3.4 s, but `sandbox.net` (the firewall rules) grew from 8.0 s to 10.6 s while
  the zip extracted beside it. Writing and checking the rules in one PowerShell session instead
  of two left `sandbox.net` at 10.7 s, so starting PowerShell is not where its time goes.
- **install-ps1 CI job**: tried `setup-go` without its cache and reverted: setup-go saved 6 s
  and the cold build cost 16 s more. The script itself takes 0.6 to 0.9 s on Windows
  (`download_verify`).

`first_job_pickup` (1.2 to 7.6 s) and `first_job_green` (2 to 6 s, to the second) vary with
GitHub's status API more than with Tentmaker.

## Gate

The e2e workflow fails a run whose install is over its budget: the median before these changes
plus 25%, rounded up to a second. Linux: 15.5 s, budget 20 s. Windows: 14.1 s, budget 18 s.
macOS is gated without the guest image pull (`setup.sandbox.images`), whose time is ghcr.io's
and swung from 32 s to 144 s over 20 runs; the rest took 3.0 s, budget 4 s. The budgets live in
`TM_INSTALL_BUDGET_MS_*` at the top of the workflow.
