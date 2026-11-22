package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The gate's limits.
const (
	MaxStartS = 5.0 // median Tentmaker job start latency, seconds
	MinRuns   = 3   // passing runs each gated cell needs
	// Tentmaker warm fails only when it is slower than GitHub-hosted warm
	// by more than SlackFrac of the hosted median plus SlackS. Both sides'
	// job times come from timestamps with whole-second resolution, so a
	// 1 s difference is noise, not a regression.
	SlackFrac = 0.05
	SlackS    = 1.0
)

type workload struct {
	Name, Title string
	Gated       bool
	Cached      bool     // the warm run restores what the cold run saved; it records cache_hit
	Go          bool     // compiles Go; both sides must record the same Go
	Rates       []string // phases whose warm throughput the gate compares
}

// workloadInfo is the fixed list of workloads, in table order.
var workloadInfo = []workload{
	{"go-test", "Go test (this repo: gofmt, vet, go test -race)", true, true, true, nil},
	{"cross-build", "Go cross build (this repo, 6 targets)", true, true, true, nil},
	{"node", "Node: tj/commander.js v15.1.0, npm ci and npm test", true, true, false, nil},
	{"docker", "Docker build, two stages (baseline: no layer cache yet)", false, false, false, nil},
	{"cache", "Cache: 256 MiB blob save, then restore", true, true, false, []string{"restore"}},
}

func infoOf(name string) workload {
	for _, wi := range workloadInfo {
		if wi.Name == name {
			return wi
		}
	}
	return workload{Name: name}
}

