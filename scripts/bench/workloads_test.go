package main

import (
	"os"
	"path/filepath"
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
