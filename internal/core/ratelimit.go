package core

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// RateLimitError is a forge call refused because the token's rate limit is
// used up, or not sent because the client already knows it is. It matches
// ErrRateLimited and unwraps to the forge's own answer (Err), so a caller
// that reads the HTTP status still can. A rate limit is never a permission
// problem: the token works again after Until.
type RateLimitError struct {
	Forge string    // github, gitlab or forgejo
	Until time.Time // when calls may resume; zero when the forge didn't say
	Err   error     // the forge's answer, when there was one
}

func (e *RateLimitError) Error() string {
	when := "the forge did not say until when"
	if !e.Until.IsZero() {
		when = "until " + e.Until.UTC().Format("2006-01-02 15:04:05 UTC")
	}
	if e.Err != nil {
		return e.Err.Error() + "; rate limited " + when
	}
	return e.Forge + ": the API rate limit is used up " + when
}

// Is lets errors.Is(err, ErrRateLimited) match.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

func (e *RateLimitError) Unwrap() error { return e.Err }

// RateLimitUnknownWait is how long to wait when a forge refused a call for
// its rate limit without saying until when. GitHub asks for at least a
// minute in that case.
const RateLimitUnknownWait = time.Minute

// RateLimited is what a person or an agent needs to know about a rate limit
// refusal: a message, a fix and how long to wait.
type RateLimited struct {
	Message string        // "GitHub rate limit reached for this token; it resets at 05:18 UTC (in 12 min)"
	Fix     string        // wait, then run it again; a token not shared with other workflows
	Until   time.Time     // when to try again
	Wait    time.Duration // Until less now, at least 1 s
}

// RetryAfterS is Wait in whole seconds, rounded up.
func (r RateLimited) RetryAfterS() int { return int(math.Ceil(r.Wait.Seconds())) }

// AsRateLimited reports whether err is a forge rate limit and, when it is,
// what to tell the user. The fix never says to make a new token: the token
// works, it is only out of requests until the reset. again is what to run
// after the wait, such as "run the same command again".
func AsRateLimited(err error, now time.Time, again string) (RateLimited, bool) {
	if !errors.Is(err, ErrRateLimited) {
		return RateLimited{}, false
	}
	forge := ""
	until := time.Time{}
	var rl *RateLimitError
	if errors.As(err, &rl) {
		forge, until = rl.Forge, rl.Until
	}
	said := !until.IsZero()
	if !said {
		until = now.Add(RateLimitUnknownWait)
	}
	wait := until.Sub(now)
	if wait < time.Second {
		wait = time.Second
		until = now.Add(wait)
	}
	at := clockTime(until, now, wait)
	msg := fmt.Sprintf("%s rate limit reached for this token; it resets at %s (in %s)", forgeTitle(forge), at, humanWait(wait))
	if !said {
		msg = fmt.Sprintf("%s rate limit reached for this token; it did not say when it resets, so wait until %s (%s)", forgeTitle(forge), at, humanWait(wait))
	}
	fix := fmt.Sprintf("the token is fine; wait until %s, then %s. If it keeps happening, the token's requests are shared with other workflows or devices: use a token that is not shared", at, again)
	return RateLimited{Message: msg, Fix: fix, Until: until, Wait: wait}, true
}

// clockTime is until as a UTC clock time: to the second under 5 minutes,
// else to the minute rounded up, so waiting until it is never too early;
// with the date when it is not today.
func clockTime(until, now time.Time, wait time.Duration) string {
	u := until.UTC()
	layout := "15:04:05 UTC"
	if wait >= 5*time.Minute {
		layout = "15:04 UTC"
		if t := u.Truncate(time.Minute); !t.Equal(u) {
			u = t.Add(time.Minute)
		}
	}
	if u.Format("2006-01-02") != now.UTC().Format("2006-01-02") {
		layout = "2006-01-02 " + layout
	}
	return u.Format(layout)
}

func forgeTitle(kind string) string {
	switch kind {
	case "github":
		return "GitHub"
	case "gitlab":
		return "GitLab"
	case "forgejo":
		return "Forgejo"
	}
	return "The forge's"
}

// humanWait is a wait such as "45 s", "12 min" or "1 h 5 min", rounded up.
func humanWait(d time.Duration) string {
	s := int(math.Ceil(d.Seconds()))
	if s < 60 {
		return fmt.Sprintf("%d s", s)
	}
	m := (s + 59) / 60
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	if m%60 == 0 {
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}
