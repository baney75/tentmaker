package mesh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

// RunsSource is this device's run history for the pool.
type RunsSource interface {
	// Runs answers q from this device: its stored and running records and,
	// on a broker when q names a repo, the jobs it knows are queued.
	Runs(q runs.Query) runs.Answer
	// RunLog is a job's redacted log tail (max bytes) when it ran or runs
	// here; core.ErrNotFound when it didn't.
	RunLog(ctx context.Context, id string, max int64) (runs.LogAnswer, error)
	// Rerun asks the forge to rerun a run's failed jobs with this broker's
	// token; core.ErrNotFound when this device has no forge for repo.
	Rerun(ctx context.Context, repo string, runID int64) error
	// RunTests is a job's record and the test results this device keeps
	// for it; core.ErrNotFound when it has no record of the job.
	RunTests(ctx context.Context, id string) (runs.TestsAnswer, error)
}

const (
	maxRunsAnswer = 8 << 20 // a pool device's slice of history
	maxLogAnswer  = 2 << 20 // a 512 KiB tail, base64 in JSON
	// maxTestsAnswer covers a run's kept test results (8 MiB at most).
	maxTestsAnswer = 10 << 20
	// rerunTimeout covers the broker's forge call.
	rerunTimeout = 20 * time.Second
)

// rerunRequest is POST /v1/rerun.
type rerunRequest struct {
	Device string `json:"device"`
	Repo   string `json:"repo"`
	RunID  int64  `json:"run_id"`
}

func (n *Node) handleRuns(w http.ResponseWriter, r *http.Request) {
	if n.o.Runs == nil {
		writeJSON(w, http.StatusOK, runs.Answer{Records: []runs.Record{}})
		return
	}
	q, err := runs.ParseQuery(r.URL.Query())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, n.o.Runs.Runs(q))
}

func (n *Node) handleRunLog(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !runs.ValidID(id) {
		writeErr(w, http.StatusBadRequest, "bad job id")
		return
	}
	max := int64(runs.LogTailBytes)
	if s := r.URL.Query().Get("max"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 && v < max {
			max = v
		}
	}
	if n.o.Runs == nil {
		writeErr(w, http.StatusNotFound, "no such job here")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), n.callTimeout)
	defer cancel()
	l, err := n.o.Runs.RunLog(ctx, id, max)
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeErr(w, http.StatusNotFound, "no such job here")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "can't read the job's log")
	default:
		writeJSON(w, http.StatusOK, l)
	}
}

func (n *Node) handleRunTests(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !runs.ValidID(id) {
		writeErr(w, http.StatusBadRequest, "bad job id")
		return
	}
	if n.o.Runs == nil {
		writeErr(w, http.StatusNotFound, "no such job here")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), n.callTimeout)
	defer cancel()
	t, err := n.o.Runs.RunTests(ctx, id)
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeErr(w, http.StatusNotFound, "no such job here")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "can't read the job's test results")
	default:
		writeJSON(w, http.StatusOK, t)
	}
}

func (n *Node) handleRerun(w http.ResponseWriter, r *http.Request) {
	var req rerunRequest
	if !readJSON(w, r, &req) {
		return
	}
	cctx, ok := n.callerIs(w, r, req.Device)
	if !ok {
		return
	}
	if n.o.Runs == nil || n.o.Broker == nil {
		writeErr(w, http.StatusNotFound, "this device is not a broker")
		return
	}
	if req.Repo == "" || req.RunID <= 0 {
		writeErr(w, http.StatusBadRequest, "name a repo and a run id")
		return
	}
	ctx, cancel := context.WithTimeout(cctx, rerunTimeout)
	defer cancel()
	err := n.o.Runs.Rerun(ctx, req.Repo, req.RunID)
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeErr(w, http.StatusNotFound, "this broker has no forge for that repo")
	case err != nil:
		n.log.Warn("mesh: rerun failed", "repo", req.Repo, "run", req.RunID, "for", req.Device, "error", err)
		writeErr(w, http.StatusBadGateway, "the forge refused the rerun: "+err.Error())
	default:
		n.log.Info("mesh: rerun asked", "repo", req.Repo, "run", req.RunID, "for", req.Device)
		writeJSON(w, http.StatusOK, map[string]any{})
	}
}

