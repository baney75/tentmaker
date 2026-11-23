package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fleet makes three cold and three warm passing runs of every workload on
// both platforms: GitHub-hosted at ghJob seconds, Tentmaker at tmJob,
// Tentmaker starting in tmStart seconds. Warm runs of cached workloads hit
// the cache, Go workloads record go1.25.0 on both sides, and the cache
// workload also gets restore and save rates.
func fleet(ghJob, tmJob, tmStart float64) []Sample {
	var out []Sample
	for _, wi := range workloadInfo {
		for _, round := range []string{"cold", "warm"} {
			for rep := 1; rep <= 3; rep++ {
				g := Sample{Workload: wi.Name, Platform: "github", Round: round, Rep: rep, OK: true, Conclusion: "success", StartS: 4, JobS: ghJob}
				m := Sample{Workload: wi.Name, Platform: "tentmaker", Round: round, Rep: rep, OK: true, Conclusion: "success", StartS: tmStart, JobS: tmJob}
				if wi.Cached {
					hit := round == "warm"
					g.CacheHit, m.CacheHit = ptr(hit), ptr(hit)
				}
				if wi.Go {
					g.Go, m.Go = "go1.25.0", "go1.25.0"
				}
				if wi.Name == "cache" {
					g.MBps = map[string]float64{"restore": 150, "save": 90}
					m.MBps = map[string]float64{"restore": 900, "save": 700}
				}
				out = append(out, g, m)
			}
		}
	}
	return out
}

func gate(ss []Sample) GateResult { return Summarize(Run{Samples: ss}).Gate }

// one edits the sample of one rep.
func one(rep int, f func(*Sample)) func(*Sample) {
	return func(s *Sample) {
		if s.Rep == rep {
			f(s)
		}
	}
}

// edit changes the samples of one workload; an empty platform or round
// matches every one.
func edit(ss []Sample, w, p, round string, f func(*Sample)) []Sample {
	for i := range ss {
		if ss[i].Workload == w && (p == "" || ss[i].Platform == p) && (round == "" || ss[i].Round == round) {
			f(&ss[i])
		}
	}
	return ss
}

