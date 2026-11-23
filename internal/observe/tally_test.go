package observe

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

func runnerRec(id, concl string) runs.Record {
	return runs.Record{ID: id, Kind: core.KindRunner, Forge: "github", Repo: "o/r", Device: "studio", Conclusion: concl,
		LogSource: runs.LogRunnerDiag}
}

// A runner job waits for its forge result. One that comes in time decides
// the series the job is counted in; one that comes after the wait moves
// nothing, since the job was counted already (review first-F1, sol-F2).
func TestTallyForgeResult(t *testing.T) {
	now := fixtureNow
	tl := NewTally(10 * time.Minute)

	// Tentmaker stopped it (cancelled by the diag log), then the forge
	// says failure in time: counted once, as a failure.
	tl.Ended(runnerRec("a", runs.Cancelled), false, now)
	if c := tl.Counts(now.Add(time.Minute)); c.Results[runs.Cancelled] != 0 || c.Pending != 1 {
		t.Fatalf("counted before its result: %+v", c)
	}
	tl.Forge(runnerRec("a", runs.Failure))
	c := tl.Counts(now.Add(time.Minute))
	if c.Results[runs.Failure] != 1 || c.Results[runs.Cancelled] != 0 || c.Pending != 0 {
		t.Fatalf("after the forge result: %+v", c)
	}

	// No result in time: counted as unknown, and a late success moves
	// nothing.
	tl.Ended(runnerRec("b", runs.Unknown), false, now)
	c = tl.Counts(now.Add(10 * time.Minute))
	if c.Results[runs.Unknown] != 1 || c.Pending != 0 {
		t.Fatalf("after the wait: %+v", c)
	}
	tl.Forge(runnerRec("b", runs.Success))
	tl.Forge(runnerRec("a", runs.Success))
	c = tl.Counts(now.Add(11 * time.Minute))
	if c.Results[runs.Unknown] != 1 || c.Results[runs.Success] != 0 || c.Results[runs.Failure] != 1 {
		t.Fatalf("a late result moved a counted job: %+v", c)
	}

	// A final job counts at once; a forge result for a job never fed (it
	// ended before the daemon started) counts nothing.
	tl.Ended(runs.Record{ID: "c", Kind: core.KindCheck, Conclusion: runs.Success}, true, now)
	tl.Forge(runnerRec("never-fed", runs.Success))
	if c = tl.Counts(now.Add(11 * time.Minute)); c.Results[runs.Success] != 1 {
		t.Fatalf("final and unknown jobs: %+v", c)
	}
}

// Whatever order jobs end, results come and scrapes happen in, no series
// ever goes down and each job is counted at most once.
func TestTallySeriesNeverDecrease(t *testing.T) {
	rng := rand.New(rand.NewSource(45))
	concls := []string{runs.Success, runs.Failure, runs.Cancelled, runs.Error, runs.Unknown}
	tl := NewTally(5 * time.Minute)
	now := fixtureNow
	prev := tl.Counts(now)
	ended := 0
	var ids []string
	for i := 0; i < 5000; i++ {
		now = now.Add(time.Duration(rng.Intn(90)) * time.Second)
		switch rng.Intn(3) {
		case 0:
			id := fmt.Sprintf("tm-%d", i)
			ids = append(ids, id)
			r := runnerRec(id, concls[rng.Intn(len(concls))])
			r.QueuedAt, r.StartedAt = now.Add(-time.Minute), now.Add(-time.Duration(rng.Intn(60))*time.Second)
			r.RepoCache = []string{"", core.CacheHit, core.CacheMiss}[rng.Intn(3)]
			tl.Ended(r, rng.Intn(2) == 0, now)
			ended++
		case 1:
			if len(ids) > 0 {
				tl.Forge(runnerRec(ids[rng.Intn(len(ids))], concls[rng.Intn(len(concls))]))
			}
		}
		c := tl.Counts(now)
		var total int64
		for k, v := range prev.Results {
			if c.Results[k] < v {
				t.Fatalf("step %d: %s went from %d to %d", i, k, v, c.Results[k])
			}
		}
		for _, v := range c.Results {
			total += v
		}
		for k, v := range prev.Cache {
			if c.Cache[k] < v {
				t.Fatalf("step %d: cache %v went down", i, k)
			}
		}
		for k, v := range prev.Buckets {
			if c.Buckets[k] < v {
				t.Fatalf("step %d: bucket %d went down", i, k)
			}
		}
		if c.LatencyCount < prev.LatencyCount || c.LatencySum < prev.LatencySum || total+int64(c.Pending) != int64(ended) {
			t.Fatalf("step %d: latency %d->%d, counted %d + pending %d of %d ended", i, prev.LatencyCount, c.LatencyCount, total, c.Pending, ended)
		}
		prev = c
	}
}

