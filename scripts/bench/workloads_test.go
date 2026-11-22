package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/checks"
)

// TestWorkloadsPlan runs Tentmaker's own planner over each workload file,
// as the bench's device does with .tm-scn/wf.yml: one job, named after the
// workload, nothing skipped, and every step present on GitHub's side too.
func TestWorkloadsPlan(t *testing.T) {
	for _, wi := range workloadInfo {
		path := filepath.Join("..", "..", ".github", "workflows", "bench-"+wi.Name+".yml")
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", wi.Name, err)
		}
		jobs, skips, err := checks.Plan(".tm-scn/wf.yml", src, "github")
		if err != nil {
			t.Fatalf("%s: %v", wi.Name, err)
		}
		if len(skips) != 0 {
			t.Fatalf("%s: checks mode skips %+v", wi.Name, skips)
		}
		if len(jobs) != 1 || checks.Context(jobs[0]) != "tentmaker/"+wi.Name {
			t.Fatalf("%s: planned %+v, want one job with context tentmaker/%s", wi.Name, jobs, wi.Name)
		}
		last := jobs[0].Steps[len(jobs[0].Steps)-1]
		if !strings.Contains(last.Run, "scripts/bench/phase.sh report") {
			t.Fatalf("%s: the last step is %q; the report must be last so its marks are in the kept log tail", wi.Name, last.Run)
		}
		if !strings.Contains(string(src), "workflow_call:") {
			t.Fatalf("%s: bench.yml calls the workload; it needs on.workflow_call", wi.Name)
		}
	}
}

func readRepo(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestWorkloadsRecord checks what each workload records for the gate, as
// checks mode plans it. first-F1: a Go workload pins the exact Go in go.mod
// (go-version-file is a minimum in checks mode, which took the device's
// newer Go) and records the Go it used. first-F2: a cached workload records
// whether it hit the cache, after its work and before the report.
func TestWorkloadsRecord(t *testing.T) {
	m := regexp.MustCompile(`(?m)^go (\S+)$`).FindStringSubmatch(readRepo(t, "go.mod"))
	if m == nil {
		t.Fatal("no go line in go.mod")
	}
	goMod := m[1]
	for _, wi := range workloadInfo {
		src := readRepo(t, filepath.Join(".github", "workflows", "bench-"+wi.Name+".yml"))
		jobs, _, err := checks.Plan(".tm-scn/wf.yml", []byte(src), "github")
		if err != nil || len(jobs) != 1 {
			t.Fatalf("%s: %v %+v", wi.Name, err, jobs)
		}
		steps := jobs[0].Steps
		var goPin string
		var runs []string
		for _, st := range steps {
			if v, ok := st.Tool["go"]; ok {
				goPin = v
			}
			runs = append(runs, st.Run)
		}
		all := strings.Join(runs, "\n")
		if wi.Go {
			if goPin != goMod {
				t.Errorf("%s: sets up Go %q; want exactly %q, go.mod's", wi.Name, goPin, goMod)
			}
			if regexp.MustCompile(`(?m)^\s+go-version-file:`).MatchString(src) {
				t.Errorf("%s: uses go-version-file, which checks mode reads as a minimum", wi.Name)
			}
			if !strings.Contains(all, `phase.sh meta go "$(go env GOVERSION)"`) {
				t.Errorf("%s: doesn't record its Go", wi.Name)
			}
		}
		if wi.Cached {
			// The step before the report records the hit; the cache
			// workload records it as it restores.
			before := steps[len(steps)-2].Run
			if !strings.Contains(before, "scripts/bench/hit.sh ") && !strings.Contains(all, "scripts/bench/blob.sh hit") {
				t.Errorf("%s: doesn't record whether it hit the cache (the step before the report is %q)", wi.Name, before)
			}
		}
	}
}

// TestCacheKeys: second-F7. With the run attempt in the key, "Re-run failed
// jobs" sent rerun warm reps to a key no cold rep had saved.
func TestCacheKeys(t *testing.T) {
	src := readRepo(t, filepath.Join(".github", "workflows", "bench.yml"))
	keys := regexp.MustCompile(`(?m)^\s+key: (.*)$`).FindAllStringSubmatch(src, -1)
	if len(keys) != 2*len(workloadInfo) {
		t.Fatalf("found %d cache keys, want a cold and a warm one per workload (%d)", len(keys), 2*len(workloadInfo))
	}
	for _, k := range keys {
		if k[1] != "${{ github.run_id }}-${{ matrix.rep }}" {
			t.Errorf("key %q; want the run id and the rep, the same for a rep's cold and warm runs", k[1])
		}
	}
}

// TestUngatedDontFailTheRun: second-F5. A workload the gate leaves out
// can't fail the bench run: its hosted job continues on error and bench.sh
// records its failures without failing. A gated workload's job does fail.
func TestUngatedDontFailTheRun(t *testing.T) {
	m := regexp.MustCompile(`(?m)^UNGATED="([^"]*)"$`).FindStringSubmatch(readRepo(t, filepath.Join("test", "scenarios", "bench.sh")))
	if m == nil {
		t.Fatal("bench.sh has no UNGATED list")
	}
	got := strings.Fields(m[1])
	sort.Strings(got)
	var want []string
	coe := regexp.MustCompile(`(?m)^    continue-on-error: true$`)
	for _, wi := range workloadInfo {
		src := readRepo(t, filepath.Join(".github", "workflows", "bench-"+wi.Name+".yml"))
		if !wi.Gated {
			want = append(want, wi.Name)
		}
		if has := coe.MatchString(src); has == wi.Gated {
			t.Errorf("%s: job continue-on-error is %v; want %v (gated: %v)", wi.Name, has, !wi.Gated, wi.Gated)
		}
	}
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("bench.sh UNGATED = %v; the report leaves out %v", got, want)
	}
}
