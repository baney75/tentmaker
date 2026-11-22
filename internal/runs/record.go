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
	// LogSource says where a runner job's conclusion and log came from:
	// LogForge (the forge's own job log and step results) or LogRunnerDiag
	// (the runner's _diag trace logs, when the forge could not be read).
	LogSource string `json:"log_source,omitempty"`
	// Tests counts the job's test results (JUnit reports in its workspace,
	// go test -json lines in its log), nil when it reported none. The
	// device keeps the cases themselves (Store.Tests).
	Tests *TestSummary `json:"tests,omitempty"`
	// KeptUntil is when a failed job's sandbox, kept for debugging, is
	// cleaned up; zero when it was not kept.
	KeptUntil time.Time `json:"kept_until,omitzero"`
}

// Log sources.
const (
	LogForge      = "forge"
	LogRunnerDiag = "runner-diag"
)

// ForgeGrace is how long after a runner job ends with an unknown
// conclusion the pool waits for the broker's forge result.
const ForgeGrace = 2 * time.Minute

// awaitingForge reports whether r ended without a conclusion and the
// broker's forge result may still come.
func (r Record) awaitingForge(now time.Time) bool {
	return r.Kind == "runner" && r.Conclusion == Unknown && r.LogSource != LogForge &&
		!r.EndedAt.IsZero() && now.Sub(r.EndedAt) < ForgeGrace
}

// ForgeReport is a broker's forge result for one runner job.
type ForgeReport struct {
	ID, Device, Forge, Repo, Job string
	JobID, RunID                 int64
	RunURL                       string
	SHA, Workflow                string
	StartedAt, EndedAt           time.Time
	Conclusion, FailedStep       string
	Log                          []byte // raw; nil when it could not be read
}

// ApplyForge puts the forge's conclusion and failed step on r. A job
// Tentmaker ended keeps its note.
func (r *Record) ApplyForge(rep ForgeReport) {
	if c := ForgeConclusion(rep.Conclusion); c != Unknown || r.Conclusion == "" {
		r.Conclusion = c
	}
	if rep.FailedStep != "" || r.Conclusion == Success {
		r.FailedStep = rep.FailedStep
	}
	if rep.Log != nil {
		r.LogSource = LogForge
	}
	if r.SHA == "" {
		r.SHA = rep.SHA
	}
	if r.Workflow == "" {
		r.Workflow = rep.Workflow
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = rep.StartedAt
	}
}

// Record is the record a broker keeps for a peer's job it has a forge
// result for; the pool merges it into the peer's own record. It is marked
// LogForge even when the log could not be read (HasLog then stays false).
func (rep ForgeReport) Record() Record {
	r := Record{ID: rep.ID, Kind: "runner", Forge: rep.Forge, Repo: rep.Repo, Job: rep.Job, JobID: rep.JobID,
		RunID: rep.RunID, RunURL: rep.RunURL, Device: rep.Device, EndedAt: rep.EndedAt}
	r.ApplyForge(rep)
	r.SHA, r.Workflow, r.StartedAt = rep.SHA, rep.Workflow, rep.StartedAt
	r.LogSource = LogForge
	return r
}

// ForgeConclusion maps a forge job conclusion to a run conclusion.
func ForgeConclusion(s string) string {
	switch s {
	case "success", "neutral", "skipped":
		return Success
	case "failure", "timed_out", "startup_failure":
		return Failure
	case "cancelled":
		return Cancelled
	}
	return Unknown
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

// withForge is dev's record with the conclusion a broker read from the
// forge.
func withForge(dev, forge Record) Record {
	dev.Conclusion, dev.FailedStep = forge.Conclusion, forge.FailedStep
	if dev.SHA == "" {
		dev.SHA = forge.SHA
	}
	if dev.Workflow == "" {
		dev.Workflow = forge.Workflow
	}
	if dev.StartedAt.IsZero() {
		dev.StartedAt = forge.StartedAt
	}
	if forge.HasLog {
		dev.LogSource = LogForge
	}
	if dev.Tests == nil {
		dev.Tests = forge.Tests
	}
	return dev
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
// ended copy wins over a running one; an ended device copy takes the
// conclusion of a broker's forge copy (LogSource LogForge); else the first
// seen wins.
func Merge(lists ...[]Record) []Record {
	type key struct{ device, id string }
	seen := map[key]int{}
	var out []Record
	for _, l := range lists {
		for _, r := range l {
			k := key{r.Device, r.ID}
			if i, ok := seen[k]; ok {
				switch {
				case !out[i].Ended() && r.Ended():
					out[i] = r
				case r.Ended() && r.LogSource == LogForge && out[i].LogSource != LogForge:
					out[i] = withForge(out[i], r)
				case out[i].LogSource == LogForge && r.LogSource != LogForge && r.Ended():
					out[i] = withForge(r, out[i])
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
