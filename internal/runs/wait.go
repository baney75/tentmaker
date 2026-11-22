package runs

import (
	"context"
	"errors"
	"time"
)

// Snapshot is what one look at the pool shows for a commit: its records
// (ended and running) and the jobs brokers still hold queued for it.
type Snapshot struct {
	Records []Record
	Queued  []string
}

// WaitOptions tunes Wait.
type WaitOptions struct {
	Timeout time.Duration // give up after this; required
	Poll    time.Duration // between looks; default 5 s
	// Settle is how long every job must stay ended, with nothing queued,
	// before Wait believes the run is over: a job that needs another one
	// queues only after it ends. Default 20 s.
	Settle time.Duration
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
}

// WaitResult is how Wait ended.
type WaitResult struct {
	Done     bool     `json:"done"`
	TimedOut bool     `json:"timed_out"`
	Waited   float64  `json:"waited_s"`
	Records  []Record `json:"records"`
	Queued   []string `json:"queued,omitempty"`
	// Conclusion sums the jobs: failure if any failed or errored, cancelled
	// if any was cancelled, else success; "" when nothing ended.
	Conclusion string `json:"conclusion,omitempty"`
}

// Wait looks at the pool through look until every job it shows for the
// commit has ended and nothing is queued for it for Settle, or Timeout
// passes. A look that fails is retried at the next poll; Wait returns its
// error only if no look ever worked.
func Wait(ctx context.Context, look func(ctx context.Context) (Snapshot, error), o WaitOptions) (WaitResult, error) {
	if o.Poll <= 0 {
		o.Poll = 5 * time.Second
	}
	if o.Settle < 0 {
		o.Settle = 0
	} else if o.Settle == 0 {
		o.Settle = 20 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	start := o.Now()
	deadline := start.Add(o.Timeout)
	var last Snapshot
	var lastErr error
	looked := false
	var quietSince time.Time
	for {
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		snap, err := look(lctx)
		cancel()
		now := o.Now()
		if err == nil {
			looked, last = true, snap
			if settled(snap, now) {
				if quietSince.IsZero() {
					quietSince = now
				}
				if now.Sub(quietSince) >= o.Settle {
					return result(last, true, false, now.Sub(start)), nil
				}
			} else {
				quietSince = time.Time{}
			}
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			return result(last, false, false, now.Sub(start)), ctx.Err()
		}
		if !now.Before(deadline) {
			if !looked && lastErr != nil {
				return WaitResult{}, lastErr
			}
			return result(last, false, true, now.Sub(start)), nil
		}
		d := min(o.Poll, deadline.Sub(now))
		if err := o.Sleep(ctx, d); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return result(last, false, false, o.Now().Sub(start)), err
			}
		}
	}
}

func settled(s Snapshot, now time.Time) bool {
	if len(s.Records) == 0 || len(s.Queued) > 0 {
		return false
	}
	for _, r := range s.Records {
		if !r.Ended() || r.awaitingForge(now) {
			return false
		}
	}
	return true
}

func result(s Snapshot, done, timedOut bool, waited time.Duration) WaitResult {
	r := WaitResult{Done: done, TimedOut: timedOut, Waited: round(waited.Seconds(), 1), Records: s.Records, Queued: s.Queued}
	if r.Records == nil {
		r.Records = []Record{}
	}
	r.Conclusion = Overall(s.Records)
	return r
}

// Overall sums conclusions: failure if any job failed or errored, then
// running, then cancelled, then success; "" for no jobs.
func Overall(recs []Record) string {
	if len(recs) == 0 {
		return ""
	}
	has := map[string]bool{}
	for _, r := range recs {
		has[r.Conclusion] = true
	}
	switch {
	case has[Failure] || has[Error]:
		return Failure
	case has[Running]:
		return Running
	case has[Cancelled]:
		return Cancelled
	case has[Unknown]:
		return Unknown
	}
	return Success
}
