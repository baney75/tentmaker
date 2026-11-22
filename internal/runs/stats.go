package runs

import (
	"math"
	"sort"
	"strings"
	"time"
)

// HostedRate is GitHub's per-minute price for its standard hosted runner of
// one OS, in US dollars.
//
// Source: GitHub's "Actions runner pricing" reference
// (https://docs.github.com/en/billing/reference/actions-runner-pricing) and
// the 2026 pricing change (https://resources.github.com/actions/2026-pricing-changes-for-github-actions/),
// effective 2026-01-01: Linux 2-core $0.006, Windows 2-core $0.010, macOS
// 3-core or 4-core $0.062. GitHub bills each job rounded up to the whole
// minute. Update this table when GitHub changes its prices.
var HostedRate = map[string]float64{
	"linux":   0.006,
	"windows": 0.010,
	"macos":   0.062,
}

// RatesAsOf is when HostedRate was last checked against GitHub's prices.
const RatesAsOf = "2026-01-01"

// RepoStats is one repo's numbers over a window.
type RepoStats struct {
	Repo        string  `json:"repo"`
	Jobs        int     `json:"jobs"`
	Failed      int     `json:"failed"`
	FailureRate float64 `json:"failure_rate"` // failed / jobs, 0 to 1
	QueueP50S   float64 `json:"queue_wait_p50_s"`
	QueueP95S   float64 `json:"queue_wait_p95_s"`
	DurationP50 float64 `json:"duration_p50_s"`
	DurationP95 float64 `json:"duration_p95_s"`
	Minutes     float64 `json:"minutes"` // job minutes served, unrounded
	// BilledMinutes rounds each job up to a whole minute, as GitHub bills.
	BilledMinutes int `json:"billed_minutes"`
	// EstimatedSavingUSD is what BilledMinutes would cost on GitHub-hosted
	// runners at HostedRate. It is an estimate: hosted runners differ in
	// size and speed, and public repos run free there.
	EstimatedSavingUSD float64 `json:"estimated_saving_usd"`
}

// Report is stats for every repo plus the total.
type Report struct {
	Since    time.Time   `json:"since"`
	Until    time.Time   `json:"until"`
	Repos    []RepoStats `json:"repos"`
	Total    RepoStats   `json:"total"`
	Estimate string      `json:"estimate"`
}

// Stats sums the records that ended in [since, now]. Running jobs are left
// out. Repos are sorted by job count, most first.
func Stats(recs []Record, since, now time.Time) Report {
	by := map[string][]Record{}
	var all []Record
	for _, r := range recs {
		if !r.Ended() || r.EndedAt.IsZero() || r.EndedAt.Before(since) || r.EndedAt.After(now) {
			continue
		}
		key := strings.ToLower(r.Repo)
		by[key] = append(by[key], r)
		all = append(all, r)
	}
	rep := Report{Since: since, Until: now,
		Estimate: "estimated saving: billed minutes at GitHub-hosted per-minute rates as of " + RatesAsOf +
			" (Linux $0.006, Windows $0.010, macOS $0.062); an estimate, not a bill"}
	for _, rs := range by {
		rep.Repos = append(rep.Repos, sum(rs[0].Repo, rs))
	}
	sort.Slice(rep.Repos, func(i, k int) bool {
		if rep.Repos[i].Jobs != rep.Repos[k].Jobs {
			return rep.Repos[i].Jobs > rep.Repos[k].Jobs
		}
		return rep.Repos[i].Repo < rep.Repos[k].Repo
	})
	if rep.Repos == nil {
		rep.Repos = []RepoStats{}
	}
	rep.Total = sum("total", all)
	return rep
}

func sum(repo string, rs []Record) RepoStats {
	s := RepoStats{Repo: repo, Jobs: len(rs)}
	var waits, durs []float64
	for _, r := range rs {
		if r.Failed() {
			s.Failed++
		}
		if w := r.QueueWait(); w > 0 || !r.QueuedAt.IsZero() {
			waits = append(waits, w.Seconds())
		}
		d := r.Duration(r.EndedAt)
		durs = append(durs, d.Seconds())
		s.Minutes += d.Minutes()
		billed := int(math.Ceil(d.Minutes()))
		if billed < 1 {
			billed = 1
		}
		s.BilledMinutes += billed
		s.EstimatedSavingUSD += float64(billed) * rateFor(r.Platform)
	}
	if s.Jobs > 0 {
		s.FailureRate = round(float64(s.Failed)/float64(s.Jobs), 4)
	}
	s.QueueP50S, s.QueueP95S = round(Percentile(waits, 50), 1), round(Percentile(waits, 95), 1)
	s.DurationP50, s.DurationP95 = round(Percentile(durs, 50), 1), round(Percentile(durs, 95), 1)
	s.Minutes = round(s.Minutes, 1)
	s.EstimatedSavingUSD = round(s.EstimatedSavingUSD, 2)
	return s
}

// rateFor picks the hosted rate for a "os/arch" platform; an unknown OS
// counts as Linux, the cheapest.
func rateFor(platform string) float64 {
	osName, _, _ := strings.Cut(strings.ToLower(platform), "/")
	if r, ok := HostedRate[osName]; ok {
		return r
	}
	return HostedRate["linux"]
}

// Percentile is the p-th percentile (0 to 100) of xs by the nearest-rank
// method: the smallest value with at least p% of the values at or below
// it. An empty list is 0.
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}

func round(v float64, places int) float64 {
	m := math.Pow(10, float64(places))
	return math.Round(v*m) / m
}

// ParseSince reads a window like "7d", "24h" or "90m" (days are 24 h).
func ParseSince(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	mult := time.Duration(1)
	if n, ok := strings.CutSuffix(s, "d"); ok {
		s, mult = n+"h", 24
	}
	d, err := time.ParseDuration(s)
	// Ten years is more history than any store keeps, and keeps d*24 from
	// overflowing.
	if err != nil || d <= 0 || d > maxSince/mult {
		return 0, false
	}
	return d * mult, true
}

const maxSince = 3650 * 24 * time.Hour
