package runs

import (
	"errors"
	"net/url"
	"strconv"
	"time"
)

// MaxLimit caps one query's answer so a pool view stays a few MiB.
const MaxLimit = 5000

// Values encodes q as URL query parameters.
func (q Query) Values() url.Values {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		}
	}
	set("id", q.ID)
	set("repo", q.Repo)
	set("sha", q.SHA)
	if q.RunID != 0 {
		v.Set("run_id", strconv.FormatInt(q.RunID, 10))
	}
	if q.Failed {
		v.Set("failed", "1")
	}
	if !q.Since.IsZero() {
		v.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	return v
}

// ParseQuery reads what Values wrote. Limit is capped at MaxLimit.
func ParseQuery(v url.Values) (Query, error) {
	q := Query{ID: v.Get("id"), Repo: v.Get("repo"), SHA: v.Get("sha"), Failed: v.Get("failed") == "1"}
	if len(q.ID) > 128 || len(q.Repo) > 512 || len(q.SHA) > 64 {
		return q, errors.New("a query value is too long")
	}
	if s := v.Get("run_id"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return q, errors.New("run_id must be a number")
		}
		q.RunID = n
	}
	if s := v.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return q, errors.New("since must be an RFC 3339 time")
		}
		q.Since = t
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return q, errors.New("limit must be a number")
		}
		q.Limit = n
	}
	if q.Limit == 0 || q.Limit > MaxLimit {
		q.Limit = MaxLimit
	}
	return q, nil
}

// Answer is one device's (or the pool's) reply to a query.
type Answer struct {
	Records []Record `json:"records"`
	// Queued names jobs a broker knows for the queried repo and sha (or run
	// id) that no device runs yet. Only a query with a repo and a sha or
	// run id gets them.
	Queued []string `json:"queued,omitempty"`
	// Errors names the devices that did not answer, and why.
	Errors []string `json:"errors,omitempty"`
	// HistoryFrom, when not zero, is when the oldest record of a full run
	// history ended (or started): history_max dropped what came before, so
	// an answer can't speak for earlier jobs. A pool answer carries the
	// latest of its devices'.
	HistoryFrom time.Time `json:"history_from,omitzero"`
}

// Later returns the later of two HistoryFrom times; zero means none.
func Later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// LogAnswer is a job's log tail.
type LogAnswer struct {
	ID      string `json:"id"`
	Device  string `json:"device"`
	Running bool   `json:"running"`
	// Size is the whole log's size in bytes, which may be more than Data;
	// a follower asks again and prints what is past what it has.
	Size int64  `json:"size"`
	Data []byte `json:"data"`
	// Source is the record's LogSource, for an ended runner job.
	Source string `json:"source,omitempty"`
}
