# Benchmarks

How fast Tentmaker runs real CI jobs next to GitHub's hosted runners, measured by
[`bench.yml`](../.github/workflows/bench.yml) every week and on demand. This file and
[`benchmarks.json`](benchmarks.json) are written by `go run ./scripts/bench report`; don't edit
them by hand.

Source run: [bench run](https://github.com/baney75/tentmaker/actions/runs/45063494656), commit `f49c6409ec4f`, 2026-11-22.

## Hardware, honestly

Both sides run on GitHub's hosted Linux VMs. The GitHub-hosted numbers are plain `ubuntu-latest`
jobs. The Tentmaker numbers come from Tentmaker installed on another `ubuntu-latest` VM, used as
the device, with its default job size (2 CPUs and 2 GiB per job, where the hosted job gets the whole
VM). A real home or office machine is likely faster than this. Neither side is tuned.

| Workload | Device image | CPUs | CPU | Memory | Tentmaker | Budget |
|---|---|---|---|---|---|---|
| [cache](https://github.com/baney75/tentmaker/actions/runs/45063494656/job/137335944816) | ubuntu26 20261116.88.1 | 4 | Intel(R) Xeon(R) 6975P-C | 15 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [cross-build](https://github.com/baney75/tentmaker/actions/runs/45063494656/job/137335945037) | ubuntu26 20261116.202.1 | 4 | INTEL(R) XEON(R) PLATINUM 8573C | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [docker](https://github.com/baney75/tentmaker/actions/runs/45063494656/job/137335945033) | ubuntu26 20261116.202.1 | 4 | AMD EPYC 7763 64-Core Processor | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [go-test](https://github.com/baney75/tentmaker/actions/runs/45063494656/job/137335944959) | ubuntu26 20261116.202.1 | 4 | AMD EPYC 7763 64-Core Processor | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |
| [node](https://github.com/baney75/tentmaker/actions/runs/45063494656/job/137335945043) | ubuntu26 20261116.202.1 | 4 | INTEL(R) XEON(R) PLATINUM 8573C | 16 GiB | 0.0.1-scn.1 | `{"admit":true,"evict":false,"limit":{"cpus":4,"memory_bytes":8589934592},"profile":"boost","reasons":null,"slots":2}` |

## Job time

Median seconds from the job starting to its result. Cold: an empty cache. Warm: the same
workload again with the cache the cold run left (`actions/cache` on GitHub, Tentmaker's repo
cache on Tentmaker). Speedup is GitHub-hosted warm over Tentmaker warm. n counts passing runs,
GitHub-hosted / Tentmaker, warm.

| Workload | GitHub-hosted cold | GitHub-hosted warm | Tentmaker cold | Tentmaker warm | Speedup | n |
|---|---|---|---|---|---|---|
| Go test (this repo: gofmt, vet, go test -race) | 80.0 s | 28.0 s | 135.0 s | 22.0 s | 1.3x | 3 / 3 |
| Go cross build (this repo, 6 targets) | 90.0 s | 19.0 s | 143.0 s | 6.0 s | 3.2x | 3 / 3 |
| Node: tj/commander.js v15.1.0, npm ci and npm test | 16.0 s | 18.0 s | 19.0 s | 18.0 s | 1.0x | 3 / 3 |
| Docker build, two stages (baseline: no layer cache yet) | 28.0 s | 27.5 s | 39.0 s | 41.0 s | 0.7x | 2 / 3 |
| Cache: 256 MiB blob save, then restore | 8.0 s | 8.0 s | 3.0 s | 2.0 s | 4.0x | 3 / 3 |

Every run, with links to each job, is in [`benchmarks.json`](benchmarks.json) (`samples`).

## Job start latency

Seconds from queued to running. GitHub-hosted: the job's `created_at` to `started_at`.
Tentmaker: the push to the `pending` status it posts when a device takes the job, so it also
counts noticing the commit.

| Platform | Median | Min | Max | Jobs |
|---|---|---|---|---|
| GitHub-hosted | 2.0 s | 1.0 s | 2.0 s | 30 |
| Tentmaker | 4.0 s | 2.0 s | 2235.0 s | 30 |

## Where the time goes

Median seconds per phase, from marks the job itself writes (`scripts/bench/phase.sh`). setup is
the job start to the first mark: checkout and tool setup, plus cache restore on GitHub, where
`actions/cache` restores before the first mark.

| Workload | Platform | Round | Phases |
|---|---|---|---|
| go-test | GitHub-hosted | cold | setup 12.2 s, deps 0.3 s, test 64.0 s |
| go-test | GitHub-hosted | warm | setup 11.1 s, deps 0.0 s, test 15.2 s |
| go-test | Tentmaker | cold | setup 1.2 s, deps 0.3 s, test 131.9 s |
| go-test | Tentmaker | warm | setup 1.7 s, deps 0.0 s, test 20.0 s |
| cross-build | GitHub-hosted | cold | setup 11.9 s, build 71.9 s, deps 0.3 s |
| cross-build | GitHub-hosted | warm | setup 12.9 s, build 3.2 s, deps 0.0 s |
| cross-build | Tentmaker | cold | setup 2.0 s, build 134.7 s, deps 0.4 s |
| cross-build | Tentmaker | warm | setup 1.6 s, build 3.4 s, deps 0.0 s |
| node | GitHub-hosted | cold | setup 3.7 s, deps 2.3 s, fetch 0.1 s, test 8.1 s |
| node | GitHub-hosted | warm | setup 5.1 s, deps 2.4 s, fetch 0.3 s, test 7.9 s |
| node | Tentmaker | cold | setup 1.6 s, deps 2.6 s, fetch 0.1 s, test 13.9 s |
| node | Tentmaker | warm | setup 1.7 s, deps 1.7 s, fetch 0.1 s, test 14.0 s |
| docker | GitHub-hosted | cold | setup 2.9 s, build 23.4 s |
| docker | GitHub-hosted | warm | setup 3.3 s, build 22.4 s |
| docker | Tentmaker | cold | setup 1.7 s, build 35.4 s |
| docker | Tentmaker | warm | setup 1.2 s, build 36.1 s |
| cache | GitHub-hosted | cold | setup 3.4 s, restore 0.2 s, save 1.9 s |
| cache | GitHub-hosted | warm | setup 2.9 s, restore 2.6 s, save 0.0 s |
| cache | Tentmaker | cold | setup 1.7 s, restore 0.0 s, save 0.2 s |
| cache | Tentmaker | warm | setup 1.3 s, restore 0.2 s, save 0.0 s |

## Cache throughput

The cache workload moves a 256 MiB random blob (MB here is 10^6 bytes). GitHub-hosted:
`actions/cache/save` in the cold run and `actions/cache/restore` in the warm run, timed from the
step before to the step after. Tentmaker: a copy into its repo cache on the same disk, synced
(save), and a copy back out (restore), which the OS may serve from memory.

| Platform | Save (cold run) | Restore (warm run) |
|---|---|---|
| GitHub-hosted | 138 MB/s | 104 MB/s |
| Tentmaker | 1304 MB/s | 1700 MB/s |

## Gate

The bench run fails when, on any workload but the Docker baseline, Tentmaker warm is slower
than GitHub-hosted warm (job time, and cache restore throughput), when either side has fewer
than 3 passing warm runs, or when the median Tentmaker job start latency is over 5 s.

This run: **pass**.

## Known limits of this bench

- CPUs: a Tentmaker job is capped at 2 CPUs (the default `job.cpus`); the hosted job has the
  VM's 4. That is most of why Tentmaker's cold runs are slower: they compile from nothing on
  half the CPUs.
- Toolchains: GitHub-hosted jobs download the exact Go in `go.mod` (inside their setup time).
  Under Tentmaker, checks mode uses the first Go on the device that satisfies `go.mod`; on these
  VMs that is the image's own, a newer minor version. Node is 22 on both.
- Pickup outliers: the five bench devices share one workflow `GITHUB_TOKEN`. When its hourly
  budget runs low, Tentmaker stops polling until the reset, as designed, so a few runs wait
  minutes (the Max column above). A device with its own token doesn't share that budget. The
  gate uses the median.
- Pulls: base images come from the ECR Public mirror, which can answer 429 to a busy shared IP;
  a run that fails that way counts as failed, not slow.

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
