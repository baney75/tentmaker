# Benchmarks

How fast Tentmaker runs real CI jobs next to GitHub's hosted runners, measured by
[`bench.yml`](../.github/workflows/bench.yml) every week and on demand. This file and
[`benchmarks.json`](benchmarks.json) are written by `go run ./scripts/bench report`; don't edit
them by hand.

Source run: [bench run](https://github.com/baney75/tentmaker/actions/runs/45105047156), commit `fb257a61ccce`, 2026-11-23.

## Hardware, honestly

Both sides run on GitHub's hosted Linux VMs. The GitHub-hosted numbers are plain `ubuntu-latest`
jobs. The Tentmaker numbers come from Tentmaker installed on another `ubuntu-latest` VM, used as
the device, with its default job size (2 CPUs and 2 GiB per job, where the hosted job gets the whole
VM). A real home or office machine is likely faster than this. Neither side is tuned.

| Workload | Device image | CPUs | CPU | Memory | Tentmaker | Budget |
|---|---|---|---|---|---|---|
| [cache](https://github.com/baney75/tentmaker/actions/runs/45105047156/job/137464292587) | ubuntu26 20261116.202.1 | 4 | AMD EPYC 7763 64-Core Processor | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [cross-build](https://github.com/baney75/tentmaker/actions/runs/45105047156/job/137464292522) | ubuntu26 20261116.202.1 | 4 | AMD EPYC 9V74 80-Core Processor | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [docker](https://github.com/baney75/tentmaker/actions/runs/45105047156/job/137464292649) | ubuntu26 20261116.202.1 | 4 | AMD EPYC 7763 64-Core Processor | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [go-test](https://github.com/baney75/tentmaker/actions/runs/45105047156/job/137464292365) | ubuntu26 20261116.202.1 | 4 | AMD EPYC 9V45 96-Core Processor | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [node](https://github.com/baney75/tentmaker/actions/runs/45105047156/job/137464292598) | ubuntu26 20261116.202.1 | 4 | AMD EPYC 7763 64-Core Processor | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |

## Job time

Median seconds from the job starting to its result. Cold: an empty cache. Warm: the same
workload again with the cache the cold run left (`actions/cache` on GitHub, Tentmaker's repo
cache on Tentmaker). Speedup is GitHub-hosted warm over Tentmaker warm. n counts passing warm
runs, GitHub-hosted / Tentmaker; for every workload but Docker, only warm runs that hit the cache
count, and a warm run that missed it is left out of every number.

| Workload | GitHub-hosted cold | GitHub-hosted warm | Tentmaker cold | Tentmaker warm | Speedup | n |
|---|---|---|---|---|---|---|
| Go test (this repo: gofmt, vet, go test -race) | 95.0 s | 33.0 s | 113.0 s | 22.0 s | 1.5x | 3 / 3 |
| Go cross build (this repo, 6 targets) | 98.0 s | 20.0 s | 120.0 s | 6.0 s | 3.3x | 3 / 3 |
| Node: tj/commander.js v15.1.0, npm ci and npm test | 16.0 s | 18.0 s | 20.0 s | 19.0 s | 0.9x | 3 / 2 |
| Docker build, two stages (baseline: no layer cache yet) | 30.5 s | 27.0 s | 40.0 s | 42.0 s | 0.6x | 3 / 3 |
| Cache: 256 MiB blob save, then restore | 11.0 s | 12.0 s | 5.0 s | 3.0 s | 4.0x | 3 / 3 |

Go used by each side (`go env GOVERSION`, every passing run):

- go-test: GitHub-hosted go1.25.0; Tentmaker go1.25.0
- cross-build: GitHub-hosted go1.25.0; Tentmaker go1.25.0

Every run, with links to each job, is in [`benchmarks.json`](benchmarks.json) (`samples`).

## Job start latency

Seconds from queued to running. GitHub-hosted: the job's `created_at` to `started_at`.
Tentmaker: the push to the `pending` status it posts when a device takes the job, so it also
counts noticing the commit.

| Platform | Median | p95 | Min | Max | Jobs |
|---|---|---|---|---|---|
| GitHub-hosted | 3.0 s | 48.0 s | 1.0 s | 66.0 s | 30 |
| Tentmaker | 1.0 s | 2.0 s | 0.0 s | 2.0 s | 30 |

## API calls

What each Tentmaker device's daemon asked of the GitHub API since its last restart (the bench
restarts it between reps), from `tentmaker status --json` (`broker.api`). All five devices share the workflow's
`GITHUB_TOKEN`, which gets 1000 billed requests an hour per repo. Billed is every request but
a 304: a conditional GET that found nothing new costs no rate limit.

| Workload | Hours | Requests/h | Billed/h | 304s | Rate limited |
|---|---|---|---|---|---|
| cache | 0.01 | 22347 | 6285 | 72% | 0 |
| cross-build | 0.04 | 19100 | 1110 | 94% | 0 |
| docker | 0.03 | 19543 | 1850 | 91% | 0 |
| go-test | 0.04 | 19248 | 1009 | 95% | 0 |
| node | 0.01 | 14923 | 2926 | 80% | 0 |

## Where the time goes

Median seconds per phase, from marks the job itself writes (`scripts/bench/phase.sh`). setup is
the job start to the first mark: checkout and tool setup, plus cache restore on GitHub, where
`actions/cache` restores before the first mark.

| Workload | Platform | Round | Phases |
|---|---|---|---|
| go-test | GitHub-hosted | cold | setup 13.0 s, deps 0.2 s, test 75.0 s |
| go-test | GitHub-hosted | warm | setup 12.3 s, deps 0.0 s, test 16.9 s |
| go-test | Tentmaker | cold | setup 1.8 s, deps 0.3 s, test 110.5 s |
| go-test | Tentmaker | warm | setup 1.8 s, deps 0.0 s, test 20.1 s |
| cross-build | GitHub-hosted | cold | setup 11.8 s, build 78.6 s, deps 0.3 s |
| cross-build | GitHub-hosted | warm | setup 13.8 s, build 3.7 s, deps 0.0 s |
| cross-build | Tentmaker | cold | setup 1.3 s, build 117.6 s, deps 0.2 s |
| cross-build | Tentmaker | warm | setup 2.2 s, build 3.2 s, deps 0.0 s |
| node | GitHub-hosted | cold | setup 3.7 s, deps 1.9 s, fetch 0.3 s, test 7.1 s |
| node | GitHub-hosted | warm | setup 4.5 s, deps 1.9 s, fetch 0.2 s, test 6.4 s |
| node | Tentmaker | cold | setup 1.6 s, deps 2.7 s, fetch 0.1 s, test 14.7 s |
| node | Tentmaker | warm | setup 1.6 s, deps 2.0 s, fetch 0.1 s, test 14.5 s |
| docker | GitHub-hosted | cold | setup 4.5 s, build 24.6 s |
| docker | GitHub-hosted | warm | setup 3.0 s, build 22.3 s |
| docker | Tentmaker | cold | setup 1.5 s, build 37.0 s |
| docker | Tentmaker | warm | setup 1.5 s, build 36.8 s |
| cache | GitHub-hosted | cold | setup 6.2 s, restore 0.3 s, save 2.9 s |
| cache | GitHub-hosted | warm | setup 7.4 s, restore 2.0 s, save 0.0 s |
| cache | Tentmaker | cold | setup 2.1 s, restore 0.0 s, save 1.3 s |
| cache | Tentmaker | warm | setup 1.7 s, restore 0.2 s, save 0.0 s |

## Cache throughput

The cache workload moves a 256 MiB random blob (MB here is 10^6 bytes). GitHub-hosted:
`actions/cache/save` in the cold run and `actions/cache/restore` in the warm run, timed from the
step before to the step after. Tentmaker: a copy into its repo cache on the same disk, synced
(save), and a copy back out (restore), which the OS may serve from memory.

| Platform | Save (cold run) | Restore (warm run) |
|---|---|---|
| GitHub-hosted | 91 MB/s | 131 MB/s |
| Tentmaker | 211 MB/s | 1240 MB/s |

## Gate

The bench run fails when, on any workload but the Docker baseline, Tentmaker warm is slower
than GitHub-hosted warm by more than 5% plus 1 s (job times have whole-second
resolution), when Tentmaker's median cache restore throughput is lower, when either side has
fewer than 3 passing warm runs that hit the cache or fewer than 3 restore throughput
measurements, when a warm run didn't record whether it hit the cache, when the two sides didn't
record the same Go, or when the median Tentmaker job start latency is over 5 s.

This run: **fail**.

- node: 2 passing Tentmaker warm runs that hit the cache, of 3 (1 failed, 0 missed the cache); the gate needs 3

## Known limits of this bench

- CPUs: a Tentmaker job is capped at 2 CPUs (the default `job.cpus`); the hosted job has the
  VM's 4. That is most of why Tentmaker's cold runs are slower: they compile from nothing on
  half the CPUs.
- Toolchains: the Go workloads pin the exact Go in `go.mod`. GitHub-hosted jobs download it
  inside their setup time; the Tentmaker device has it installed in its tools folder, as a
  device you use every day would. Each run records its Go, and the gate fails when the sides
  differ. Node is 22 on both.
- Pickup outliers: the five bench devices share one workflow `GITHUB_TOKEN`. When its hourly
  budget runs low, Tentmaker stops polling until the reset, as designed, so a few runs wait
  minutes (the Max column above). A device with its own token doesn't share that budget. The
  gate uses the median.
- Pulls: base images come from the ECR Public mirror, which can answer 429 to a busy shared IP;
  a run that fails that way counts as failed, not slow. The gate leaves Docker out, so a failed
  Docker run doesn't fail the bench run.

## Blacksmith's published claims

We can't run Blacksmith, so these are **their claims, not our measurements**, quoted from their
docs as of 2026-11-22. Compare them with the tables above knowing they come from different
hardware and workloads.

- Runner boot: "ephemeral Firecracker microVMs that boot in under 3 seconds"
  ([overview](https://docs.blacksmith.sh/blacksmith-runners/overview.md)).
- Speed: "Blacksmith runners are 2x faster and costs 67% less than GitHub-hosted runners"
  ([overview](https://docs.blacksmith.sh/blacksmith-runners/overview.md)).
- Cache: a "co-located cache which quadruples cache throughput to 400MB/s", and a table of
  90 MB/s for the GitHub Actions cache against 400 MB/s for theirs
  ([overview](https://docs.blacksmith.sh/blacksmith-runners/overview.md),
  [dependencies and sticky disks](https://docs.blacksmith.sh/blacksmith-caching/dependencies-sticky-disks.md)).
- Docker: "customers have reported 2x to 40x improvements in build times" from their layer cache
  ([Docker builds](https://docs.blacksmith.sh/blacksmith-caching/docker-builds.md)).

Tentmaker has no Docker layer cache yet; the Docker row above is the baseline for that work.

## Reproduce

```sh
gh workflow run bench.yml --ref main
```

The run's `bench-report` artifact holds this file and `benchmarks.json`. To rebuild them from the
raw data (`bench-raw` artifact): `go run ./scripts/bench report -raw <dir> -md docs/BENCHMARKS.md
-json docs/benchmarks.json`.

## Install

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

### Before and after

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

### Gate

The e2e workflow fails a run whose install is over its budget: the median before these changes
plus 25%, rounded up to a second. Linux: 15.5 s, budget 20 s. Windows: 14.1 s, budget 18 s.
macOS is gated without the guest image pull (`setup.sandbox.images`), whose time is ghcr.io's
and swung from 32 s to 144 s over 20 runs; the rest took 3.0 s, budget 4 s. The budgets live in
`TM_INSTALL_BUDGET_MS_*` at the top of the workflow.
