package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParsePhases(t *testing.T) {
	log := strings.Join([]string{
		"2026-11-22T10:00:58.0100000Z BENCH_PHASE deps begin 100.25",
		"BENCH_PHASE deps begin 101.0", // a second begin doesn't move the start
		"BENCH_PHASE deps end 103.75 0",
		"$ scripts/bench/phase.sh begin test", // the command echo is not a mark
		"BENCH_PHASE test begin 104",
		"BENCH_PHASE test end 110.5 2000000",
		"BENCH_PHASE late end 99", // ends before it begins: dropped
		"BENCH_PHASE late begin 100",
		"BENCH_PHASE open begin 120", // never ended: only counts for setup
	}, "\n")
	ph, mbps := phaseTimes(parsePhases(log), 95)
	want := map[string]float64{"deps": 3.5, "test": 6.5, "setup": 5} // the first begin is late's, at 100
	if !reflect.DeepEqual(ph, want) {
		t.Fatalf("phases = %v, want %v", ph, want)
	}
	if got := mbps["test"]; got != 0.3 {
		t.Fatalf("test MB/s = %v, want 0.3", got)
	}
	if _, ok := mbps["deps"]; ok {
		t.Fatalf("deps moved no bytes but has a rate: %v", mbps)
	}
}

func TestLoadRun(t *testing.T) {
	r, err := LoadRun("testdata/raw")
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "https://github.com/baney75/tentmaker/actions/runs/100" || r.Date != "2026-11-22" {
		t.Fatalf("run = %+v", r)
	}
	if len(r.Samples) != 3 {
		t.Fatalf("got %d samples, want 3 (the device job is not a sample): %+v", len(r.Samples), r.Samples)
	}
	gh := find(t, r.Samples, "cache", "github", "warm", 1)
	if !gh.OK || gh.StartS != 4 || gh.JobS != 56 {
		t.Fatalf("hosted sample = %+v", gh)
	}
	if want := map[string]float64{"restore": 2, "save": 0.1, "setup": 6}; !reflect.DeepEqual(gh.Phases, want) {
		t.Fatalf("hosted phases = %v, want %v", gh.Phases, want)
	}
	if gh.MBps["restore"] != 134.2 {
		t.Fatalf("hosted restore = %v MB/s, want 134.2", gh.MBps["restore"])
	}
	failed := find(t, r.Samples, "go-test", "github", "cold", 2)
	if failed.OK || failed.Conclusion != "failure" {
		t.Fatalf("failed hosted sample = %+v", failed)
	}
	tm := find(t, r.Samples, "cache", "tentmaker", "warm", 1)
	if !tm.OK || tm.StartS != 3 || tm.JobS != 9 {
		t.Fatalf("Tentmaker sample = %+v", tm)
	}
	if want := map[string]float64{"restore": 0.5, "save": 0, "setup": 2}; !reflect.DeepEqual(tm.Phases, want) {
		t.Fatalf("Tentmaker phases = %v, want %v", tm.Phases, want)
	}
	if gh.CacheHit == nil || !*gh.CacheHit || tm.CacheHit == nil || !*tm.CacheHit {
		t.Fatalf("cache hits: hosted %v, Tentmaker %v; both logs say 1", gh.CacheHit, tm.CacheHit)
	}
	if tm.MBps["restore"] != 536.9 {
		t.Fatalf("Tentmaker restore = %v MB/s, want 536.9", tm.MBps["restore"])
	}
	if tm.URL != "https://github.com/baney75/tentmaker/actions/runs/100/job/11" {
		t.Fatalf("Tentmaker sample links to %q, want its device job", tm.URL)
	}
	if len(r.Devices) != 1 || r.Devices[0].CPUs != 4 || r.Devices[0].JobURL != tm.URL {
		t.Fatalf("devices = %+v", r.Devices)
	}
	if len(r.Missing) != 1 || !strings.Contains(r.Missing[0], "gh go-test cold 2") {
		t.Fatalf("missing = %v, want the failed job's log", r.Missing)
	}
	// The API table shows the shared limit left, so a paced run is plain.
	if a := r.Devices[0].API; a == nil || a.RateLeft != 800 || a.RateLimit != 1000 {
		t.Fatalf("device API = %+v, want 800 of 1000 left", a)
	}
	if md := Markdown(Summarize(r)); !strings.Contains(md, "| 800 of 1000 |") {
		t.Fatalf("the API calls table has no limit left:\n%s", md)
	}
}

