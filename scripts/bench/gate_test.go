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
// Tentmaker starting in tmStart seconds. The cache workload also gets
// restore and save rates.
func fleet(ghJob, tmJob, tmStart float64) []Sample {
	var out []Sample
	for _, wi := range workloadInfo {
		for _, round := range []string{"cold", "warm"} {
			for rep := 1; rep <= 3; rep++ {
				g := Sample{Workload: wi.Name, Platform: "github", Round: round, Rep: rep, OK: true, Conclusion: "success", StartS: 4, JobS: ghJob}
				m := Sample{Workload: wi.Name, Platform: "tentmaker", Round: round, Rep: rep, OK: true, Conclusion: "success", StartS: tmStart, JobS: tmJob}
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

func edit(ss []Sample, w, p, round string, f func(*Sample)) []Sample {
	for i := range ss {
		if ss[i].Workload == w && ss[i].Platform == p && ss[i].Round == round {
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
		{"slower warm", edit(fleet(50, 20, 3), "go-test", "tentmaker", "warm", func(s *Sample) { s.JobS = 51 }),
			"go-test: Tentmaker warm (51.0 s) is slower than GitHub-hosted warm (50.0 s)"},
		{"slower cold only", edit(fleet(50, 20, 3), "node", "tentmaker", "cold", func(s *Sample) { s.JobS = 500 }), ""},
		{"docker is a baseline", edit(fleet(50, 20, 3), "docker", "tentmaker", "warm", func(s *Sample) { s.JobS = 500 }), ""},
		{"slow start", fleet(50, 20, 6), "median Tentmaker job start latency is 6.0 s"},
		{"slower cache restore", edit(fleet(50, 20, 3), "cache", "tentmaker", "warm", func(s *Sample) { s.MBps["restore"] = 100 }),
			"cache: Tentmaker warm restore (100.0 MB/s) is slower than GitHub-hosted warm (150.0 MB/s)"},
		{"no Tentmaker restore rate", edit(fleet(50, 20, 3), "cache", "tentmaker", "warm", func(s *Sample) { s.MBps = nil }),
			"cache: no Tentmaker warm restore throughput"},
		{"failed runs don't count", edit(fleet(50, 20, 3), "cross-build", "tentmaker", "warm", func(s *Sample) {
			if s.Rep == 2 {
				s.OK, s.Conclusion = false, "failure"
			}
		}), "cross-build: 2 passing Tentmaker warm runs of 3; the gate needs 3"},
		{"too few hosted runs", edit(fleet(50, 20, 3), "go-test", "github", "warm", func(s *Sample) { s.Workload = "gone" }),
			"go-test: 0 passing GitHub-hosted warm runs of 0; the gate needs 3"},
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

// TestGateCommand runs the gate as bench.yml does. The JSON says the run
// passed, but its samples don't: the gate must recompute and fail.
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
	if !strings.Contains(out.String(), "FAIL: go-test: Tentmaker warm (60.0 s) is slower") {
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
