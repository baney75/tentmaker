package observe

import (
	"sort"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

// JobCounts are the job counters a device exports: cumulative since its
// daemon started, each job counted once, when its result was final. The
// run history's eviction (history_max) and a forge result that comes late
// change none of them.
type JobCounts struct {
	// Results counts ended jobs by conclusion (runs.Success and so on).
	Results map[string]int64
	// Latency is the start latency histogram (queued at the forge to
	// running here): Buckets[i] counts jobs that waited at most
	// LatencyBuckets[i] seconds.
	Buckets      []int64
	LatencyCount int64
	LatencySum   float64
	// Cache counts jobs by cache outcome: "repo" or "container_store",
	// then core.CacheHit or core.CacheMiss.
	Cache map[[2]string]int64
	// Pending is how many ended jobs still wait for a final result.
	Pending int
}

// maxPending caps the jobs a Tally holds while their forge result may
// still come; past it the oldest is counted with what it has.
const maxPending = 1024

// Tally keeps JobCounts. The daemon feeds it every job this device ran as
// the job ends, and every forge result for one of them. A runner job whose
// forge result may still come waits, up to the wait given to NewTally (the
// broker's whole retry budget, dispatch.ReportBudget); then it is counted
// with the conclusion it has. A counted job is never counted again, so a
// result that comes later moves nothing between series. Safe for
// concurrent use; a nil Tally counts nothing.
type Tally struct {
	wait time.Duration

	mu      sync.Mutex
	pending map[string]pendingJob
	seq     uint64
	c       JobCounts
}

type pendingJob struct {
	rec runs.Record
	due time.Time
	seq uint64
}

// NewTally returns an empty Tally whose jobs wait at most wait for their
// forge result.
func NewTally(wait time.Duration) *Tally {
	return &Tally{wait: wait, pending: map[string]pendingJob{},
		c: JobCounts{Results: map[string]int64{}, Buckets: make([]int64, len(LatencyBuckets)), Cache: map[[2]string]int64{}}}
}

// Ended takes a job this device ran, as it ended at now. final says its
// result can't change any more: a check job, a runner job whose forge
// result came already, or one no broker on this device reads a result
// for. Else the job waits for Forge or for the wait to pass.
func (t *Tally) Ended(rec runs.Record, final bool, now time.Time) {
	if t == nil || !rec.Ended() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.settleLocked(now)
	if final {
		delete(t.pending, rec.ID)
		t.countLocked(rec)
		return
	}
	t.seq++
	t.pending[rec.ID] = pendingJob{rec: rec, due: now.Add(t.wait), seq: t.seq}
	for len(t.pending) > maxPending {
		t.countLocked(t.popOldestLocked())
	}
}

// Forge takes a job's record once its forge result is on it. A job still
// waiting is counted now, with this record; any other job (counted already,
// or ended before the daemon started) is left alone.
func (t *Tally) Forge(rec runs.Record) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.pending[rec.ID]; !ok {
		return
	}
	delete(t.pending, rec.ID)
	t.countLocked(rec)
}

// Counts counts the jobs whose wait is over at now and returns a copy of
// the counters.
func (t *Tally) Counts(now time.Time) JobCounts {
	if t == nil {
		return NewTally(0).Counts(now)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.settleLocked(now)
	out := JobCounts{Results: map[string]int64{}, Buckets: append([]int64(nil), t.c.Buckets...),
		LatencyCount: t.c.LatencyCount, LatencySum: t.c.LatencySum, Cache: map[[2]string]int64{}, Pending: len(t.pending)}
	for k, v := range t.c.Results {
		out.Results[k] = v
	}
	for k, v := range t.c.Cache {
		out.Cache[k] = v
	}
	return out
}

// settleLocked counts every waiting job that is due, oldest first.
func (t *Tally) settleLocked(now time.Time) {
	var due []pendingJob
	for id, p := range t.pending {
		if !now.Before(p.due) {
			due = append(due, p)
			delete(t.pending, id)
		}
	}
	sort.Slice(due, func(i, k int) bool { return due[i].seq < due[k].seq })
	for _, p := range due {
		t.countLocked(p.rec)
	}
}

func (t *Tally) popOldestLocked() runs.Record {
	var oldest pendingJob
	first := true
	for _, p := range t.pending {
		if first || p.seq < oldest.seq {
			oldest, first = p, false
		}
	}
	delete(t.pending, oldest.rec.ID)
	return oldest.rec
}

func (t *Tally) countLocked(r runs.Record) {
	c := r.Conclusion
	if c == "" {
		c = runs.Unknown
	}
	t.c.Results[c]++
	if !r.QueuedAt.IsZero() && !r.StartedAt.IsZero() {
		v := max(0, r.StartedAt.Sub(r.QueuedAt).Seconds())
		for i, le := range LatencyBuckets {
			if v <= le {
				t.c.Buckets[i]++
			}
		}
		t.c.LatencyCount++
		t.c.LatencySum += v
	}
	if r.RepoCache == core.CacheHit || r.RepoCache == core.CacheMiss {
		t.c.Cache[[2]string{"repo", r.RepoCache}]++
	}
	if r.ContainerStore == core.CacheHit || r.ContainerStore == core.CacheMiss {
		t.c.Cache[[2]string{"container_store", r.ContainerStore}]++
	}
}