func TestGate(t *testing.T) {
	cases := []struct {
		name    string
		samples []Sample
		want    string // a failure must contain this; "" means the gate passes
	}{
		{"faster everywhere", fleet(50, 20, 3), ""},
		{"equal is not slower", fleet(20, 20, 5), ""},
		// Job times have whole-second resolution: 1 s is inside the
		// allowance, 18 * 1.05 + 1 = 19.9 s is its edge.
		{"1 s slower is noise", fleet(18, 19, 3), ""},
		{"slower than the allowance", fleet(18, 20, 3),
			"go-test: Tentmaker warm (20.0 s) is slower than GitHub-hosted warm (18.0 s) by more than the 5% + 1 s allowance"},
		{"slower warm", edit(fleet(50, 20, 3), "go-test", "tentmaker", "warm", func(s *Sample) { s.JobS = 54 }),
			"go-test: Tentmaker warm (54.0 s) is slower than GitHub-hosted warm (50.0 s) by more than the 5% + 1 s allowance"},
		{"slower cold only", edit(fleet(50, 20, 3), "node", "tentmaker", "cold", func(s *Sample) { s.JobS = 500 }), ""},
		{"docker is a baseline", edit(fleet(50, 20, 3), "docker", "tentmaker", "warm", func(s *Sample) { s.JobS = 500 }), ""},
		{"slow start", fleet(50, 20, 6), "median Tentmaker job start latency is 6.0 s"},
		{"slower cache restore", edit(fleet(50, 20, 3), "cache", "tentmaker", "warm", func(s *Sample) { s.MBps["restore"] = 100 }),
			"cache: Tentmaker warm restore (100.0 MB/s) is slower than GitHub-hosted warm (150.0 MB/s)"},
		{"no Tentmaker restore rate", edit(fleet(50, 20, 3), "cache", "tentmaker", "warm", func(s *Sample) { s.MBps = nil }),
			"cache: 0 Tentmaker warm restore throughput measurements; the gate needs 3"},
		// sol-F1: hosted logs without throughput marks used to drop the
		// throughput check altogether.
		{"no hosted restore rate", edit(fleet(50, 20, 3), "cache", "github", "warm", func(s *Sample) { s.MBps = nil }),
			"cache: 0 GitHub-hosted warm restore throughput measurements; the gate needs 3"},
		{"one hosted restore rate", edit(fleet(50, 20, 3), "cache", "github", "warm", func(s *Sample) {
			if s.Rep != 1 {
				s.MBps = map[string]float64{"save": 90}
			}
		}), "cache: 1 GitHub-hosted warm restore throughput measurements; the gate needs 3"},
		{"two Tentmaker restore rates", edit(fleet(50, 20, 3), "cache", "tentmaker", "warm", one(3, func(s *Sample) { s.MBps = nil })),
			"cache: 2 Tentmaker warm restore throughput measurements; the gate needs 3"},
		{"failed runs don't count", edit(fleet(50, 20, 3), "cross-build", "tentmaker", "warm", one(2, func(s *Sample) {
			s.OK, s.Conclusion = false, "failure"
		})), "cross-build: 2 passing Tentmaker warm runs that hit the cache, of 3 (1 failed, 0 missed the cache); the gate needs 3"},
		{"too few hosted runs", edit(fleet(50, 20, 3), "go-test", "github", "warm", func(s *Sample) { s.Workload = "gone" }),
			"go-test: 0 passing GitHub-hosted warm runs that hit the cache, of 0 (0 failed, 0 missed the cache); the gate needs 3"},
		{"too few Tentmaker runs", edit(fleet(50, 20, 3), "node", "tentmaker", "warm", one(1, func(s *Sample) { s.Workload = "gone" })),
			"node: 2 passing Tentmaker warm runs that hit the cache, of 2 (0 failed, 0 missed the cache); the gate needs 3"},
		// sol-F2: a passing run with no job time counted as 0 s.
		{"no job time is not a pass", edit(fleet(50, 20, 3), "go-test", "tentmaker", "warm", func(s *Sample) { s.JobS = 0 }),
			"go-test: 0 passing Tentmaker warm runs that hit the cache, of 3 (3 failed, 0 missed the cache); the gate needs 3"},
		// first-F2: a warm run that missed the cache is left out.
		{"a warm miss is left out", edit(fleet(50, 20, 3), "go-test", "github", "warm", one(2, func(s *Sample) { s.CacheHit = ptr(false) })),
			"go-test: 2 passing GitHub-hosted warm runs that hit the cache, of 3 (0 failed, 1 missed the cache); the gate needs 3"},
		{"an unrecorded hit fails", edit(fleet(50, 20, 3), "node", "tentmaker", "warm", one(1, func(s *Sample) { s.CacheHit = nil })),
			"node: 1 passing Tentmaker warm runs didn't record whether they hit the cache"},
		{"three hits and a miss pass", append(fleet(50, 20, 3), Sample{Workload: "go-test", Platform: "tentmaker", Round: "warm", Rep: 4,
			OK: true, Conclusion: "success", StartS: 3, JobS: 500, Go: "go1.25.0", CacheHit: ptr(false)}), ""},
		// first-F1: both sides must compile with the same Go.
		{"different Go", edit(fleet(50, 20, 3), "go-test", "tentmaker", "", func(s *Sample) { s.Go = "go1.26.11" }),
			"go-test: the two sides didn't compile with the same Go: GitHub-hosted go1.25.0, Tentmaker go1.26.11"},
		{"Go not recorded", edit(fleet(50, 20, 3), "cross-build", "github", "cold", func(s *Sample) { s.Go = "" }),
			"cross-build: 3 passing GitHub-hosted cold runs recorded no Go version"},
		{"node needs no Go", edit(fleet(50, 20, 3), "node", "", "", func(s *Sample) { s.Go = "" }), ""},
		{"nothing ran", nil, "no Tentmaker job start latency was measured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := gate(c.samples)
			if c.want == "" {
				if !g.Pass || len(g.Failures) != 0 {
					t.Fatalf("gate failed: %v", g.Failures)
				}
				return
			}
			if g.Pass {
				t.Fatalf("gate passed; want a failure containing %q", c.want)
			}
			if !strings.Contains(strings.Join(g.Failures, "\n"), c.want) {
				t.Fatalf("failures = %q, want one containing %q", g.Failures, c.want)
			}
		})
	}
}

func TestStartLatencyMedian(t *testing.T) {
	ss := fleet(50, 20, 3)
	// Unmeasured starts (-1) are left out, and the median is over every
	// Tentmaker job, cold and warm, passing or not.
	ss = edit(ss, "go-test", "tentmaker", "cold", func(s *Sample) { s.StartS = -1 })
	rep := Summarize(Run{Samples: ss})
	if c := rep.StartLatency["tentmaker"]; c.N != 27 || c.Median != 3 {
		t.Fatalf("Tentmaker start latency = %+v, want 27 jobs, median 3", c)
	}
}

