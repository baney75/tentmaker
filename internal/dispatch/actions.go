package dispatch

import (
	"github.com/baney75/tentmaker/internal/actionscache"
	"github.com/baney75/tentmaker/internal/core"
)

// ActionsCache is the device's GitHub Actions cache service
// (actionscache.Service). The worker grants each runner job that may use
// it before the job starts and revokes the grant when the job ends.
type ActionsCache interface {
	// URL is the base URL jobs reach it at, "" while it isn't listening.
	URL() string
	Grant(b actionscache.Binding) error
	Revoke(job string)
	Limit(job string, a actionscache.Access)
}

// actionsRec is a job's Actions cache binding in state.json, so a daemon
// that restarts while the job runs grants it again. It holds no secret: a
// restarted service mints the job a new capability.
type actionsRec struct {
	Ref     string `json:"ref,omitempty"`
	Default string `json:"default,omitempty"`
	Access  string `json:"access"`
}

// actionsLocked decides whether runner offer off, about to start on e with
// repo cache c (cacheDecisionLocked's answer), uses the device's Actions
// cache, and how:
//   - write when it got the repo cache, so the same trust rules hold: it is
//     vouched safe for the repo, and only that repo's trusted jobs have
//     run since the last reset;
//   - read when it would have got the cache but for a drain;
//   - otherwise none, and the job keeps GitHub's own cache.
//
// The caller adds the job record afterward.
func (w *Worker) actionsLocked(e *execInfo, off core.WorkOffer, kind string, c core.CacheRepo) *actionsRec {
	svc := w.o.ActionsCache
	if svc == nil || kind != core.KindRunner || off.Actions == nil || svc.URL() == "" {
		return nil
	}
	ac, ok := e.ex.(core.ActionsCacher)
	if !ok || !ac.ActionsCacheReady() {
		return nil
	}
	access := actionscache.None
	switch {
	case !c.IsZero():
		access = actionscache.Write
	case w.mayReadCacheLocked(e, off):
		access = actionscache.Read
	}
	if access == actionscache.None {
		return nil
	}
	return &actionsRec{
		Ref:     actionscache.RefScope(off.Job.Event, off.Job.HeadBranch),
		Default: actionscache.BranchScope(off.Actions.DefaultBranch),
		Access:  access.String(),
	}
}

// mayReadCacheLocked is cacheDecisionLocked's test without the drain: may
// off's job see its repo's cache at all.
func (w *Worker) mayReadCacheLocked(e *execInfo, off core.WorkOffer) bool {
	tk := trustKey(offerCache(off))
	rc, caches := e.ex.(core.RepoCacher)
	if tk == "" || !caches || !rc.CachesRepos() {
		return false
	}
	if w.st.Dirty && w.st.DirtyTrust != tk {
		return false
	}
	for _, j := range w.st.Jobs {
		if j.Trust != tk {
			return false
		}
	}
	return true
}

func (w *Worker) actionsBinding(e *execInfo, j *jobRec) (actionscache.Binding, bool) {
	ac, ok := e.ex.(core.ActionsCacher)
	if !ok || j.Actions == nil {
		return actionscache.Binding{}, false
	}
	return actionscache.Binding{
		Job:     j.Name,
		Repo:    core.CacheRepo{Forge: j.Forge, Repo: j.Repo},
		Ref:     j.Actions.Ref,
		Default: j.Actions.Default,
		Access:  actionscache.ParseAccess(j.Actions.Access),
		Store:   ac,
	}, true
}

// limitActionsLocked takes the cache from every running job whose trust
// differs from tk, the trust of a job that is starting: a job can read
// its neighbors' memory, so the cache is only as private as the least
// trusted job on the device. The jobs keep running and simply stop
// restoring and saving.
func (w *Worker) limitActionsLocked(tk string) {
	if w.o.ActionsCache == nil {
		return
	}
	for _, j := range w.st.Jobs {
		if j.Actions != nil && j.Trust != tk && j.Actions.Access != actionscache.None.String() {
			w.o.ActionsCache.Limit(j.Name, actionscache.None)
			j.Actions.Access = actionscache.None.String()
			w.log.Info("actions cache closed for a job: another repo's job started beside it", "runner", j.Name, "repo", j.Repo)
		}
	}
}

// revokeActions forgets a job's grant (a no-op for a job without one).
func (w *Worker) revokeActions(name string) {
	if w.o.ActionsCache != nil {
		w.o.ActionsCache.Revoke(name)
	}
}

// regrantActions grants e's recorded jobs again after a restart.
func (w *Worker) regrantActions(e *execInfo) {
	if w.o.ActionsCache == nil {
		return
	}
	w.mu.Lock()
	var bs []actionscache.Binding
	for _, j := range w.st.Jobs {
		if j.Executor != e.name || j.Actions == nil {
			continue
		}
		if b, ok := w.actionsBinding(e, j); ok {
			bs = append(bs, b)
		}
	}
	w.mu.Unlock()
	for _, b := range bs {
		if err := w.o.ActionsCache.Grant(b); err != nil {
			w.log.Warn("actions cache grant not restored", "runner", b.Job, "error", err)
		}
	}
}
