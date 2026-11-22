package main

import (
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