func find(t *testing.T, ss []Sample, w, p, round string, rep int) Sample {
	t.Helper()
	for _, s := range ss {
		if s.Workload == w && s.Platform == p && s.Round == round && s.Rep == rep {
			return s
		}
	}
	t.Fatalf("no sample %s %s %s %d in %+v", w, p, round, rep, ss)
	return Sample{}
}

func TestParseMeta(t *testing.T) {
	var s Sample
	s.OK = true
	log := "2026-11-22T10:00:58Z BENCH_PHASE build begin 1\nBENCH_PHASE build end 2 0\n" +
		"2026-11-22T10:00:58Z BENCH_META cache_hit 0\nBENCH_META go go1.25.0\n$ scripts/bench/phase.sh meta go x\n"
	if why := readLog(&s, log, 0); why != "" || !s.OK {
		t.Fatalf("readLog = %q, OK %v", why, s.OK)
	}
	if s.CacheHit == nil || *s.CacheHit || s.Go != "go1.25.0" {
		t.Fatalf("cache hit %v, Go %q; want a recorded miss and go1.25.0", s.CacheHit, s.Go)
	}
	if s.StoreBound != nil {
		t.Fatalf("store bound %v; the log didn't record it", s.StoreBound)
	}
	s = Sample{OK: true}
	readLog(&s, "BENCH_PHASE build begin 1\nBENCH_PHASE build end 2 0\nBENCH_META store_bound 0\nBENCH_META cache_hit 0\n", 0)
	if s.StoreBound == nil || *s.StoreBound || s.CacheHit == nil || *s.CacheHit {
		t.Fatalf("store bound %v, cache hit %v; want both recorded as 0", s.StoreBound, s.CacheHit)
	}
	s = Sample{OK: true}
	readLog(&s, "BENCH_PHASE build begin 1\nBENCH_PHASE build end 2 0\nBENCH_META store_bound 1\nBENCH_META cache_hit 1\n", 0)
	if s.StoreBound == nil || !*s.StoreBound || s.CacheHit == nil || !*s.CacheHit {
		t.Fatalf("store bound %v, cache hit %v; want both recorded as 1", s.StoreBound, s.CacheHit)
	}
	s = Sample{OK: true}
	if why := readLog(&s, "BENCH_PHASE build begin 1\nBENCH_PHASE build end 2 0\n", 0); why != "" || s.CacheHit != nil || s.Go != "" {
		t.Fatalf("a log with no BENCH_META: why %q, hit %v, Go %q", why, s.CacheHit, s.Go)
	}
}

