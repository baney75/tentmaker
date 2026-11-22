package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// cappedSource refuses a request whose body is over 1 MiB, as the mesh's
// peer API does.
type cappedSource struct {
	*source
	biggest int
}

func (s *cappedSource) Work(ctx context.Context, req core.WorkRequest) (core.WorkResponse, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return core.WorkResponse{}, err
	}
	if len(b) > s.biggest {
		s.biggest = len(b)
	}
	if len(b) > 1<<20 {
		return core.WorkResponse{}, fmt.Errorf("request is over 1 MiB (%d bytes)", len(b))
	}
	return s.source.Work(ctx, req)
}

// Many results with full log tails, waiting for a broker that was away,
// go out over several requests that each fit the mesh's 1 MiB cap, instead
// of one request the broker refuses forever (review r1 partA second-F6).
func TestPendingResultsFitTheRequestCap(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	w := r.addWorker("alpha", 1)
	src := &cappedSource{source: &source{inner: r.broker}}
	w.sources = []core.WorkSource{src}
	// A log tail of ANSI color codes: each byte escapes to six in JSON.
	tail := strings.Repeat("\x1b", 64<<10) // checks.logTailBytes
	var results []core.CheckResult
	for i := 0; i < 8; i++ {
		results = append(results, core.CheckResult{Runner: fmt.Sprintf("tm-gone-%d", i), State: "failure", LogTail: tail})
	}
	w.w.mu.Lock()
	w.w.reportLocked(src.Name()).Results = results
	w.w.mu.Unlock()
	r.run(20*time.Second, 2*time.Second)
	if st := w.w.Status(); st.PendingReports != 0 {
		t.Fatalf("pending reports = %d; biggest request %d bytes", st.PendingReports, src.biggest)
	}
	if src.biggest > 1<<20 {
		t.Fatalf("a request of %d bytes went out", src.biggest)
	}
}

// Two check jobs seen running end before an exchange, and their results
// together pass resultBudget. The one held back must not be reported ended
// without it: the broker would post "the job ended without a result" in
// place of its real state (review r2 partA first-A2-F1, sol-F2).
func TestHeldBackResultKeepsItsCheck(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	w := r.addWorker("alpha", 2)
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\nlint\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main"}})
	r.tick()
	names := started(w.ex)
	if len(names) != 2 {
		t.Fatalf("starts = %v, want test and lint", names)
	}
	r.run(2*time.Second, 2*time.Second) // the broker sees both running
	tail := strings.Repeat("\x1b", 64<<10)
	want := map[string]string{}
	for i, n := range names {
		spec, _ := w.ex.Spec(n)
		job := bundleOf(t, spec).Job.Name
		state := []string{"success", "failure"}[i]
		want[job] = state
		w.ex.SetResult(n, core.CheckResult{State: state, Description: "ran", LogTail: tail})
		w.ex.Exit(n)
	}
	r.run(10*time.Second, 2*time.Second)
	for job, state := range want {
		if got := r.forge.Status(repoSite, sha1, "tentmaker/"+job); got != state {
			t.Errorf("%s = %q, want %q", job, got, state)
		}
	}
	for _, p := range r.forge.Posts() {
		if strings.Contains(p.Description, "without a result") {
			t.Errorf("posted %q for a check whose result was held back", p.Description)
		}
	}
	if n := len(started(w.ex)); n != 2 {
		t.Errorf("starts = %d; a held-back check must not run again", n)
	}
	if st := w.w.Status(); st.PendingReports != 0 {
		t.Errorf("pending reports = %d", st.PendingReports)
	}
}
