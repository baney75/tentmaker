// Package runs is Tentmaker's run history: one record per job, kept on the
// device that ran it, with a redacted tail of the job's log. The pool view
// merges every device's records over the mesh, and stats, the CLI's runs,
// logs and stats commands, the MCP tools and wait_for_run all read it.
package runs

import (
	"sort"
	"strings"
	"time"
)

// Conclusions. A job that ended is one of the first five; a job still on
// a device is Running.
const (
	Success   = "success"
	Failure   = "failure"
	Cancelled = "cancelled"
	Error     = "error"
	Unknown   = "unknown"
	Running   = "running"
)

// Record is one job on one device.
type Record struct {
	// ID is the runner name the device gave the job, unique in the pool.
	ID       string `json:"id"`
	Kind     string `json:"kind"` // core.KindRunner or core.KindCheck
	Forge    string `json:"forge"`
	Repo     string `json:"repo"`
	Workflow string `json:"workflow,omitempty"`
	Job      string `json:"job"`
	JobID    int64  `json:"job_id,omitempty"` // the forge's job ID; 0 for check jobs
	RunID    int64  `json:"run_id,omitempty"`
	RunURL   string `json:"run_url,omitempty"`
	Ref      string `json:"ref,omitempty"`
	SHA      string `json:"sha,omitempty"`
	Device   string `json:"device"`
	Executor string `json:"executor,omitempty"`
	Platform string `json:"platform,omitempty"` // "linux/x64"
	// Slot is which of the device's slots the job took, 1 for the first.
	Slot int `json:"slot,omitempty"`

	QueuedAt  time.Time `json:"queued_at,omitzero"`
	StartedAt time.Time `json:"started_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`

	Conclusion string `json:"conclusion"`
	// FailedStep is the step that failed, when the log says.
	FailedStep string `json:"failed_step,omitempty"`
	// Note says why Tentmaker ended the job, when it did.
	Note string `json:"note,omitempty"`

	// PeakMemoryBytes and CPUSeconds come from the job's cgroup on Linux
	// and are 0 where the sandbox can't say.
	PeakMemoryBytes int64   `json:"peak_memory_bytes,omitempty"`
	CPUSeconds      float64 `json:"cpu_seconds,omitempty"`
	// CacheBytes is the size of the repo cache the job started with, when
	// it had one.
	CacheBytes int64 `json:"cache_bytes,omitempty"`
	// HasLog says the device keeps a log tail for the job.
	HasLog bool `json:"has_log,omitempty"`
}

// QueueWait is how long the job waited between the forge queueing it and
// a device starting it; 0 when either time is unknown.
func (r Record) QueueWait() time.Duration {
	if r.QueuedAt.IsZero() || r.StartedAt.IsZero() || r.StartedAt.Before(r.QueuedAt) {
		return 0
	}
	return r.StartedAt.Sub(r.QueuedAt)
}

// Duration is how long the job ran. A running job's duration runs up to
// now.
func (r Record) Duration(now time.Time) time.Duration {
	if r.StartedAt.IsZero() {
		return 0
	}
	end := r.EndedAt
	if end.IsZero() {
		end = now
	}
	if end.Before(r.StartedAt) {
		return 0
	}
	return end.Sub(r.StartedAt)
}

// Failed reports whether the job ended in failure or error.
func (r Record) Failed() bool { return r.Conclusion == Failure || r.Conclusion == Error }

// Ended reports whether the job is over.
func (r Record) Ended() bool { return r.Conclusion != Running && r.Conclusion != "" }

// at is the time a record sorts by: when it ended, else when it started.
func (r Record) at() time.Time {
	if !r.EndedAt.IsZero() {
		return r.EndedAt
	}
	return r.StartedAt
}

// Query picks records. Zero fields match everything.
type Query struct {
	ID     string    `json:"id,omitempty"`
	Repo   string    `json:"repo,omitempty"` // owner/name, case-insensitive
	SHA    string    `json:"sha,omitempty"`  // a full sha, or a prefix of 7 or more
	RunID  int64     `json:"run_id,omitempty"`
	Failed bool      `json:"failed,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	Limit  int       `json:"limit,omitempty"` // 0 = no limit
}

// Match reports whether r fits q (Limit aside).
func (q Query) Match(r Record) bool {
	if q.ID != "" && r.ID != q.ID {
		return false
	}
	if q.Repo != "" && !strings.EqualFold(r.Repo, q.Repo) {
		return false
	}
	if q.SHA != "" && !SameSHA(r.SHA, q.SHA) {
		return false
	}
	if q.RunID != 0 && r.RunID != q.RunID {
		return false
	}
	if q.Failed && !r.Failed() {
		return false
	}
	if !q.Since.IsZero() && r.at().Before(q.Since) {
		return false
	}
	return true
}

// SameSHA reports whether two commit ids name the same commit: equal, or
// one a prefix (7 or more characters) of the other.
func SameSHA(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == "" || b == "" {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 7 && strings.HasPrefix(b, a)
}

// Apply filters recs by q, newest first, and cuts to q.Limit.
func Apply(recs []Record, q Query) []Record {
	out := make([]Record, 0, len(recs))
	for _, r := range recs {
		if q.Match(r) {
			out = append(out, r)
		}
	}
	SortNewest(out)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

// SortNewest sorts running jobs first, then by end (or start) time, newest
// first; ties by device and id, so the order is stable.
func SortNewest(recs []Record) {
	sort.SliceStable(recs, func(i, k int) bool {
		a, b := recs[i], recs[k]
		if ar, br := a.Conclusion == Running, b.Conclusion == Running; ar != br {
			return ar
		}
		if !a.at().Equal(b.at()) {
			return a.at().After(b.at())
		}
		if a.Device != b.Device {
			return a.Device < b.Device
		}
		return a.ID < b.ID
	})
}

// Merge joins the record lists of several devices into one, newest first.
// A record two lists both carry (the same device and id) is kept once: the
// ended copy wins over a running one, else the first seen.
func Merge(lists ...[]Record) []Record {
	type key struct{ device, id string }
	seen := map[key]int{}
	var out []Record
	for _, l := range lists {
		for _, r := range l {
			k := key{r.Device, r.ID}
			if i, ok := seen[k]; ok {
				if !out[i].Ended() && r.Ended() {
					out[i] = r
				}
				continue
			}
			seen[k] = len(out)
			out = append(out, r)
		}
	}
	SortNewest(out)
	return out
}