// Cell is the spread of one number over runs.
type Cell struct {
	N      int     `json:"n"`
	Median float64 `json:"median"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

func cellOf(v []float64) Cell {
	if len(v) == 0 {
		return Cell{}
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	med := s[n/2]
	if n%2 == 0 {
		med = (s[n/2-1] + s[n/2]) / 2
	}
	return Cell{N: n, Median: round1(med), Min: s[0], Max: s[n-1]}
}

// Side is one workload on one platform in one round.
type Side struct {
	Runs   int `json:"runs"`   // samples recorded
	Failed int `json:"failed"` // samples that did not pass, or had no job time
	// Warm runs of a cached workload only: passing runs that hit the cache,
	// that missed it (left out of every number below), and that didn't
	// record which (kept, but the gate fails on them).
	Hits       int             `json:"cache_hits,omitempty"`
	Missed     int             `json:"cache_missed,omitempty"`
	Unrecorded int             `json:"cache_unrecorded,omitempty"`
	Go         []string        `json:"go,omitempty"`    // the Go versions passing runs recorded
	NoGo       int             `json:"no_go,omitempty"` // passing runs of a Go workload that recorded none
	Job        Cell            `json:"job_s"`           // passing runs only
	Start      Cell            `json:"start_s"`
	Phases     map[string]Cell `json:"phases_s,omitempty"`
	MBps       map[string]Cell `json:"mbps,omitempty"`
}

// WorkloadResult is one row of the tables.
type WorkloadResult struct {
	Name        string           `json:"name"`
	Title       string           `json:"title"`
	Gated       bool             `json:"gated"`
	GitHub      map[string]*Side `json:"github"`    // "cold", "warm"
	Tentmaker   map[string]*Side `json:"tentmaker"` // "cold", "warm"
	SpeedupWarm float64          `json:"speedup_warm,omitempty"`
}

// GateResult says whether the bench run passes.
type GateResult struct {
	Pass     bool     `json:"pass"`
	Failures []string `json:"failures"`
}

// Report is what docs/benchmarks.json holds.
type Report struct {
	RunURL       string           `json:"run_url"`
	SHA          string           `json:"sha"`
	Date         string           `json:"date"`
	Devices      []Device         `json:"devices"`
	Missing      []string         `json:"missing,omitempty"`
	StartLatency map[string]Cell  `json:"start_latency_s"` // per platform, every run
	Workloads    []WorkloadResult `json:"workloads"`
	Gate         GateResult       `json:"gate"`
	Samples      []Sample         `json:"samples"`
}

// Summarize builds the report and runs the gate on it.
func Summarize(r Run) Report {
	rep := Report{RunURL: r.URL, SHA: r.SHA, Date: r.Date, Devices: r.Devices, Missing: r.Missing, Samples: r.Samples}
	starts := map[string][]float64{}
	for _, s := range r.Samples {
		if s.StartS >= 0 {
			starts[s.Platform] = append(starts[s.Platform], s.StartS)
		}
	}
	rep.StartLatency = map[string]Cell{}
	for p, v := range starts {
		rep.StartLatency[p] = cellOf(v)
	}
	for _, wi := range workloadInfo {
		w := WorkloadResult{Name: wi.Name, Title: wi.Title, Gated: wi.Gated,
			GitHub: side(r.Samples, wi, "github"), Tentmaker: side(r.Samples, wi, "tentmaker")}
		if g, t := w.GitHub["warm"].Job, w.Tentmaker["warm"].Job; g.N > 0 && t.N > 0 && t.Median > 0 {
			w.SpeedupWarm = round1(g.Median / t.Median)
		}
		rep.Workloads = append(rep.Workloads, w)
	}
	rep.Gate = Gate(rep)
	return rep
}

func side(samples []Sample, wi workload, platform string) map[string]*Side {
	out := map[string]*Side{}
	for _, round := range []string{"cold", "warm"} {
		var job, start []float64
		phases := map[string][]float64{}
		mbps := map[string][]float64{}
		gos := map[string]bool{}
		sd := &Side{}
		for _, s := range samples {
			if s.Workload != wi.Name || s.Platform != platform || s.Round != round {
				continue
			}
			sd.Runs++
			if s.StartS >= 0 {
				start = append(start, s.StartS)
			}
			// A passing run with no job time can't be measured; counting its
			// zero would make it look fast.
			if !s.OK || s.JobS <= 0 {
				sd.Failed++
				continue
			}
			if wi.Go {
				if s.Go == "" {
					sd.NoGo++
				} else {
					gos[s.Go] = true
				}
			}
			if round == "warm" && wi.Cached {
				switch {
				case s.CacheHit == nil:
					sd.Unrecorded++
				case !*s.CacheHit:
					sd.Missed++
					continue
				default:
					sd.Hits++
				}
			}
			job = append(job, s.JobS)
			for k, v := range s.Phases {
				phases[k] = append(phases[k], v)
			}
			for k, v := range s.MBps {
				mbps[k] = append(mbps[k], v)
			}
		}
		sd.Job, sd.Start = cellOf(job), cellOf(start)
		sd.Go = sortedKeys(gos)
		if len(phases) > 0 {
			sd.Phases = map[string]Cell{}
			for k, v := range phases {
				sd.Phases[k] = cellOf(v)
			}
		}
		if len(mbps) > 0 {
			sd.MBps = map[string]Cell{}
			for k, v := range mbps {
				sd.MBps[k] = cellOf(v)
			}
		}
		out[round] = sd
	}
	return out
}

// Gate fails the run when, on a gated workload:
//
//   - either side has fewer than MinRuns passing warm runs (for a cached
//     workload: passing warm runs that hit the cache; a warm run that
//     missed it is left out, and one that didn't record it fails the gate);
//   - a Go workload's runs didn't all record the same Go, on both sides;
//   - Tentmaker warm is slower than GitHub-hosted warm by more than the
//     SlackFrac + SlackS allowance;
//   - a throughput the workload names (Rates) has fewer than MinRuns
//     measurements on either side, or Tentmaker's median is lower.
//
// It also fails when the median Tentmaker job start latency is over
// MaxStartS. A number it can't read fails the gate.
func Gate(rep Report) GateResult {
	var f []string
	for _, w := range rep.Workloads {
		if !w.Gated {
			continue
		}
		wi := infoOf(w.Name)
		gw, tw := w.GitHub["warm"], w.Tentmaker["warm"]
		if gw == nil || tw == nil {
			f = append(f, w.Name+": no warm runs")
			continue
		}
		if wi.Go {
			f = append(f, goFailures(w)...)
		}
		short := false
		for _, pl := range []struct {
			name string
			sd   *Side
		}{{"Tentmaker", tw}, {"GitHub-hosted", gw}} {
			sd := pl.sd
			if !wi.Cached {
				if sd.Job.N < MinRuns {
					f = append(f, fmt.Sprintf("%s: %d passing %s warm runs of %d; the gate needs %d", w.Name, sd.Job.N, pl.name, sd.Runs, MinRuns))
					short = true
				}
				continue
			}
			if sd.Unrecorded > 0 {
				f = append(f, fmt.Sprintf("%s: %d passing %s warm runs didn't record whether they hit the cache", w.Name, sd.Unrecorded, pl.name))
			}
			if sd.Hits < MinRuns {
				short = true
				if sd.Hits+sd.Unrecorded >= MinRuns {
					continue // the line above says why
				}
				f = append(f, fmt.Sprintf("%s: %d passing %s warm runs that hit the cache, of %d (%d failed, %d missed the cache); the gate needs %d",
					w.Name, sd.Hits, pl.name, sd.Runs, sd.Failed, sd.Missed, MinRuns))
			}
		}
		for _, p := range wi.Rates {
			g, t := gw.MBps[p], tw.MBps[p]
			ok := true
			if t.N < MinRuns {
				f = append(f, fmt.Sprintf("%s: %d Tentmaker warm %s throughput measurements; the gate needs %d", w.Name, t.N, p, MinRuns))
				ok = false
			}
			if g.N < MinRuns {
				f = append(f, fmt.Sprintf("%s: %d GitHub-hosted warm %s throughput measurements; the gate needs %d", w.Name, g.N, p, MinRuns))
				ok = false
			}
			if ok && t.Median < g.Median {
				f = append(f, fmt.Sprintf("%s: Tentmaker warm %s (%.1f MB/s) is slower than GitHub-hosted warm (%.1f MB/s)", w.Name, p, t.Median, g.Median))
			}
		}
		if short {
			continue
		}
		if limit := gw.Job.Median*(1+SlackFrac) + SlackS; tw.Job.Median > limit {
			f = append(f, fmt.Sprintf("%s: Tentmaker warm (%.1f s) is slower than GitHub-hosted warm (%.1f s) by more than the %.0f%% + %.0f s allowance",
				w.Name, tw.Job.Median, gw.Job.Median, SlackFrac*100, SlackS))
		}
	}
	lat, ok := rep.StartLatency["tentmaker"]
	switch {
	case !ok || lat.N == 0:
		f = append(f, "no Tentmaker job start latency was measured")
	case lat.Median > MaxStartS:
		f = append(f, fmt.Sprintf("median Tentmaker job start latency is %.1f s over %d jobs; the limit is %.0f s", lat.Median, lat.N, MaxStartS))
	}
	return GateResult{Pass: len(f) == 0, Failures: f}
}

// goFailures checks that every passing run of a Go workload, cold and warm
// on both sides, recorded its Go, and that they all recorded the same one.
func goFailures(w WorkloadResult) []string {
	var f []string
	all := map[string]bool{}
	byPlatform := map[string][]string{}
	for _, pl := range []string{"github", "tentmaker"} {
		m := w.GitHub
		if pl == "tentmaker" {
			m = w.Tentmaker
		}
		seen := map[string]bool{}
		for _, round := range []string{"cold", "warm"} {
			sd := m[round]
			if sd == nil {
				continue
			}
			if sd.NoGo > 0 {
				f = append(f, fmt.Sprintf("%s: %d passing %s %s runs recorded no Go version", w.Name, sd.NoGo, platformName(pl), round))
			}
			for _, v := range sd.Go {
				seen[v], all[v] = true, true
			}
		}
		byPlatform[pl] = sortedKeys(seen)
	}
	if len(all) > 1 {
		f = append(f, fmt.Sprintf("%s: the two sides didn't compile with the same Go: GitHub-hosted %s, Tentmaker %s",
			w.Name, orNone(byPlatform["github"]), orNone(byPlatform["tentmaker"])))
	}
	return f
}

func orNone(v []string) string {
	if len(v) == 0 {
		return "none recorded"
	}
	return strings.Join(v, " and ")
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

// LoadReport reads a benchmarks.json and recomputes every number and the
// gate from its samples, so an edited summary can't pass the gate.
func LoadReport(data []byte) (Report, error) {
	var old Report
	if err := json.Unmarshal(data, &old); err != nil {
		return Report{}, err
	}
	return Summarize(Run{URL: old.RunURL, SHA: old.SHA, Date: old.Date, Samples: old.Samples, Devices: old.Devices, Missing: old.Missing}), nil
}

func secs(c Cell) string {
	if c.N == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f s", c.Median)
}

func rate(c Cell) string {
	if c.N == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f MB/s", c.Median)
}

func link(text, url string) string {
	if url == "" {
		return text
	}
	return "[" + text + "](" + url + ")"
}

// Markdown renders docs/BENCHMARKS.md.
func Markdown(rep Report) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	p("# Benchmarks\n\n")
	p("How fast Tentmaker runs real CI jobs next to GitHub's hosted runners, measured by\n")
	p("[`bench.yml`](../.github/workflows/bench.yml) every week and on demand. This file and\n")
	p("[`benchmarks.json`](benchmarks.json) are written by `go run ./scripts/bench report`; don't edit\n")
	p("them by hand.\n\n")
	p("Source run: %s, commit `%s`, %s.\n\n", link("bench run", rep.RunURL), short(rep.SHA), rep.Date)

	p("## Hardware, honestly\n\n")
	p("Both sides run on GitHub's hosted Linux VMs. The GitHub-hosted numbers are plain `ubuntu-latest`\n")
	p("jobs. The Tentmaker numbers come from Tentmaker installed on another `ubuntu-latest` VM, used as\n")
	p("the device, with its default job size (2 CPUs and 2 GiB per job, where the hosted job gets the whole\n")
	p("VM). A real home or office machine is likely faster than this. Neither side is tuned.\n\n")
	if len(rep.Devices) > 0 {
		p("| Workload | Device image | CPUs | CPU | Memory | Tentmaker | Budget |\n|---|---|---|---|---|---|---|\n")
		for _, d := range rep.Devices {
			bj, _ := json.Marshal(d.Budget)
			p("| %s | %s %s | %d | %s | %.0f GiB | %s | `%s` |\n", link(d.Workload, d.JobURL), d.ImageOS, d.ImageVersion, d.CPUs, d.CPUModel, d.MemoryGiB, d.Version, bj)
		}
		p("\n")
	}

	p("## Job time\n\n")
	p("Median seconds from the job starting to its result. Cold: an empty cache. Warm: the same\n")
	p("workload again with the cache the cold run left (`actions/cache` on GitHub, Tentmaker's repo\n")
	p("cache on Tentmaker). Speedup is GitHub-hosted warm over Tentmaker warm. n counts passing warm\n")
	p("runs, GitHub-hosted / Tentmaker; for every workload but Docker, only warm runs that hit the cache\n")
	p("count, and a warm run that missed it is left out of every number.\n\n")
	p("| Workload | GitHub-hosted cold | GitHub-hosted warm | Tentmaker cold | Tentmaker warm | Speedup | n |\n")
	p("|---|---|---|---|---|---|---|\n")
	for _, w := range rep.Workloads {
		sp := "n/a"
		if w.SpeedupWarm > 0 {
			sp = fmt.Sprintf("%.1fx", w.SpeedupWarm)
		}
		p("| %s | %s | %s | %s | %s | %s | %d / %d |\n", w.Title,
			secs(w.GitHub["cold"].Job), secs(w.GitHub["warm"].Job),
			secs(w.Tentmaker["cold"].Job), secs(w.Tentmaker["warm"].Job), sp,
			w.GitHub["warm"].Job.N, w.Tentmaker["warm"].Job.N)
	}
	p("\n")
	var notes []string
	for _, w := range rep.Workloads {
		for _, pl := range []string{"github", "tentmaker"} {
			sd := w.GitHub["warm"]
			if pl == "tentmaker" {
				sd = w.Tentmaker["warm"]
			}
			if sd.Missed > 0 {
				notes = append(notes, fmt.Sprintf("%s, %s: %d warm runs missed the cache and are left out", w.Name, platformName(pl), sd.Missed))
			}
			if sd.Unrecorded > 0 {
				notes = append(notes, fmt.Sprintf("%s, %s: %d warm runs didn't record whether they hit the cache", w.Name, platformName(pl), sd.Unrecorded))
			}
		}
	}
	for _, n := range notes {
		p("- %s\n", n)
	}
	if len(notes) > 0 {
		p("\n")
	}
	p("Go used by each side (`go env GOVERSION`, every passing run):\n\n")
	for _, w := range rep.Workloads {
		if !infoOf(w.Name).Go {
			continue
		}
		var g, t []string
		for _, round := range []string{"cold", "warm"} {
			g = append(g, w.GitHub[round].Go...)
			t = append(t, w.Tentmaker[round].Go...)
		}
		p("- %s: GitHub-hosted %s; Tentmaker %s\n", w.Name, orNone(uniq(g)), orNone(uniq(t)))
	}
	p("\nEvery run, with links to each job, is in [`benchmarks.json`](benchmarks.json) (`samples`).\n\n")

	p("## Job start latency\n\n")
	p("Seconds from queued to running. GitHub-hosted: the job's `created_at` to `started_at`.\n")
	p("Tentmaker: the push to the `pending` status it posts when a device takes the job, so it also\n")
	p("counts noticing the commit.\n\n")
	p("| Platform | Median | Min | Max | Jobs |\n|---|---|---|---|---|\n")
	for _, pl := range []string{"github", "tentmaker"} {
		c := rep.StartLatency[pl]
		p("| %s | %s | %.1f s | %.1f s | %d |\n", platformName(pl), secs(c), c.Min, c.Max, c.N)
	}
	p("\n")

	p("## Where the time goes\n\n")
	p("Median seconds per phase, from marks the job itself writes (`scripts/bench/phase.sh`). setup is\n")
	p("the job start to the first mark: checkout and tool setup, plus cache restore on GitHub, where\n")
	p("`actions/cache` restores before the first mark.\n\n")
	p("| Workload | Platform | Round | Phases |\n|---|---|---|---|\n")
	for _, w := range rep.Workloads {
		for _, pl := range []string{"github", "tentmaker"} {
			m := w.GitHub
			if pl == "tentmaker" {
				m = w.Tentmaker
			}
			for _, round := range []string{"cold", "warm"} {
				sd := m[round]
				if len(sd.Phases) == 0 {
					continue
				}
				var parts []string
				for _, k := range phaseOrder(sd.Phases) {
					parts = append(parts, fmt.Sprintf("%s %s", k, secs(sd.Phases[k])))
				}
				p("| %s | %s | %s | %s |\n", w.Name, platformName(pl), round, strings.Join(parts, ", "))
			}
		}
	}
	p("\n")

	p("## Cache throughput\n\n")
	p("The cache workload moves a 256 MiB random blob (MB here is 10^6 bytes). GitHub-hosted:\n")
	p("`actions/cache/save` in the cold run and `actions/cache/restore` in the warm run, timed from the\n")
	p("step before to the step after. Tentmaker: a copy into its repo cache on the same disk, synced\n")
	p("(save), and a copy back out (restore), which the OS may serve from memory.\n\n")
	p("| Platform | Save (cold run) | Restore (warm run) |\n|---|---|---|\n")
	for _, w := range rep.Workloads {
		if w.Name != "cache" {
			continue
		}
		p("| GitHub-hosted | %s | %s |\n", rate(w.GitHub["cold"].MBps["save"]), rate(w.GitHub["warm"].MBps["restore"]))
		p("| Tentmaker | %s | %s |\n", rate(w.Tentmaker["cold"].MBps["save"]), rate(w.Tentmaker["warm"].MBps["restore"]))
	}
	p("\n")

	p("## Gate\n\n")
	p("The bench run fails when, on any workload but the Docker baseline, Tentmaker warm is slower\n")
	p("than GitHub-hosted warm by more than %.0f%% plus %.0f s (job times have whole-second\n", SlackFrac*100, SlackS)
	p("resolution), when Tentmaker's median cache restore throughput is lower, when either side has\n")
	p("fewer than %d passing warm runs that hit the cache or fewer than %d restore throughput\n", MinRuns, MinRuns)
	p("measurements, when a warm run didn't record whether it hit the cache, when the two sides didn't\n")
	p("record the same Go, or when the median Tentmaker job start latency is over %.0f s.\n\n", MaxStartS)
	if rep.Gate.Pass {
		p("This run: **pass**.\n\n")
	} else {
		p("This run: **fail**.\n\n")
		for _, f := range rep.Gate.Failures {
			p("- %s\n", f)
		}
		p("\n")
	}
	if len(rep.Missing) > 0 {
		p("Not measured in this run:\n\n")
		for _, m := range rep.Missing {
			p("- %s\n", m)
		}
		p("\n")
	}

	p("## Known limits of this bench\n\n")
	p("- CPUs: a Tentmaker job is capped at 2 CPUs (the default `job.cpus`); the hosted job has the\n")
	p("  VM's 4. That is most of why Tentmaker's cold runs are slower: they compile from nothing on\n")
	p("  half the CPUs.\n")
	p("- Toolchains: the Go workloads pin the exact Go in `go.mod`. GitHub-hosted jobs download it\n")
	p("  inside their setup time; the Tentmaker device has it installed in its tools folder, as a\n")
	p("  device you use every day would. Each run records its Go, and the gate fails when the sides\n")
	p("  differ. Node is 22 on both.\n")
	p("- Pickup outliers: the five bench devices share one workflow `GITHUB_TOKEN`. When its hourly\n")
	p("  budget runs low, Tentmaker stops polling until the reset, as designed, so a few runs wait\n")
	p("  minutes (the Max column above). A device with its own token doesn't share that budget. The\n")
	p("  gate uses the median.\n")
	p("- Pulls: base images come from the ECR Public mirror, which can answer 429 to a busy shared IP;\n")
	p("  a run that fails that way counts as failed, not slow. The gate leaves Docker out, so a failed\n")
	p("  Docker run doesn't fail the bench run.\n\n")

	p("## Blacksmith's published claims\n\n")
	p("We can't run Blacksmith, so these are **their claims, not our measurements**, quoted from their\n")
	p("docs as of 2026-11-22. Compare them with the tables above knowing they come from different\n")
	p("hardware and workloads.\n\n")
	p("- Runner boot: \"ephemeral Firecracker microVMs that boot in under 3 seconds\"\n")
	p("  ([overview](https://docs.blacksmith.sh/blacksmith-runners/overview.md)).\n")
	p("- Speed: \"Blacksmith runners are 2x faster and costs 67%% less than GitHub-hosted runners\"\n")
	p("  ([overview](https://docs.blacksmith.sh/blacksmith-runners/overview.md)).\n")
	p("- Cache: a \"co-located cache which quadruples cache throughput to 400MB/s\", and a table of\n")
	p("  90 MB/s for the GitHub Actions cache against 400 MB/s for theirs\n")
	p("  ([overview](https://docs.blacksmith.sh/blacksmith-runners/overview.md),\n")
	p("  [dependencies and sticky disks](https://docs.blacksmith.sh/blacksmith-caching/dependencies-sticky-disks.md)).\n")
	p("- Docker: \"customers have reported 2x to 40x improvements in build times\" from their layer cache\n")
	p("  ([Docker builds](https://docs.blacksmith.sh/blacksmith-caching/docker-builds.md)).\n\n")
	p("Tentmaker has no Docker layer cache yet; the Docker row above is the baseline for that work.\n\n")

	p("## Reproduce\n\n")
	p("```sh\ngh workflow run bench.yml --ref main\n```\n\n")
	p("The run's `bench-report` artifact holds this file and `benchmarks.json`. To rebuild them from the\n")
	p("raw data (`bench-raw` artifact): `go run ./scripts/bench report -raw <dir> -md docs/BENCHMARKS.md\n")
	p("-json docs/benchmarks.json`.\n")
	return b.String()
}

func uniq(v []string) []string {
	m := map[string]bool{}
	for _, s := range v {
		m[s] = true
	}
	return sortedKeys(m)
}

func platformName(p string) string {
	if p == "github" {
		return "GitHub-hosted"
	}
	return "Tentmaker"
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// phaseOrder puts setup first, then the rest by name.
func phaseOrder(m map[string]Cell) []string {
	k := sortedKeys(m)
	sort.SliceStable(k, func(i, j int) bool { return k[i] == "setup" && k[j] != "setup" })
	return k
}

func jsonIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }
