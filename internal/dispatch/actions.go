package dispatch

import (
	"slices"

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
	// Hold holds a job to read, or lets it write again.
	Hold(job string, held bool)
}

// actionsRec is a job's Actions cache binding in state.json, so a daemon
// that restarts while the job runs grants it again. It holds no secret: a
// restarted service mints the job a new capability.
type actionsRec struct {
	Ref     string `json:"ref,omitempty"`
	Default string `json:"default,omitempty"`
	Access  string `json:"access"`
	// Confirmed: Ref and Access are narrowed to the job the broker saw the
	// runner take (actionscache.Binding.Confirm).
	Confirmed bool `json:"confirmed,omitempty"`
}

// actionsLocked decides whether runner offer off, about to start on e with
// repo cache c (cacheDecisionLocked's answer), uses the device's Actions
// cache, and how:
//   - write when it got the repo cache, so the same trust rules hold: it is
//     vouched safe for the repo, and only that repo's trusted jobs have
//     run since the last reset;
//   - otherwise none, and the job keeps GitHub's own cache. A drained job
//     gets none too: the worker sends OnlyRepo and declines other trust
//     only while a Cached job runs, so a reader beside no cached job would
//     leave R's entries open to whatever starts next.
//
// Write is not honored until the broker confirms which job the runner
// took (confirmActionsLocked).
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
	if c.IsZero() {
		return nil
	}
	return &actionsRec{
		Ref:     actionscache.RefScope(off.Job.Event, off.Job.HeadBranch),
		Default: actionscache.BranchScope(off.Actions.DefaultBranch),
		Access:  actionscache.Write.String(),
	}
}

// confirmActionsLocked applies source's word on which job each runner took
// to the runner's Actions grant, once: the record is narrowed for good
// (actionscache.Binding.Confirm), saved with the job, and granted again.
func (w *Worker) confirmActionsLocked(source string, cs []core.RunnerJob) {
	if w.o.ActionsCache == nil {
		return
	}
	for _, c := range cs {
		for _, j := range w.st.Jobs {
			if j.Name != c.Runner || j.Source != source || j.Actions == nil || j.Actions.Confirmed {
				continue
			}
			e := w.execByName(j.Executor)
			if e == nil {
				continue
			}
			b, ok := w.actionsBinding(e, j)
			if !ok {
				continue
			}
			b = b.Confirm(c.Event, c.HeadBranch)
			j.Actions.Ref, j.Actions.Access, j.Actions.Confirmed = b.Ref, b.Access.String(), true
			w.holdActionsLocked() // before the confirmed grant lands
			if err := w.o.ActionsCache.Grant(b); err != nil {
				// The old grant stays, and it may not write unconfirmed.
				w.log.Warn("actions cache grant not narrowed", "runner", j.Name, "error", err)
			}
			w.log.Info("actions cache grant confirmed", "runner", j.Name, "ref", b.Ref, "access", b.Access.String())
		}
	}
	w.holdActionsLocked()
}

// holdActionsLocked holds every job to read unless all other jobs on the
// device are confirmed writers of its repo and ref: jobs share the runner
// uid, so any job can pass as another (review second-S1).
func (w *Worker) holdActionsLocked() {
	if w.o.ActionsCache == nil {
		return
	}
	for _, j := range w.st.Jobs {
		if j.Actions == nil {
			continue
		}
		w.o.ActionsCache.Hold(j.Name, slices.ContainsFunc(w.st.Jobs, func(o *jobRec) bool {
			return o != j && !(o.Actions != nil && o.Actions.Confirmed && o.Actions.Access == actionscache.Write.String() &&
				o.Forge == j.Forge && o.Repo == j.Repo && o.Actions.Ref == j.Actions.Ref)
		}))
	}
}

func (w *Worker) actionsBinding(e *execInfo, j *jobRec) (actionscache.Binding, bool) {
	ac, ok := e.ex.(core.ActionsCacher)
	if !ok || j.Actions == nil {
		return actionscache.Binding{}, false
	}
	return actionscache.Binding{
		Job:       j.Name,
		Repo:      core.CacheRepo{Forge: j.Forge, Repo: j.Repo},
		Ref:       j.Actions.Ref,
		Default:   j.Actions.Default,
		Access:    actionscache.ParseAccess(j.Actions.Access),
		Store:     ac,
		Confirmed: j.Actions.Confirmed,
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
	w.holdActionsLocked() // before any grant comes back
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