// p95 is nearest rank: the 19th of 20 sorted starts, the 2nd of 2.
func TestStartLatencyP95(t *testing.T) {
	var v []float64
	for i := 1; i <= 20; i++ {
		v = append(v, float64(i))
	}
	if c := cellOf(v); c.P95 != 19 || c.Median != 10.5 {
		t.Fatalf("cell = %+v, want p95 19", c)
	}
	if c := cellOf([]float64{1, 30}); c.P95 != 30 {
		t.Fatalf("two starts: p95 %v, want 30", c.P95)
	}
}

// TestGateCommand runs the gate as bench.yml does. The JSON says the run
// passed, but its samples don't: the gate must recompute and fail.
// TestWarmMissLeftOut: a warm run that missed the cache is a cold run in
// disguise; it must not move the warm median.
func TestWarmMissLeftOut(t *testing.T) {
	ss := append(fleet(50, 20, 3), Sample{Workload: "go-test", Platform: "tentmaker", Round: "warm", Rep: 4,
		OK: true, Conclusion: "success", StartS: 3, JobS: 500, Go: "go1.25.0", CacheHit: ptr(false)})
	rep := Summarize(Run{Samples: ss})
	tw := rep.Workloads[0].Tentmaker["warm"]
	if tw.Job.Median != 20 || tw.Job.N != 3 || tw.Hits != 3 || tw.Missed != 1 || tw.Runs != 4 {
		t.Fatalf("Tentmaker go-test warm = %+v, want median 20 over 3 hits, 1 missed", tw)
	}
	md := Markdown(rep)
	if !strings.Contains(md, "go-test, Tentmaker: 1 warm runs missed the cache and are left out") {
		t.Fatalf("the page doesn't say a warm run was left out:\n%s", md)
	}
	if !strings.Contains(md, "- go-test: GitHub-hosted go1.25.0; Tentmaker go1.25.0") {
		t.Fatalf("the page doesn't say which Go each side used:\n%s", md)
	}
}

func TestGateCommand(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, rep Report) string {
		p := filepath.Join(dir, name)
		data, err := json.Marshal(rep)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.json", Summarize(Run{Samples: fleet(50, 20, 3)}))
	lie := Summarize(Run{Samples: fleet(50, 60, 3)})
	lie.Gate = GateResult{Pass: true}
	bad := write("bad.json", lie)

	var out, errb strings.Builder
	if rc := run([]string{"gate", "-json", good}, &out, &errb); rc != 0 {
		t.Fatalf("good run: exit %d, %s%s", rc, out.String(), errb.String())
	}
	out.Reset()
	if rc := run([]string{"gate", "-json", bad}, &out, &errb); rc != 1 {
		t.Fatalf("slower run: exit %d, want 1; %s", rc, out.String())
	}
	if !strings.Contains(out.String(), "FAIL: go-test: Tentmaker warm (60.0 s) is slower than GitHub-hosted warm (50.0 s) by more") {
		t.Fatalf("gate output = %q", out.String())
	}
}

func TestReportCommand(t *testing.T) {
	dir := t.TempDir()
	md, js := filepath.Join(dir, "BENCHMARKS.md"), filepath.Join(dir, "benchmarks.json")
	var out, errb strings.Builder
	if rc := run([]string{"report", "-raw", "testdata/raw", "-md", md, "-json", js}, &out, &errb); rc != 0 {
		t.Fatalf("exit %d: %s", rc, errb.String())
	}
	text, err := os.ReadFile(md)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| Cache: 256 MiB blob save, then restore | n/a | 56.0 s | n/a | 9.0 s | 6.2x | 1 / 1 |",
		"| GitHub-hosted | n/a | 134 MB/s |",
		"| Tentmaker | n/a | 537 MB/s |",
		"their claims, not our measurements",
		"This run: **fail**.",
		"| cache | 0.50 | 7200 | 120 | 98% | 0 |",
	} {
		if !strings.Contains(string(text), want) {
			t.Errorf("BENCHMARKS.md lacks %q", want)
		}
	}
	if strings.ContainsAny(string(text), "\u2013\u2014") {
		t.Error("BENCHMARKS.md has an en or em dash")
	}
	if rc := run([]string{"gate", "-json", js}, &out, &errb); rc != 1 {
		t.Fatalf("the fixture has one run per cell; the gate must fail it, got exit %d", rc)
	}
}