// PoolRuns answers q from every pool device and merges the records,
// newest first, cut to q.Limit. A device that doesn't answer is named in
// Errors; the rest still count.
func (n *Node) PoolRuns(ctx context.Context, q runs.Query) (runs.Answer, error) {
	n.ensureFresh(ctx)
	var self runs.Answer
	if n.o.Runs != nil {
		self = n.o.Runs.Runs(q)
	}
	peers := n.snapshot()
	answers := make([]runs.Answer, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		if p.skipped {
			answers[i].Errors = []string{p.hello.Device + ": not answering; skipped for now"}
			continue
		}
		wg.Add(1)
		go func(i int, p peerRef) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, n.statusTimeout)
			defer cancel()
			start := time.Now()
			err := n.callMax(cctx, http.MethodGet, p.base+"/v1/runs?"+q.Values().Encode(), nil, &answers[i], maxRunsAnswer)
			n.record(p.id, err, time.Since(start))
			if err != nil {
				answers[i] = runs.Answer{Errors: []string{p.hello.Device + ": " + err.Error()}}
			}
		}(i, p)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return runs.Answer{}, err
	}
	lists := [][]runs.Record{self.Records}
	out := runs.Answer{Queued: self.Queued, Errors: self.Errors}
	seenQ := map[string]bool{}
	for _, s := range self.Queued {
		seenQ[s] = true
	}
	for _, a := range answers {
		lists = append(lists, a.Records)
		out.Errors = append(out.Errors, a.Errors...)
		for _, s := range a.Queued {
			if !seenQ[s] {
				seenQ[s] = true
				out.Queued = append(out.Queued, s)
			}
		}
	}
	out.Records = runs.Apply(runs.Merge(lists...), q)
	return out, nil
}