// writeRaw writes files under a new raw folder, by path relative to it.
func writeRaw(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const okLog = "BENCH_PHASE build begin 1795341605\nBENCH_PHASE build end 1795341606 0\nBENCH_META cache_hit 1\n"

// TestTentmakerSampleNeedsTimes: sol-F2. A success with no pending status,
// or one before its pending, has no job time and must not pass as 0 s.
func TestTentmakerSampleNeedsTimes(t *testing.T) {
	meta := `{"workload": "go-test", "rep": 1, "round": "warm", "context": "tentmaker/go-test", "push_epoch": 1795341600}`
	for _, c := range []struct{ name, statuses string }{
		{"no pending", `[{"context": "tentmaker/go-test", "state": "success", "created_at": "2026-11-22T10:00:12Z"}]`},
		{"pending after the end", `[{"context": "tentmaker/go-test", "state": "success", "created_at": "2026-11-22T10:00:12Z"},
			{"context": "tentmaker/go-test", "state": "pending", "created_at": "2026-11-22T10:00:13Z"}]`},
		{"pending at the end", `[{"context": "tentmaker/go-test", "state": "success", "created_at": "2026-11-22T10:00:12Z"},
			{"context": "tentmaker/go-test", "state": "pending", "created_at": "2026-11-22T10:00:12Z"}]`},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := writeRaw(t, map[string]string{"meta.json": meta, "statuses.json": c.statuses, "log.txt": okLog})
			s, why, err := parseTentmakerSample(dir)
			if err != nil {
				t.Fatal(err)
			}
			if s.OK || s.JobS != 0 || !strings.Contains(why, "no pending status before its final one") {
				t.Fatalf("sample OK %v, job %v s, why %q; want not passing", s.OK, s.JobS, why)
			}
			if s.Conclusion != "success" {
				t.Fatalf("conclusion = %q; the status said success", s.Conclusion)
			}
		})
	}
	dir := writeRaw(t, map[string]string{"meta.json": meta, "log.txt": okLog, "statuses.json": `[
		{"context": "tentmaker/go-test", "state": "success", "created_at": "2026-11-22T10:00:12Z"},
		{"context": "tentmaker/go-test", "state": "pending", "created_at": "2026-11-22T10:00:03Z"}]`})
	if s, why, err := parseTentmakerSample(dir); err != nil || !s.OK || s.JobS != 9 || why != "" {
		t.Fatalf("ordered sample: OK %v, job %v s, why %q, err %v", s.OK, s.JobS, why, err)
	}
}

// TestGitHubSampleValidation: sol-F2 for the hosted side, and second-F5:
// a job that passed (or a continue-on-error job GitHub calls a success)
// counts only with an ordered start and end and a phase report in its log.
func TestGitHubSampleValidation(t *testing.T) {
	jobs := `[
	 {"id": 1, "name": "gh node warm 1 / node", "status": "completed", "conclusion": "success",
	  "created_at": "2026-11-22T10:00:00Z", "started_at": "2026-11-22T10:00:04Z"},
	 {"id": 2, "name": "gh node warm 2 / node", "status": "completed", "conclusion": "success",
	  "created_at": "2026-11-22T10:00:00Z", "started_at": "2026-11-22T10:00:04Z", "completed_at": "2026-11-22T10:00:02Z"},
	 {"id": 3, "name": "gh docker warm 1 / docker", "status": "completed", "conclusion": "success",
	  "created_at": "2026-11-22T10:00:00Z", "started_at": "2026-11-22T10:00:04Z", "completed_at": "2026-11-22T10:00:30Z"},
	 {"id": 4, "name": "gh node warm 3 / node", "status": "completed", "conclusion": "success",
	  "created_at": "2026-11-22T10:00:00Z", "started_at": "2026-11-22T10:00:04Z", "completed_at": "2026-11-22T10:00:30Z"}
	]`
	dir := writeRaw(t, map[string]string{
		"jobs.json": jobs, "logs/1.txt": okLog, "logs/2.txt": okLog, "logs/4.txt": okLog,
		"logs/3.txt": "##[error]Process completed with exit code 1.\n",
	})
	ss, missing, _, err := parseGitHub(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range ss {
		got[fmt.Sprintf("%s %s %d", s.Workload, s.Round, s.Rep)] = s.OK
	}
	want := map[string]bool{"node warm 1": false, "node warm 2": false, "docker warm 1": false, "node warm 3": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("passing = %v, want %v", got, want)
	}
	text := strings.Join(missing, "\n")
	for _, w := range []string{
		"gh node warm 1 / node: no ordered start and end time",
		"gh node warm 2 / node: no ordered start and end time",
		"gh docker warm 1 / docker: no phase report in its log",
	} {
		if !strings.Contains(text, w) {
			t.Errorf("missing = %q, want one containing %q", missing, w)
		}
	}
}

// TestMissingHostedLog: sol-F1. With the hosted cache log gone, the gate
// must fail on throughput instead of skipping the comparison.
func TestMissingHostedLog(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("testdata/raw")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "gh", "logs", "12.txt")); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := Summarize(r).Gate
	if g.Pass || !strings.Contains(strings.Join(g.Failures, "\n"), "cache: 0 GitHub-hosted warm restore throughput measurements; the gate needs 3") {
		t.Fatalf("gate = %+v, want a hosted throughput failure", g)
	}
}
