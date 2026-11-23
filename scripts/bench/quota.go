package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/baney75/tentmaker/internal/forge/github"
)

// QuotaNeed is about what one bench run's five devices bill the API: a
// bench run in issue #39 used about 2000 requests. Some spare is on top.
const QuotaNeed = 2500

// quotaMaxWait bounds one wait: GitHub's core limit resets every hour.
const quotaMaxWait = 61 * time.Minute

// rateCore is the core part of GitHub's GET /rate_limit.
type rateCore struct {
	Limit     int   `json:"limit"`
	Remaining int   `json:"remaining"`
	Reset     int64 `json:"reset"`
}

// QuotaWait is how long the bench should wait before it starts so that its
// devices don't pace their polls (issue #39). The repo's GITHUB_TOKEN limit
// is shared by every workflow run in the repo: a device whose remaining
// limit is under github.CheapFloor polls at most every PollDelay, up to a
// minute, and that wait counts as job start latency. So the bench starts
// only with CheapFloor plus need left, or else waits for the reset (a few
// seconds past it, as GitHub's clock and ours differ). When even a fresh
// hour can't cover that, the reset is the best it can do.
func QuotaWait(c rateCore, now time.Time, need int) time.Duration {
	if c.Limit <= 0 {
		return 0 // no reading: don't hold the bench up
	}
	want := github.CheapFloor(c.Limit) + need
	if want > c.Limit {
		want = c.Limit
	}
	if c.Remaining >= want {
		return 0
	}
	d := time.Unix(c.Reset, 0).Add(5 * time.Second).Sub(now)
	switch {
	case d < 0:
		return 0
	case d > quotaMaxWait:
		return quotaMaxWait
	}
	return d.Round(time.Second)
}

// runQuota reads GET /rate_limit's JSON on stdin and prints the seconds to
// wait, then a line on why.
func runQuota(args []string, stdin io.Reader, stdout, stderr io.Writer, now time.Time) int {
	fs := flag.NewFlagSet("bench quota", flag.ContinueOnError)
	fs.SetOutput(stderr)
	need := fs.Int("need", QuotaNeed, "requests the bench needs on top of the pacing floor")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var body struct {
		Resources struct {
			Core rateCore `json:"core"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(stdin).Decode(&body); err != nil {
		fmt.Fprintln(stderr, "bench quota: reading /rate_limit:", err)
		return 1
	}
	c := body.Resources.Core
	d := QuotaWait(c, now, *need)
	fmt.Fprintf(stdout, "%d\n", int(math.Ceil(d.Seconds())))
	fmt.Fprintf(stderr, "bench quota: %d of %d left, want %d (pacing floor %d + %d), reset at %s: wait %v\n",
		c.Remaining, c.Limit, min(github.CheapFloor(c.Limit)+*need, c.Limit), github.CheapFloor(c.Limit), *need,
		time.Unix(c.Reset, 0).UTC().Format(time.RFC3339), d)
	return 0
}