// PoolRunLog finds the device that has job id and returns its log tail.
// An ended runner job's forge log, which its broker keeps, wins over the
// runner's diag log the device that ran it keeps.
func (n *Node) PoolRunLog(ctx context.Context, id string, max int64) (runs.LogAnswer, error) {
	if !runs.ValidID(id) {
		return runs.LogAnswer{}, errors.New("bad job id")
	}
	var fallback *runs.LogAnswer
	if n.o.Runs != nil {
		l, err := n.o.Runs.RunLog(ctx, id, max)
		if err == nil && l.Source == runs.LogRunnerDiag {
			fallback = &l
		} else if !errors.Is(err, core.ErrNotFound) {
			return l, err
		}
	}
	n.ensureFresh(ctx)
	v := url.Values{"id": {id}, "max": {strconv.FormatInt(max, 10)}}
	type got struct {
		l   runs.LogAnswer
		err error
	}
	peers := n.snapshot()
	ch := make(chan got, len(peers))
	cctx, cancel := context.WithTimeout(ctx, n.callTimeout)
	defer cancel()
	asked := 0
	for _, p := range peers {
		if p.skipped {
			continue
		}
		asked++
		go func(p peerRef) {
			var l runs.LogAnswer
			start := time.Now()
			err := n.callMax(cctx, http.MethodGet, p.base+"/v1/runs/log?"+v.Encode(), nil, &l, maxLogAnswer)
			if err != nil && !strings.Contains(err.Error(), ": 404 ") {
				n.record(p.id, err, time.Since(start))
			}
			ch <- got{l, err}
		}(p)
	}
	var errs []string
	for range asked {
		g := <-ch
		if g.err == nil {
			if g.l.Source != runs.LogRunnerDiag {
				return g.l, nil
			}
			if fallback == nil {
				fallback = &g.l
			}
			continue
		}
		if !strings.Contains(g.err.Error(), ": 404 ") {
			errs = append(errs, g.err.Error())
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	if len(errs) > 0 {
		return runs.LogAnswer{}, fmt.Errorf("job %s is on no device that answered (%s)", id, strings.Join(errs, "; "))
	}
	return runs.LogAnswer{}, fmt.Errorf("job %s: %w", id, core.ErrNotFound)
}

// PoolRerun reruns a run's failed jobs: this device does it when it is a
// broker for the repo, else the first broker in the pool that is. The
// forge token never leaves the broker.
func (n *Node) PoolRerun(ctx context.Context, repo string, runID int64) (string, error) {
	if n.o.Runs != nil && n.o.Broker != nil {
		err := n.o.Runs.Rerun(ctx, repo, runID)
		if !errors.Is(err, core.ErrNotFound) {
			return n.o.Device, err
		}
	}
	n.ensureFresh(ctx)
	var errs []string
	for _, p := range n.snapshot() {
		if !p.hello.Broker || p.skipped {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, rerunTimeout+n.callTimeout)
		err := n.call(cctx, http.MethodPost, p.base+"/v1/rerun", rerunRequest{Device: n.o.Device, Repo: repo, RunID: runID}, &struct{}{})
		cancel()
		if err == nil {
			return p.hello.Device, nil
		}
		if !strings.Contains(err.Error(), ": 404 ") {
			errs = append(errs, p.hello.Device+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return "", errors.New(strings.Join(errs, "; "))
	}
	return "", fmt.Errorf("no broker in the pool has a forge token for %s", repo)
}

// PoolRunTests finds job id's test results. The device that ran the job
// keeps them, so its answer wins; another device's answer (a broker that
// has only the job's forge result) is the fallback.
func (n *Node) PoolRunTests(ctx context.Context, id string) (runs.TestsAnswer, error) {
	if !runs.ValidID(id) {
		return runs.TestsAnswer{}, errors.New("bad job id")
	}
	var fallback *runs.TestsAnswer
	if n.o.Runs != nil {
		t, err := n.o.Runs.RunTests(ctx, id)
		switch {
		case err == nil && (t.Record.Device == n.o.Device || len(t.Results.Cases) > 0):
			return t, nil
		case err == nil:
			fallback = &t
		case !errors.Is(err, core.ErrNotFound):
			return t, err
		}
	}
	n.ensureFresh(ctx)
	type got struct {
		dev string
		t   runs.TestsAnswer
		err error
	}
	peers := n.snapshot()
	ch := make(chan got, len(peers))
	cctx, cancel := context.WithTimeout(ctx, n.callTimeout)
	defer cancel()
	asked := 0
	for _, p := range peers {
		if p.skipped {
			continue
		}
		if fallback != nil && fallback.Record.Device != "" && !strings.EqualFold(p.hello.Device, fallback.Record.Device) {
			continue // the record names the device that has the tests
		}
		asked++
		go func(p peerRef) {
			var t runs.TestsAnswer
			start := time.Now()
			err := n.callMax(cctx, http.MethodGet, p.base+"/v1/runs/tests?"+url.Values{"id": {id}}.Encode(), nil, &t, maxTestsAnswer)
			if err != nil && !strings.Contains(err.Error(), ": 404 ") {
				n.record(p.id, err, time.Since(start))
			}
			ch <- got{p.hello.Device, t, err}
		}(p)
	}
	var errs []string
	for range asked {
		g := <-ch
		if g.err == nil {
			if g.t.Record.Device == g.dev || len(g.t.Results.Cases) > 0 {
				return g.t, nil
			}
			if fallback == nil {
				fallback = &g.t
			}
			continue
		}
		if !strings.Contains(g.err.Error(), ": 404 ") {
			errs = append(errs, g.err.Error())
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	if len(errs) > 0 {
		return runs.TestsAnswer{}, fmt.Errorf("job %s is on no device that answered (%s)", id, strings.Join(errs, "; "))
	}
	return runs.TestsAnswer{}, fmt.Errorf("job %s: %w", id, core.ErrNotFound)
}
