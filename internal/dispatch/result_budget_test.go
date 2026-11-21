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
