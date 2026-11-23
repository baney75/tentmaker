package dispatch

import (
	"sort"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// WakeAfter is how long a queued job waits with no runner before Waiting
// lists it: long enough for every awake worker (they ask every second) to
// have taken it if it could.
const WakeAfter = 15 * time.Second

// takerFresh: a device that asked this recently is awake and may take a
// job on its next request.
const takerFresh = 30 * time.Second

// WaitingJob is a queued job no awake device has taken: a reason to wake
// a sleeping one (the mesh's Wake-on-LAN).
type WaitingJob struct {
	Key  string `json:"key"`
	Repo string `json:"repo"`
	Name string `json:"name"`
	// Labels are what a runner must carry, as MatchJob reads them: a
	// runner job's own labels, or a check job's platform labels.
	Labels []string `json:"labels"`
	// Since is when it started waiting: a runner job's queue time (or
	// this broker's start, if later), a check job's first sight.
	Since time.Time `json:"since"`
}

// Fits reports whether a worker with base labels and platforms could run
// the job.
func (j WaitingJob) Fits(base []string, platforms []core.Platform) bool {
	_, _, ok := MatchJob(j.Labels, base, platforms)
	return ok
}

// Waiting lists the jobs this broker would grant but nobody takes: each has
// waited at least WakeAfter, has no runner, and no device that asked in the
// last takerFresh with a free slot can run it. Oldest first.
func (b *Broker) Waiting() []WaitingJob {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	var out []WaitingJob
	add := func(j WaitingJob) {
		if now.Sub(j.Since) >= WakeAfter && !b.takerLocked(j, now) {
			out = append(out, j)
		}
	}
	live := map[string]bool{}
	for _, r := range b.repos {
		if r.denied {
			continue
		}
		switch r.mode {
		case "runner":
			if b.repoRefusalLocked(r, now) != "" {
				continue
			}
			var idle []*grantRec
			took := map[int64]bool{}
			for _, g := range b.grants {
				if g.Kind != core.KindRunner || g.Forge != r.forge || g.Repo != r.cfg.Name {
					continue
				}
				if g.TookJob != 0 {
					took[g.TookJob] = true
				}
				if !g.Ended && !g.Settled {
					idle = append(idle, g)
				}
			}
			used := make([]bool, len(idle))
			for _, j := range r.queued {
				if b.jobRefusal(j, now) != "" || took[j.ID] {
					continue
				}
				// A runner granted and not done serves one matching job.
				covered := false
				for i, g := range idle {
					if !used[i] && subset(j.Labels, g.Labels) {
						used[i], covered = true, true
						break
					}
				}
				if covered {
					continue
				}
				since := j.CreatedAt
				if since.Before(b.started) {
					since = b.started
				}
				add(WaitingJob{Key: j.Key(), Repo: r.cfg.Name, Name: j.Name, Labels: append([]string(nil), j.Labels...), Since: since})
			}
		case "checks":
			for _, h := range r.heads {
				for _, cj := range h.jobs {
					ctxName := CheckContext(cj)
					retry := b.retries[checkKey(r.forge, r.cfg.Name, h.commit.SHA, ctxName)]
					if _, ok := h.statuses[ctxName]; ok && (retry == nil || !retry.open) {
						continue
					}
					if b.checkTakenLocked(r, h.commit.SHA, ctxName) {
						continue
					}
					key := checkKey(r.forge, r.cfg.Name, h.commit.SHA, ctxName)
					live[key] = true
					since, ok := b.waitSince[key]
					if !ok {
						since = now
						b.waitSince[key] = now
					}
					add(WaitingJob{Key: key, Repo: r.cfg.Name, Name: cj.Name,
						Labels: PlatformLabels([]string{LabelTentmaker}, cj.Platform), Since: since})
				}
			}
		}
	}
	for k := range b.waitSince {
		if !live[k] {
			delete(b.waitSince, k)
		}
	}
	sort.SliceStable(out, func(i, k int) bool {
		if !out[i].Since.Equal(out[k].Since) {
			return out[i].Since.Before(out[k].Since)
		}
		return out[i].Key < out[k].Key
	})
	return out
}

// takerLocked reports whether an awake device could take j now: it asked
// in the last takerFresh, isn't quarantined, has a free slot and fits.
func (b *Broker) takerLocked(j WaitingJob, now time.Time) bool {
	for _, d := range b.devices {
		// A lent device may not be allowed the job: it never counts, so
		// the owner's sleeping device is still woken for it.
		if d.Offline || d.Quarantine || d.Free < 1 || now.Sub(d.LastSeen) >= takerFresh || d.lend != nil {
			continue
		}
		if j.Fits(d.labels, d.platforms) {
			return true
		}
	}
	return false
}