// A pile of jobs waiting for results can't grow without bound: past
// maxPending the oldest is counted with what it has.
func TestTallyPendingIsBounded(t *testing.T) {
	tl := NewTally(time.Hour)
	for i := 0; i < maxPending+10; i++ {
		tl.Ended(runnerRec(fmt.Sprintf("j%d", i), runs.Unknown), false, fixtureNow)
	}
	c := tl.Counts(fixtureNow)
	if c.Pending != maxPending || c.Results[runs.Unknown] != 10 {
		t.Fatalf("pending %d unknown %d", c.Pending, c.Results[runs.Unknown])
	}
	tl.Forge(runnerRec("j0", runs.Success))
	if c := tl.Counts(fixtureNow); c.Results[runs.Success] != 0 {
		t.Fatalf("the oldest, counted already, moved: %+v", c.Results)
	}
	var nilTally *Tally
	nilTally.Ended(runnerRec("x", runs.Success), true, fixtureNow)
	nilTally.Forge(runnerRec("x", runs.Success))
	if c := nilTally.Counts(fixtureNow); len(c.Results) != 0 || len(c.Buckets) != len(LatencyBuckets) {
		t.Fatalf("nil tally: %+v", c)
	}
}

// The last failures and the day's failure count come from the failures
// read on their own, so 200 newer successes can't hide one (review
// sol-F3); a history that history_max cut inside the day says so.
func TestPageFailuresSurviveNewerSuccesses(t *testing.T) {
	var newer []runs.Record
	for i := 0; i < 200; i++ {
		r := runnerRec(fmt.Sprintf("tm-ok-%d", i), runs.Success)
		r.EndedAt = fixtureNow.Add(-time.Duration(i) * time.Second)
		newer = append(newer, r)
	}
	fail := runnerRec("tm-bad", runs.Failure)
	fail.Job, fail.FailedStep, fail.EndedAt = "lint", "golangci-lint", fixtureNow.Add(-time.Hour)
	old := runnerRec("tm-old", runs.Error)
	old.Job, old.EndedAt = "deploy", fixtureNow.Add(-30*time.Hour)
	page := renderPage(t, PageInput{Now: fixtureNow, Runs: newer, Failures: []runs.Record{old, fail, fail}})
	text := strings.Join(strings.Fields(page), " ")
	for _, want := range []string{"<b>1</b><span>failed in 24 h</span>", "golangci-lint", "o/r · deploy"} {
		if !strings.Contains(text, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(text, "history_max") {
		t.Errorf("page notes a history limit it doesn't have")
	}

	page = renderPage(t, PageInput{Now: fixtureNow, Failures: []runs.Record{fail}, HistoryFrom: fixtureNow.Add(-3 * time.Hour)})
	text = strings.Join(strings.Fields(page), " ")
	for _, want := range []string{"failed in the last 3h", "goes back only 3h on at least one device (history_max)"} {
		if !strings.Contains(text, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// A cut older than a day changes nothing.
	page = renderPage(t, PageInput{Now: fixtureNow, Failures: []runs.Record{fail}, HistoryFrom: fixtureNow.Add(-30 * time.Hour)})
	if strings.Contains(page, "history_max") {
		t.Errorf("a history older than a day noted as cut")
	}
}
