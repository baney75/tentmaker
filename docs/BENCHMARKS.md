# Benchmarks

How fast Tentmaker runs real CI jobs next to GitHub's hosted runners, measured by
[`bench.yml`](../.github/workflows/bench.yml) every week and on demand. This file and
[`benchmarks.json`](benchmarks.json) are written by `go run ./scripts/bench report`; don't edit
them by hand.

Source run: [bench run](https://github.com/baney75/tentmaker/actions/runs/45114305731), commit `a8568b7a8ebf`, 2026-11-23.

## Hardware, honestly

Both sides run on GitHub's hosted Linux VMs. The GitHub-hosted numbers are plain `ubuntu-latest`
jobs. The Tentmaker numbers come from Tentmaker installed on another `ubuntu-latest` VM, used as
the device, with its default job size (2 CPUs and 2 GiB per job, where the hosted job gets the whole
VM). A real home or office machine is likely faster than this. Neither side is tuned.

| Workload | Device image | CPUs | CPU | Memory | Tentmaker | Budget |
|---|---|---|---|---|---|---|
| [cache](https://github.com/baney75/tentmaker/actions/runs/45114305731/job/137493494939) | ubuntu26 20261116.202.1 | 4 | AMD EPYC 7763 64-Core Processor | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [cross-build](https://github.com/baney75/tentmaker/actions/runs/45114305731/job/137493494986) | ubuntu26 20261116.202.1 | 4 | INTEL(R) XEON(R) PLATINUM 8573C | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [docker](https://github.com/baney75/tentmaker/actions/runs/45114305731/job/137493495055) | ubuntu26 20261116.88.1 | 4 | Intel(R) Xeon(R) 6975P-C | 15 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [go-test](https://github.com/baney75/tentmaker/actions/runs/45114305731/job/137493494987) | ubuntu26 20261116.202.1 | 4 | AMD EPYC 7763 64-Core Processor | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [node](https://github.com/baney75/tentmaker/actions/runs/45114305731/job/137493494815) | ubuntu26 20261116.202.1 | 4 | AMD EPYC 9V74 80-Core Processor | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |

## Job time

Median seconds from the job starting to its result. Cold: an empty cache. Warm: the same
workload again with the cache the cold run left (`actions/cache` on GitHub, Tentmaker's repo
cache on Tentmaker). Speedup is GitHub-hosted warm over Tentmaker warm. n counts passing warm
runs, GitHub-hosted / Tentmaker; for every workload but Docker, only warm runs that hit the cache
count, and a warm run that missed it is left out of every number.

| Workload | GitHub-hosted cold | GitHub-hosted warm | Tentmaker cold | Tentmaker warm | Speedup | n |
|---|---|---|---|---|---|---|
| Go test (this repo: gofmt, vet, go test -race) | 119.0 s | 30.0 s | 159.0 s | 33.0 s | 0.9x | 3 / 3 |
| Go cross build (this repo, 6 targets) | 98.0 s | 21.0 s | 149.0 s | 8.0 s | 2.6x | 3 / 3 |
| Node: tj/commander.js v15.1.0, npm ci and npm test | 21.0 s | 19.0 s | 19.0 s | 17.0 s | 1.1x | 3 / 3 |
| Docker build, two stages (baseline: no layer cache yet) | 29.0 s | 29.0 s | 28.0 s | 4.0 s | 7.3x | 2 / 3 |
| Cache: 256 MiB blob save, then restore | 9.0 s | 9.0 s | 8.0 s | 3.0 s | 3.0x | 3 / 3 |

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
| GitHub-hosted | 2.0 s | 2.0 s | 1.0 s | 2.0 s | 30 |
| Tentmaker | 3.0 s | 29.0 s | 0.0 s | 29.0 s | 30 |

## API calls

What each Tentmaker device's daemon asked of the GitHub API since its last restart (the bench
restarts it between reps), from `tentmaker status --json` (`broker.api`). All five devices share the workflow's
`GITHUB_TOKEN`, which gets 1000 billed requests an hour per repo. Billed is every request but
a 304: a conditional GET that found nothing new costs no rate limit.

| Workload | Hours | Requests/h | Billed/h | 304s | Rate limited |
|---|---|---|---|---|---|
| cache | 0.01 | 8131 | 1831 | 77% | 0 |
| cross-build | 0.05 | 3449 | 507 | 85% | 0 |
| docker | 0.01 | 4296 | 1884 | 56% | 0 |
| go-test | 0.06 | 3425 | 405 | 88% | 0 |
| node | 0.02 | 3983 | 1310 | 67% | 0 |

## Where the time goes

Median seconds per phase, from marks the job itself writes (`scripts/bench/phase.sh`). setup is
the job start to the first mark: checkout and tool setup, plus cache restore on GitHub, where
`actions/cache` restores before the first mark.

| Workload | Platform | Round | Phases |
|---|---|---|---|
| go-test | GitHub-hosted | cold | setup 10.2 s, deps 0.3 s, test 104.5 s |
| go-test | GitHub-hosted | warm | setup 12.7 s, deps 0.0 s, test 16.1 s |
| go-test | Tentmaker | cold | setup 4.6 s, deps 0.2 s, test 153.1 s |
| go-test | Tentmaker | warm | setup 4.2 s, deps 0.0 s, test 27.7 s |
| cross-build | GitHub-hosted | cold | setup 10.7 s, build 79.4 s, deps 0.2 s |
| cross-build | GitHub-hosted | warm | setup 14.0 s, build 2.6 s, deps 0.0 s |
| cross-build | Tentmaker | cold | setup 4.5 s, build 142.9 s, deps 0.2 s |
| cross-build | Tentmaker | warm | setup 4.1 s, build 3.6 s, deps 0.0 s |
| node | GitHub-hosted | cold | setup 4.3 s, deps 3.8 s, fetch 0.4 s, test 9.4 s |
| node | GitHub-hosted | warm | setup 4.7 s, deps 2.0 s, fetch 0.1 s, test 8.9 s |
| node | Tentmaker | cold | setup 4.3 s, deps 2.2 s, fetch 0.1 s, test 11.6 s |
| node | Tentmaker | warm | setup 1.9 s, deps 1.5 s, fetch 0.1 s, test 11.6 s |
| docker | GitHub-hosted | cold | setup 4.2 s, build 25.0 s |
| docker | GitHub-hosted | warm | setup 2.6 s, build 24.7 s |
| docker | Tentmaker | cold | setup 3.3 s, build 24.4 s |
| docker | Tentmaker | warm | setup 2.0 s, build 1.3 s |
| cache | GitHub-hosted | cold | setup 3.4 s, restore 0.4 s, save 2.4 s |
| cache | GitHub-hosted | warm | setup 3.2 s, restore 3.1 s, save 0.0 s |
| cache | Tentmaker | cold | setup 4.7 s, restore 0.0 s, save 1.2 s |
| cache | Tentmaker | warm | setup 1.7 s, restore 0.2 s, save 0.0 s |

## Cache throughput

The cache workload moves a 256 MiB random blob (MB here is 10^6 bytes). GitHub-hosted:
`actions/cache/save` in the cold run and `actions/cache/restore` in the warm run, timed from the
step before to the step after. Tentmaker: a copy into its repo cache on the same disk, synced
(save), and a copy back out (restore), which the OS may serve from memory.

| Platform | Save (cold run) | Restore (warm run) |
|---|---|---|
| GitHub-hosted | 112 MB/s | 87 MB/s |
| Tentmaker | 223 MB/s | 1197 MB/s |

## Gate

The bench run fails when, on any workload but the Docker baseline, Tentmaker warm is slower
than GitHub-hosted warm by more than 5% plus 1 s (job times have whole-second
resolution), when Tentmaker's median cache restore throughput is lower, when either side has
fewer than 3 passing warm runs that hit the cache or fewer than 3 restore throughput
measurements, when a warm run didn't record whether it hit the cache, when the two sides didn't
record the same Go, or when the median Tentmaker job start latency is over 5 s.

This run: **fail**.

- go-test: Tentmaker warm (33.0 s) is slower than GitHub-hosted warm (30.0 s) by more than the 5% + 1 s allowance

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
