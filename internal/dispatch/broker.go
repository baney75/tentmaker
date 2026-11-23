package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// BrokerOptions configures a Broker.
type BrokerOptions struct {
	Config config.Config
	// Forges serve runner-mode repos, by forge name ("github").
	Forges map[string]core.Forge
	// StatusForges serve checks-mode repos, by forge name.
	StatusForges map[string]core.StatusForge
	// Planner turns one workflow file into check jobs (checks.Plan without
	// the skipped list). Checks-mode repos need it.
	Planner func(workflowPath string, src []byte, forge string) ([]core.CheckJob, error)
	// Claimer coordinates claims with other brokers (nil = none).
	Claimer core.Claimer
	// HTTP fetches source archives for the relay. Nil: a client with dial,
	// TLS and header timeouts; ArchiveTimeout bounds each fetch.
	HTTP     *http.Client
	StateDir string
	Log      *slog.Logger
	Now      func() time.Time
	// OnWork, if set, is called (with no broker lock held) after a poll
	// finds work a worker could take; the daemon kicks its worker so the
	// grant doesn't wait for the worker's next tick.
	OnWork func()
	// Reports takes the forge's result for each runner job that ended
	// (history.go); nil reads none.
	Reports ReportRecorder
}

// Broker is the forge side of dispatch. It implements core.WorkSource.
type Broker struct {
	o         BrokerOptions
	log       *slog.Logger
	now       func() time.Time
	name      string
	idleTTL   time.Duration
	statePath string

	tickMu sync.Mutex // one Tick at a time
	saveMu sync.Mutex // one state write at a time

	mu        sync.Mutex // everything below
	repos     []*repoState
	infos     map[string]*infoCache
	grants    map[string]*grantRec
	devices   map[string]*deviceRec
	posts     []*statusPost
	plans     map[string][]core.CheckJob
	archives  map[string]*archiveRec // archive ref -> active check; memory only
	noted     map[string]string      // job or commit key -> refusal already logged
	errs      errorLog
	closed    bool
	lastSaved []byte

	// retries counts check grants that expired before their worker ran
	// them, by checkKey; memory only.
	retries map[string]*checkRetry
	// reports are ended runner jobs whose forge result is still to read;
	// memory only.
	reports []*reportReq
	// started is when this broker was built: a runner job queued before it
	// waits from here (Waiting).
	started time.Time
	// waitSince is when each waiting check job was first seen, by
	// checkKey; memory only.
	waitSince map[string]time.Time
}

type repoState struct {
	cfg      config.Repo
	forge    string
	mode     string
	kind     string // forge kind from config ("github", "forgejo", "gitlab")
	denied   bool
	interval time.Duration
	next     time.Time
	lastPoll time.Time
	lastErr  string
	queued   []core.Job   // runner mode: the last poll, oldest first
	heads    []*checkHead // checks mode: commits to test
	// active is when the repo last had something new or waiting (pollHot).
	active time.Time
	// seen is the last poll's head SHAs or queued job IDs, to tell a
	// change.
	seen string
}

func (r *repoState) key() string { return r.forge + ":" + r.cfg.Name }

type checkHead struct {
	commit   core.Commit
	jobs     []core.CheckJob
	statuses map[string]string
	read     time.Time // when statuses were read from the forge
}

type infoCache struct {
	at  time.Time
	err string
	why string // refusal, "" = allowed
	// noForks: a private repo whose fork PR workflows are off, so no fork
	// job can reach a runner registered to it (see cacheSafe).
	noForks bool
	// defaultBranch is the repo's, for the Actions cache (actionsOffer).
	defaultBranch string
}

// grantRec is one runner the broker granted. It holds no secret.
type grantRec struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Device string `json:"device"`
	// Node is the tailnet node that asked for the grant (core.CallerNode),
	// "" for this broker's own device. Only that node speaks for Device
	// while any of its grants is on record.
	Node       string        `json:"node,omitempty"`
	Forge      string        `json:"forge"`
	Repo       string        `json:"repo"`
	RunnerID   int64         `json:"runner_id,omitempty"`
	Job        core.Job      `json:"job"` // what it was granted for
	Platform   core.Platform `json:"platform"`
	Labels     []string      `json:"labels,omitempty"`
	Granted    time.Time     `json:"granted"`
	Started    time.Time     `json:"started,omitempty"` // first reported running
	Busy       bool          `json:"busy,omitempty"`
	BusySince  time.Time     `json:"busy_since,omitempty"`
	TookJob    int64         `json:"took_job,omitempty"`
	TookRun    int64         `json:"took_run,omitempty"`
	Identified bool          `json:"identified,omitempty"`
	Fork       bool          `json:"fork,omitempty"`
	// TookEvent and TookBranch are the taken job's run event and head
	// branch, sent to the worker as WorkResponse.Confirm.
	TookEvent  string `json:"took_event,omitempty"`
	TookBranch string `json:"took_branch,omitempty"`
	// Settled means nothing more needs checking at the forge: the runner was
	// identified as a same-repo job, or the broker deleted or stopped it.
	Settled  bool      `json:"settled,omitempty"`
	Ended    bool      `json:"ended,omitempty"`
	EndedAt  time.Time `json:"ended_at,omitempty"`
	ErrSince time.Time `json:"err_since,omitempty"`
	// Checks mode.
	SHA     string `json:"sha,omitempty"`
	Ref     string `json:"ref,omitempty"`
	PR      int    `json:"pr,omitempty"`
	Context string `json:"context,omitempty"`

	pending bool      // the grant call is in flight
	checked time.Time // last forge check
	drain   bool      // offer it with Drain: another repo waits for its device
}

type deviceRec struct {
	Name string `json:"name"`
	// LastSeen is not saved: a restarted broker starts every device's
	// silence clock at load, and saving it would rewrite the file on every
	// request.
	LastSeen       time.Time `json:"-"`
	Offline        bool      `json:"offline,omitempty"`
	Quarantine     bool      `json:"quarantine,omitempty"`
	QuarantineWhy  string    `json:"quarantine_why,omitempty"`
	QuarantineSent bool      `json:"quarantine_sent,omitempty"`
	Stop           []string  `json:"stop,omitempty"`
	Free           int       `json:"-"`
	// labels and platforms are what its last request asked for (Waiting).
	labels    []string
	platforms []core.Platform
}

type statusPost struct {
	Forge       string    `json:"forge"`
	Repo        string    `json:"repo"`
	SHA         string    `json:"sha"`
	Context     string    `json:"context"`
	State       string    `json:"state"`
	Description string    `json:"description"`
	Queued      time.Time `json:"queued"`
	Attempts    int       `json:"attempts,omitempty"`
	inflight    bool
}

type brokerFile struct {
	Version int           `json:"version"`
	Grants  []*grantRec   `json:"grants"`
	Devices []*deviceRec  `json:"devices"`
	Posts   []*statusPost `json:"posts"`
}

// NewBroker builds a broker and loads its saved state. It makes no network
// calls.
func NewBroker(o BrokerOptions) *Broker {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	name := o.Config.Device
	if name == "" {
		name = "local"
	}
	b := &Broker{
		o:         o,
		log:       o.Log.With("component", "broker"),
		now:       o.Now,
		name:      name,
		idleTTL:   o.Config.Job.IdleTTL.D(),
		infos:     map[string]*infoCache{},
		grants:    map[string]*grantRec{},
		devices:   map[string]*deviceRec{},
		plans:     map[string][]core.CheckJob{},
		archives:  map[string]*archiveRec{},
		noted:     map[string]string{},
		retries:   map[string]*checkRetry{},
		waitSince: map[string]time.Time{},
	}
	b.started = b.now()
	if b.idleTTL <= 0 {
		b.idleTTL = 90 * time.Second
	}
	if o.StateDir != "" {
		b.statePath = filepath.Join(o.StateDir, "broker.json")
	}
	now := b.now()
	for _, rc := range o.Config.Repos {
		r := &repoState{cfg: rc, forge: rc.Forge, mode: rc.Mode, interval: pollMin, next: now}
		if r.forge == "" {
			r.forge = "github"
		}
		if r.mode == "" {
			r.mode = "runner"
		}
		r.kind = o.Config.Forges[r.forge].Kind
		if r.kind == "" {
			r.kind = r.forge
		}
		r.denied = Denied(rc.Name, o.Config.DenyRepos)
		if r.denied {
			b.log.Info("repo denied by deny_repo_patterns; never polled", "repo", rc.Name)
		}
		b.repos = append(b.repos, r)
	}
	b.load(now)
	return b
}

// Name implements core.WorkSource.
func (b *Broker) Name() string { return b.name }

// ---------------------------------------------------------------- Work

// Work implements core.WorkSource. It takes the worker's report (running,
// ended, results), answers with runners to stop and a quarantine flag, and
// grants up to req.Free runners.
func (b *Broker) Work(ctx context.Context, req core.WorkRequest) (core.WorkResponse, error) {
	if req.Device == "" {
		return core.WorkResponse{}, errors.New("work request without a device name")
	}
	node := core.CallerNode(ctx)
	now := b.now()
	var resp core.WorkResponse
	var picks []*grantRec
	var logs []checkLog

	b.mu.Lock()
	// A device name is only a claim: while another tailnet node holds any
	// of this device's grants, this request changes nothing, not even the
	// device's silence clock, so the holder's grants end on schedule if it
	// is gone (review r2 partS sol-S2-F2).
	for _, rec := range b.grants {
		if rec.Device == req.Device && rec.Node != node {
			b.mu.Unlock()
			return core.WorkResponse{}, core.ErrOtherNode
		}
	}
	dev := b.device(req.Device)
	if dev.Offline {
		b.log.Info("device back online", "device", req.Device)
	}
	dev.LastSeen, dev.Offline, dev.Free = now, false, req.Free
	dev.labels, dev.platforms = append([]string(nil), req.Labels...), append([]core.Platform(nil), req.Platforms...)

	// Results first, so an ended check job with a result posts that result.
	for _, r := range req.Results {
		rec := b.grants[r.Runner]
		if rec == nil || rec.Device != req.Device || rec.Kind != core.KindCheck {
			b.log.Warn("result for a check job this broker did not grant; ignored", "device", req.Device, "runner", r.Runner)
			continue
		}
		state := r.State
		switch state {
		case "success", "failure", "error":
		default:
			state = "error"
		}
		desc := r.Description
		if desc == "" {
			desc = state
		}
		b.queuePost(rec, state, desc, now)
		b.log.Info("check finished", "runner", rec.Name, "device", rec.Device, "repo", rec.Repo,
			"sha", rec.SHA, "context", rec.Context, "state", state, "duration", r.Duration)
		logs = append(logs, newCheckLog(rec, state, desc, r.LogTail))
		delete(b.grants, rec.Name)
	}

	running := map[string]bool{}
	for _, name := range req.Running {
		running[name] = true
		rec := b.grants[name]
		if rec == nil || rec.Device != req.Device {
			// A runner this broker can't vouch for: it can't watch it for
			// fork jobs, so it must stop (fail closed).
			b.log.Warn("worker runs a runner this broker does not know; stopping it", "device", req.Device, "runner", name)
			resp.Stop = append(resp.Stop, name)
			continue
		}
		if rec.Started.IsZero() {
			rec.Started = now
		}
	}

	ended := map[string]bool{}
	var expired []*grantRec
	for _, name := range req.Ended {
		ended[name] = true
		rec := b.grants[name]
		if rec == nil || rec.Device != req.Device {
			continue
		}
		// A check its worker ended before ever reporting it running was
		// declined or failed to start: offer it again, as an expired grant.
		if rec.Kind == core.KindCheck && !rec.pending && rec.Started.IsZero() {
			b.expireCheckLocked(rec, now, "did not start on "+rec.Device)
			expired = append(expired, rec)
			continue
		}
		b.endLocked(rec, now)
	}
	// A runner the worker reported running before and now neither runs nor
	// reported ended was lost by the worker: treat it as ended.
	for _, rec := range b.grants {
		if rec.Device == req.Device && !rec.pending && !rec.Ended && !rec.Started.IsZero() && !running[rec.Name] && !ended[rec.Name] {
			b.log.Warn("worker no longer reports a runner; treating it as ended", "device", req.Device, "runner", rec.Name)
			b.endLocked(rec, now)
		}
	}

	// A check grant the worker never reported running within the start
	// window never reached it (the response was lost) or never started.
	// Nothing else ends it: monitor skips checks, and only started grants
	// count as ended when omitted.
	for _, rec := range b.grants {
		if rec.Device == req.Device && rec.Kind == core.KindCheck && !rec.pending && rec.Started.IsZero() &&
			!running[rec.Name] && !ended[rec.Name] && now.Sub(rec.Granted) >= startGrace {
			b.expireCheckLocked(rec, now, "never started on "+rec.Device)
			expired = append(expired, rec)
		}
	}

	// Stops: deliver while the worker still runs them.
	var keep []string
	for _, name := range dev.Stop {
		if running[name] {
			resp.Stop = append(resp.Stop, name)
			keep = append(keep, name)
		}
	}
	dev.Stop = keep
	resp.Stop = dedupe(resp.Stop)

	// Confirmations: the job each running runner took, every response
	// while it runs, so a lost response costs nothing.
	for _, rec := range b.grants {
		if rec.Device == req.Device && rec.Kind == core.KindRunner && rec.Identified && running[rec.Name] {
			resp.Confirm = append(resp.Confirm, core.RunnerJob{Runner: rec.Name, Event: rec.TookEvent, HeadBranch: rec.TookBranch})
		}
	}
	sort.Slice(resp.Confirm, func(i, k int) bool { return resp.Confirm[i].Runner < resp.Confirm[k].Runner })

	if dev.Quarantine {
		// The contract has no "I reset" field: a worker that got the
		// quarantine asks for nothing until its reset succeeded, so a request
		// for work with nothing of ours running means it has reset.
		if dev.QuarantineSent && req.Free > 0 && len(req.Running) == 0 {
			b.log.Info("quarantine lifted after the worker reset", "device", req.Device)
			dev.Quarantine, dev.QuarantineSent, dev.QuarantineWhy = false, false, ""
		} else {
			dev.QuarantineSent = true
			resp.Quarantine = true
		}
	}

	blocked := ""
	switch {
	case b.closed:
		blocked = "broker is shutting down"
	case resp.Quarantine:
		blocked = "device quarantined"
	case b.verdictPendingLocked(req.Device):
		blocked = "a runner of this device ended and is being checked"
	}
	if req.Free > 0 && blocked == "" {
		picks = b.pickLocked(req, now)
		for _, rec := range picks {
			rec.Node = node
		}
	}
	b.pruneArchivesLocked() // revokes the archive refs of expired grants too
	b.mu.Unlock()

	for _, rec := range expired {
		b.releaseCheck(rec)
	}
	b.saveCheckLogs(logs)
	if len(picks) > 0 {
		for _, rec := range picks {
			if offer, ok := b.realize(ctx, req, rec, now); ok {
				resp.Offers = append(resp.Offers, offer)
			}
		}
	}
	b.flushPosts(ctx, now)
	if err := b.save(); err != nil {
		b.log.Error("broker state not saved", "error", err)
	}
	return resp, nil
}

func (b *Broker) device(name string) *deviceRec {
	d := b.devices[name]
	if d == nil {
		d = &deviceRec{Name: name}
		b.devices[name] = d
	}
	return d
}

// endLocked handles a runner its worker reports ended.
func (b *Broker) endLocked(rec *grantRec, now time.Time) {
	if rec.pending {
		return
	}
	if rec.Kind == core.KindCheck {
		b.queuePost(rec, "error", "the job ended without a result", now)
		b.log.Warn("check job ended without a result", "runner", rec.Name, "device", rec.Device, "repo", rec.Repo, "context", rec.Context)
		delete(b.grants, rec.Name)
		return
	}
	rec.Ended, rec.EndedAt = true, now
	if rec.Settled {
		b.log.Info("runner ended", "runner", rec.Name, "device", rec.Device, "repo", rec.Repo, "job_id", rec.TookJob)
		b.queueReportLocked(rec)
		delete(b.grants, rec.Name)
	}
}

func (b *Broker) verdictPendingLocked(device string) bool {
	for _, rec := range b.grants {
		if rec.Device == device && rec.Kind == core.KindRunner && rec.Ended && !rec.Settled {
			return true
		}
	}
	return false
}

func (b *Broker) newNameLocked(device string) string {
	for {
		n := RunnerName(device)
		if b.grants[n] == nil {
			return n
		}
	}
}

// pickLocked reserves grants for the request. The reservations count as idle
// runners at once, so a concurrent request can't over-serve a job.
func (b *Broker) pickLocked(req core.WorkRequest, now time.Time) []*grantRec {
	skipped := false
	if req.OnlyRepoIdle {
		// Another repo waits for a device that only a reset frees: offer
		// nothing, so it empties, resets and takes that repo's job.
		if b.pickAllLocked(req, now, &skipped, false); skipped {
			return nil
		}
	}
	picks := b.pickAllLocked(req, now, &skipped, true)
	for _, rec := range picks {
		rec.drain = skipped
	}
	return picks
}

// pickAllLocked picks grants for req; it sets *skipped when a job this
// device could run waits only because of req.OnlyRepo. With reserve false
// it only sets *skipped and picks nothing.
func (b *Broker) pickAllLocked(req core.WorkRequest, now time.Time, skipped *bool, reserve bool) []*grantRec {
	var picks []*grantRec
	type cand struct {
		r      *repoState
		j      core.Job
		p      core.Platform
		labels []string
	}
	var cands []cand
	for _, r := range b.repos {
		if r.mode != "runner" || r.denied {
			continue
		}
		if why := b.repoRefusalLocked(r, now); why != "" {
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
			if !g.Busy && !g.Ended && !g.Settled {
				idle = append(idle, g)
			}
		}
		sort.Slice(idle, func(i, k int) bool { return idle[i].Granted.Before(idle[k].Granted) })
		used := make([]bool, len(idle))
		for _, j := range r.queued {
			if why := b.jobRefusal(j, now); why != "" {
				b.noteRefusal(j, why)
				continue
			}
			if took[j.ID] {
				continue
			}
			// An idle runner whose labels cover the job will be handed it
			// (or an equivalent one): the job is served already.
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
			p, labels, ok := MatchJob(j.Labels, req.Labels, req.Platforms)
			if !ok || keptRepo(req, r.forge, r.cfg.Name) {
				continue
			}
			// The device holds a repo's cache: only that repo's cache-safe
			// jobs may join it; anything else waits (see WorkOffer.Drain).
			if !onlyRepo(req, r.forge, r.cfg.Name) || (req.OnlyRepo != nil && !b.runnerCacheSafeLocked(r.forge, r.cfg.Name, j)) {
				*skipped = true
				continue
			}
			cands = append(cands, cand{r, j, p, labels})
		}
	}
	sort.SliceStable(cands, func(i, k int) bool {
		a, c := cands[i].j, cands[k].j
		if !a.CreatedAt.Equal(c.CreatedAt) {
			return a.CreatedAt.Before(c.CreatedAt)
		}
		return a.ID < c.ID
	})
	for _, c := range cands {
		if !reserve || len(picks) >= req.Free {
			break
		}
		rec := &grantRec{Name: b.newNameLocked(req.Device), Kind: core.KindRunner, Device: req.Device,
			Forge: c.r.forge, Repo: c.r.cfg.Name, Job: c.j, Platform: c.p, Labels: c.labels,
			Granted: now, pending: true}
		b.grants[rec.Name] = rec
		picks = append(picks, rec)
	}

	for _, r := range b.repos {
		if r.mode != "checks" || r.denied {
			continue
		}
		for _, h := range r.heads {
			for _, cj := range h.jobs {
				if reserve && len(picks) >= req.Free {
					return picks
				}
				ctxName := CheckContext(cj)
				retry := b.retries[checkKey(r.forge, r.cfg.Name, h.commit.SHA, ctxName)]
				if _, ok := h.statuses[ctxName]; ok && (retry == nil || !retry.open) {
					continue
				}
				if b.checkTakenLocked(r, h.commit.SHA, ctxName) {
					continue
				}
				p, ok := matchCheckPlatform(cj.Platform, req.Platforms)
				if !ok || keptRepo(req, r.forge, r.cfg.Name) {
					continue
				}
				if !onlyRepo(req, r.forge, r.cfg.Name) {
					*skipped = true
					continue
				}
				if !reserve {
					continue
				}
				job := core.Job{Forge: r.forge, Repo: r.cfg.Name, Name: cj.Name, Workflow: cj.Workflow,
					Event: CheckEvent, HeadBranch: h.commit.Ref, HeadSHA: h.commit.SHA, Status: "queued", CreatedAt: now,
					Labels: PlatformLabels(req.Labels, p)}
				rec := &grantRec{Name: b.newNameLocked(req.Device), Kind: core.KindCheck, Device: req.Device,
					Forge: r.forge, Repo: r.cfg.Name, Job: job, Platform: p, Labels: job.Labels,
					Granted: now, SHA: h.commit.SHA, Ref: h.commit.Ref, PR: h.commit.PR, Context: ctxName,
					pending: true}
				if retry != nil {
					retry.open = false
				}
				b.grants[rec.Name] = rec
				picks = append(picks, rec)
			}
		}
	}
	return picks
}

func (b *Broker) checkTakenLocked(r *repoState, sha, ctxName string) bool {
	for _, g := range b.grants {
		if g.Kind == core.KindCheck && g.Forge == r.forge && g.Repo == r.cfg.Name && g.SHA == sha && g.Context == ctxName {
			return true
		}
	}
	for _, p := range b.posts {
		if p.Forge == r.forge && p.Repo == r.cfg.Name && p.SHA == sha && p.Context == ctxName {
			return true
		}
	}
	return false
}

// realize turns a reservation into an offer: claim, grant, and for checks,
// fetch the archive URL and post "pending".
func (b *Broker) realize(ctx context.Context, req core.WorkRequest, rec *grantRec, now time.Time) (core.WorkOffer, bool) {
	drop := func(why string, err error) {
		b.mu.Lock()
		if b.grants[rec.Name] == rec {
			delete(b.grants, rec.Name)
		}
		b.mu.Unlock()
		if err != nil {
			b.noteErr(rec.Repo, why, err)
		}
	}
	if rec.Kind == core.KindCheck {
		return b.realizeCheck(ctx, req, rec, now, drop)
	}
	f := b.o.Forges[rec.Forge]
	if f == nil {
		drop("grant", fmt.Errorf("no forge named %q", rec.Forge))
		return core.WorkOffer{}, false
	}
	if b.o.Claimer != nil {
		cctx, cancel := context.WithTimeout(ctx, forgeTimeout)
		ok := b.o.Claimer.Claim(cctx, rec.Job, req.Device, b.claimTTL())
		cancel()
		if !ok {
			b.log.Debug("job claimed elsewhere; skipped", "repo", rec.Repo, "job_id", rec.Job.ID, "device", req.Device)
			drop("", nil)
			return core.WorkOffer{}, false
		}
	}
	fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	g, err := f.Grant(fctx, rec.Repo, rec.Name, rec.Labels)
	cancel()
	if err != nil {
		b.release(rec)
		drop("grant", err)
		return core.WorkOffer{}, false
	}
	b.mu.Lock()
	rec.RunnerID, rec.pending = g.RunnerID, false
	b.mu.Unlock()
	if g.RunnerName == "" {
		g.RunnerName = rec.Name
	}
	b.log.Info("runner granted", "runner", rec.Name, "runner_id", g.RunnerID, "device", req.Device,
		"repo", rec.Repo, "job_id", rec.Job.ID, "job", rec.Job.Name, "platform", rec.Platform.String(),
		"labels", strings.Join(rec.Labels, ","))
	return core.WorkOffer{Job: rec.Job, Grant: g, Secret: g.Secret, Platform: rec.Platform, Cache: b.cacheSafe(rec), Drain: rec.drain,
		Actions: b.actionsOffer(rec)}, true
}

// actionsOffer marks a runner offer for a github.com repo: only there does
// the toolkit use the cache service v2 the device's Actions cache speaks
// (GHES keeps the older protocol). The worker decides whether the job
// actually uses it, with the same trust rules as the repo cache.
func (b *Broker) actionsOffer(rec *grantRec) *core.ActionsOffer {
	name := rec.Forge
	if name == "" {
		name = "github"
	}
	fc, ok := b.o.Config.Forges[name]
	if !ok {
		fc.Kind = name
	}
	if fc.Kind == "" {
		fc.Kind = name
	}
	if fc.Kind != "github" || (fc.URL != "" && strings.TrimRight(fc.URL, "/") != "https://api.github.com") {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	off := &core.ActionsOffer{}
	if c := b.infos[rec.Forge+":"+rec.Repo]; c != nil && c.err == "" {
		off.DefaultBranch = c.defaultBranch
	}
	return off
}

// cacheSafe says whether a runner job may use its repo's persistent cache.
// A JIT runner takes whichever queued job matches its labels, not only the
// one it was granted for, so the job's own FromFork isn't enough: only a
// private repo with fork PR workflows off (the forge runs no fork job at
// all) is safe. The forge's answer is at most repoInfoTTL old, like the
// public-repo refusal it comes with.
func (b *Broker) cacheSafe(rec *grantRec) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runnerCacheSafeLocked(rec.Forge, rec.Repo, rec.Job)
}

func (b *Broker) runnerCacheSafeLocked(forge, repo string, j core.Job) bool {
	if j.FromFork {
		return false
	}
	c := b.infos[forge+":"+repo]
	return c != nil && c.err == "" && c.noForks
}

// onlyRepo reports whether repo r may serve a request restricted to
// req.OnlyRepo (always true without one).
func onlyRepo(req core.WorkRequest, forge, repo string) bool {
	o := req.OnlyRepo
	return o == nil || (o.Forge == forge && strings.EqualFold(o.Repo, repo))
}

// keptRepo reports whether the device keeps a failed job's sandbox of
// repo, so it takes none of the repo's jobs (req.Kept).
func keptRepo(req core.WorkRequest, forge, repo string) bool {
	for _, k := range req.Kept {
		if k.Forge == forge && strings.EqualFold(k.Repo, repo) {
			return true
		}
	}
	return false
}

func (b *Broker) realizeCheck(ctx context.Context, req core.WorkRequest, rec *grantRec, now time.Time, drop func(string, error)) (core.WorkOffer, bool) {
	sf := b.o.StatusForges[rec.Forge]
	if sf == nil {
		drop("grant", fmt.Errorf("no status forge named %q", rec.Forge))
		return core.WorkOffer{}, false
	}
	b.mu.Lock()
	var cj core.CheckJob
	found := false
	for _, r := range b.repos {
		if r.forge != rec.Forge || r.cfg.Name != rec.Repo {
			continue
		}
		for _, h := range r.heads {
			if h.commit.SHA != rec.SHA {
				continue
			}
			for _, j := range h.jobs {
				if CheckContext(j) == rec.Context {
					cj, found = j, true
				}
			}
		}
	}
	b.mu.Unlock()
	if !found {
		drop("", nil)
		return core.WorkOffer{}, false
	}
	// Another broker with this repo sees the same commit with no status
	// yet. The claim, held by this broker rather than the worker, makes one
	// of them grant it: a worker asks every broker, so a claim held by the
	// worker would let each broker hand it the same check.
	if b.o.Claimer != nil {
		cctx, cancel := context.WithTimeout(ctx, forgeTimeout)
		ok := b.o.Claimer.Claim(cctx, rec.Job, b.name, b.claimTTL())
		cancel()
		if !ok {
			b.log.Debug("check claimed by another broker; skipped", "repo", rec.Repo, "sha", rec.SHA, "context", rec.Context, "device", req.Device)
			drop("", nil)
			return core.WorkOffer{}, false
		}
		claimed := drop
		drop = func(why string, err error) {
			b.releaseCheck(rec)
			claimed(why, err)
		}
	}
	// The bundle names the archive by an opaque ref the granted device
	// redeems through FetchArchive; no forge URL or token leaves the broker.
	b.mu.Lock()
	ref, err := b.addArchiveLocked(rec)
	b.mu.Unlock()
	if err != nil {
		drop("archive ref", err)
		return core.WorkOffer{}, false
	}
	secret, err := json.Marshal(core.CheckBundle{Repo: rec.Repo, SHA: rec.SHA, Ref: rec.Ref, ArchiveRef: ref, Job: cj})
	if err != nil {
		drop("bundle", err)
		return core.WorkOffer{}, false
	}
	fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	err = sf.PostStatus(fctx, rec.Repo, rec.SHA, rec.Context, "pending", truncate("running on "+req.Device, 140), "")
	cancel()
	if err != nil {
		// The final status still posts; the record keeps the job from
		// being offered twice meanwhile.
		b.noteErr(rec.Repo, "post pending status", err)
	}
	b.mu.Lock()
	rec.pending = false
	b.setHeadStatusLocked(rec.Forge, rec.Repo, rec.SHA, rec.Context, "pending")
	b.mu.Unlock()
	b.log.Info("check granted", "runner", rec.Name, "device", req.Device, "repo", rec.Repo, "sha", rec.SHA,
		"context", rec.Context, "platform", rec.Platform.String())
	g := core.Grant{Forge: rec.Forge, Repo: rec.Repo, RunnerName: rec.Name, Labels: rec.Labels,
		Secret: secret, ExpiresAt: now.Add(time.Hour)}
	// A check job runs exactly one commit the broker picked, never a fork's
	// (pollChecks skips them), so it may use the repo's cache.
	return core.WorkOffer{Job: rec.Job, Grant: g, Secret: secret, Platform: rec.Platform, Cache: !rec.Job.FromFork, Drain: rec.drain}, true
}

// maxCheckRetries is how many times a check whose grant ended unstarted
// (expired, declined or failed to start) is offered again.
const maxCheckRetries = 2

type checkRetry struct {
	expired int
	open    bool // offer it again despite the error status this broker posted
}

func checkKey(forge, repo, sha, ctxName string) string {
	return forge + ":" + repo + "@" + sha + "#" + ctxName
}

// expireCheckLocked ends a check grant its worker never ran (lost on the
// way, declined, or failed to start): it posts an error status saying why,
// forgets the grant (pruneArchivesLocked then revokes its archive ref) and,
// up to maxCheckRetries times, lets pickLocked offer the check again. The
// caller releases the claim. A worker that starts it late runs an unknown
// runner, which the broker stops, and its archive ref is gone.
func (b *Broker) expireCheckLocked(rec *grantRec, now time.Time, why string) {
	k := checkKey(rec.Forge, rec.Repo, rec.SHA, rec.Context)
	retry := b.retries[k]
	if retry == nil {
		retry = &checkRetry{}
		b.retries[k] = retry
	}
	retry.expired++
	retry.open = retry.expired <= maxCheckRetries
	desc := why
	if retry.open {
		desc += "; offering it again"
	}
	b.queuePost(rec, "error", desc, now)
	b.log.Warn("check grant never ran; expired", "why", why, "runner", rec.Name, "device", rec.Device, "repo", rec.Repo,
		"sha", rec.SHA, "context", rec.Context, "again", retry.open)
	delete(b.grants, rec.Name)
}

func (b *Broker) claimTTL() time.Duration { return b.idleTTL + 2*time.Minute }

// releaseCheck drops this broker's claim on a check it did not grant after
// all.
func (b *Broker) releaseCheck(rec *grantRec) {
	if b.o.Claimer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), forgeTimeout)
	defer cancel()
	b.o.Claimer.Release(ctx, rec.Job, b.name)
}

func (b *Broker) release(rec *grantRec) {
	if b.o.Claimer == nil || rec.Kind != core.KindRunner {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), forgeTimeout)
	defer cancel()
	b.o.Claimer.Release(ctx, rec.Job, rec.Device)
}

// ---------------------------------------------------------------- refusals

func (b *Broker) repoRefusalLocked(r *repoState, now time.Time) string {
	c := b.infos[r.key()]
	if c == nil {
		return "repo settings not read yet"
	}
	if c.err != "" {
		return "can't read repo settings: " + c.err
	}
	return c.why
}

func repoRefusal(r *repoState, info core.RepoInfo) string {
	return PublicRepoRefusal(info, r.cfg.AllowPublic)
}

// PublicRepoRefusal says why the broker refuses a runner-mode repo for being
// public, or "" when it may run: a public repo needs allow_public and a fork
// policy of "approve_all" or "off". Doctor calls it too, so the two never
// disagree about a repo.
func PublicRepoRefusal(info core.RepoInfo, allowPublic bool) string {
	if !info.Public {
		return ""
	}
	if !allowPublic {
		return "public repo without allow_public"
	}
	switch info.ForkPolicy {
	case "approve_all", "off":
		return ""
	}
	return fmt.Sprintf("public repo whose fork policy is %q; require approval for all outside contributors first", info.ForkPolicy)
}

func (b *Broker) jobRefusal(j core.Job, now time.Time) string {
	switch {
	case j.FromFork:
		return "job from a fork"
	case j.Status != "queued":
		return "status " + j.Status
	case !hasLabel(j.Labels, LabelTentmaker):
		return "no tentmaker label"
	case j.CreatedAt.IsZero():
		return "unknown age"
	case now.Sub(j.CreatedAt) > maxJobAge:
		return "older than 24 h"
	}
	return ""
}

func (b *Broker) noteRefusal(j core.Job, why string) {
	k := j.Key()
	if b.noted[k] == why {
		return
	}
	b.noted[k] = why
	lvl := slog.LevelInfo
	if why == "no tentmaker label" {
		lvl = slog.LevelDebug
	} else if j.FromFork {
		lvl = slog.LevelWarn
	}
	b.log.Log(context.Background(), lvl, "job refused", "repo", j.Repo, "job_id", j.ID, "job", j.Name, "why", why)
}

// ---------------------------------------------------------------- Tick

// Run polls and watches until ctx ends.
func (b *Broker) Run(ctx context.Context) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if err := b.Tick(ctx); err != nil {
			b.log.Error("broker tick failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Tick runs one pass: poll the repos that are due, watch granted runners,
// handle silent workers, post statuses and save the state.
func (b *Broker) Tick(ctx context.Context) error {
	b.tickMu.Lock()
	defer b.tickMu.Unlock()
	now := b.now()
	for _, r := range b.repos {
		b.mu.Lock()
		// Run ticks every second; the slack keeps a 2 s poll from slipping
		// to 3 s when a tick lands a hair early.
		due := !r.denied && !now.Add(tickSlack).Before(r.next)
		b.mu.Unlock()
		if !due {
			continue
		}
		if r.mode == "checks" {
			b.pollChecks(ctx, r, now)
		} else {
			b.pollRunner(ctx, r, now)
		}
	}
	b.sweepSilent(ctx, now)
	b.monitor(ctx, now)
	b.sendReports(ctx, now)
	b.flushPosts(ctx, now)
	b.mu.Lock()
	b.pruneArchivesLocked()
	b.mu.Unlock()
	return b.save()
}

// polledLocked schedules the next poll after a good one. A forge whose
// unchanged polls are free is polled again in pollHot while the repo is
// active (hotWindow) and pollFast when it is idle, each stretched with the
// number of repos, busy or not, so a push is seen within about a second.
// A forge that says its limit runs low (not CheapPolls) is paced by its
// PollDelay when it is a core.PollPacer (at least pollFast) and backs off as
// backoffLocked says otherwise. Errors and refused repos always go through
// backoffLocked. changed says the poll found something new.
func (b *Broker) polledLocked(r *repoState, now time.Time, busy, changed bool) {
	if busy || changed || b.repoRunningLocked(r) {
		r.active = now
	}
	if !b.cheapLocked(r) {
		if d, ok := b.pollDelayLocked(r); ok {
			r.lastPoll = now
			r.interval = max(d, pollFast)
			r.next = now.Add(r.interval)
			return
		}
		b.backoffLocked(r, now, busy)
		return
	}
	n := time.Duration(len(b.repos))
	d := max(n*pollFastPerRepo, pollFast)
	if now.Sub(r.active) < hotWindow {
		d = max(n*pollHotPerRepo, pollHot)
	}
	if pd, ok := b.pollDelayLocked(r); ok {
		d = max(d, pd)
	}
	r.lastPoll = now
	r.interval = d
	r.next = now.Add(d)
}

// repoRunningLocked reports whether a grant of repo r has not ended.
func (b *Broker) repoRunningLocked(r *repoState) bool {
	for _, g := range b.grants {
		if g.Forge == r.forge && g.Repo == r.cfg.Name && !g.Ended {
			return true
		}
	}
	return false
}

// pollForgeLocked is repo r's forge: the status forge in checks mode.
func (b *Broker) pollForgeLocked(r *repoState) any {
	if r.mode == "checks" {
		return b.o.StatusForges[r.forge]
	}
	return b.o.Forges[r.forge]
}

// cheapLocked reports whether repo r's forge says its polls are free now.
func (b *Broker) cheapLocked(r *repoState) bool {
	cp, ok := b.pollForgeLocked(r).(core.CheapPoller)
	return ok && cp.CheapPolls()
}

// pollDelayLocked is repo r's forge's PollDelay, when it is a PollPacer.
func (b *Broker) pollDelayLocked(r *repoState) (time.Duration, bool) {
	pp, ok := b.pollForgeLocked(r).(core.PollPacer)
	if !ok {
		return 0, false
	}
	return pp.PollDelay(), true
}

// backoffLocked schedules the next poll: 5 s while there is work, else the
// current interval, which then grows x1.5 up to 30 s. An interval past 30 s
// is a pace PollDelay set (polledLocked), not a backoff, so it starts over
// at 5 s: a poll refused just before a reset must not wait out the whole
// pace again (issue #39). A forge whose PollDelay is longer (its rate limit
// is used up) is polled after that instead, so a refused poll isn't retried
// before the forge would answer.
func (b *Broker) backoffLocked(r *repoState, now time.Time, busy bool) {
	if busy || r.interval < pollMin || r.interval > pollMax {
		r.interval = pollMin
	}
	r.lastPoll = now
	r.next = now.Add(r.interval)
	if d, ok := b.pollDelayLocked(r); ok && now.Add(d).After(r.next) {
		r.next = now.Add(d)
	}
	if !busy {
		r.interval = time.Duration(float64(r.interval) * pollFactor)
		if r.interval > pollMax {
			r.interval = pollMax
		}
	}
}

func (b *Broker) pollRunner(ctx context.Context, r *repoState, now time.Time) {
	f := b.o.Forges[r.forge]
	if f == nil {
		b.repoFailed(r, now, "poll", fmt.Errorf("no forge named %q", r.forge))
		return
	}
	b.mu.Lock()
	c := b.infos[r.key()]
	stale := c == nil || (c.err == "" && now.Sub(c.at) >= repoInfoTTL) || (c.err != "" && now.Sub(c.at) >= repoInfoRetry)
	b.mu.Unlock()
	if stale {
		fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
		info, err := f.CheckRepo(fctx, r.cfg.Name)
		cancel()
		nc := &infoCache{at: now}
		if err != nil {
			nc.err = err.Error()
		} else {
			nc.why = repoRefusal(r, info)
			nc.noForks = !info.Public && info.ForkPolicy == "off"
			nc.defaultBranch = info.DefaultBranch
		}
		b.mu.Lock()
		old := b.infos[r.key()]
		b.infos[r.key()] = nc
		b.mu.Unlock()
		if err != nil {
			b.noteErr(r.cfg.Name, "check repo", err)
		} else if nc.why != "" && (old == nil || old.why != nc.why) {
			b.log.Warn("repo refused", "repo", r.cfg.Name, "why", nc.why)
		}
	}
	b.mu.Lock()
	why := b.repoRefusalLocked(r, now)
	if why != "" {
		r.queued = nil
		b.backoffLocked(r, now, false)
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	jobs, err := f.QueuedJobs(fctx, r.cfg.Name)
	cancel()
	if err != nil {
		b.repoFailed(r, now, "poll queued jobs", err)
		return
	}
	for i := range jobs {
		if jobs[i].Forge == "" {
			jobs[i].Forge = r.forge
		}
		if jobs[i].Repo == "" {
			jobs[i].Repo = r.cfg.Name
		}
	}
	sort.SliceStable(jobs, func(i, k int) bool { return jobs[i].CreatedAt.Before(jobs[k].CreatedAt) })
	b.mu.Lock()
	r.queued, r.lastErr = jobs, ""
	busy := false
	live := map[string]bool{}
	var keys []string
	for _, j := range jobs {
		live[j.Key()] = true
		keys = append(keys, j.Key())
		if b.jobRefusal(j, now) == "" {
			busy = true
		}
	}
	seen := strings.Join(keys, ",")
	changed := seen != r.seen
	r.seen = seen
	for k := range b.noted {
		if strings.HasPrefix(k, r.forge+":"+r.cfg.Name+":") && !live[k] {
			delete(b.noted, k)
		}
	}
	b.polledLocked(r, now, busy, changed)
	b.mu.Unlock()
	b.foundWork(busy)
}

// foundWork tells the daemon a poll found work.
func (b *Broker) foundWork(busy bool) {
	if busy && b.o.OnWork != nil {
		b.o.OnWork()
	}
}

// repoFailed records a per-repo error. The stale queue is dropped (no grant
// on old data) and the repo backs off; other repos go on.
func (b *Broker) repoFailed(r *repoState, now time.Time, what string, err error) {
	b.mu.Lock()
	r.queued = nil
	r.lastErr = what + ": " + err.Error()
	b.backoffLocked(r, now, false)
	b.mu.Unlock()
	b.noteErr(r.cfg.Name, what, err)
}

func (b *Broker) noteErr(repo, what string, err error) {
	b.log.Warn("forge call failed", "repo", repo, "call", what, "error", err)
	b.mu.Lock()
	b.errs.add(b.now(), repo+": "+what, err.Error())
	b.mu.Unlock()
}

// ---------------------------------------------------------------- monitoring

func (b *Broker) monitor(ctx context.Context, now time.Time) {
	b.mu.Lock()
	var recs []*grantRec
	for _, rec := range b.grants {
		if rec.Kind != core.KindRunner || rec.pending {
			continue
		}
		if rec.Settled && !rec.Ended {
			continue // deleted or stopped already; waiting for the worker
		}
		if !rec.checked.IsZero() && now.Sub(rec.checked) < monitorEvery && !rec.Ended {
			continue
		}
		rec.checked = now
		recs = append(recs, rec)
	}
	b.mu.Unlock()
	sort.Slice(recs, func(i, k int) bool { return recs[i].Name < recs[k].Name })
	for _, rec := range recs {
		b.mu.Lock()
		if b.grants[rec.Name] != rec {
			b.mu.Unlock()
			continue
		}
		r := *rec
		b.mu.Unlock()
		f := b.o.Forges[r.Forge]
		if f == nil {
			b.mu.Lock()
			b.quarantineLocked(r.Device, "a runner's forge is no longer configured")
			b.stopLocked(rec)
			b.mu.Unlock()
			continue
		}
		if r.Ended {
			b.verdict(ctx, f, rec, r, now)
		} else {
			b.watch(ctx, f, rec, r, now)
		}
	}
}

// watch checks a live, unsettled runner: did it take a job, which one, and is
// it a fork's? Is it idle past its TTL?
func (b *Broker) watch(ctx context.Context, f core.Forge, rec *grantRec, r grantRec, now time.Time) {
	if r.Started.IsZero() {
		if now.Sub(r.Granted) >= startGrace {
			b.removeIdle(ctx, f, rec, r, "never reported running")
		}
		return
	}
	fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	st, err := f.Runner(fctx, r.Repo, r.RunnerID)
	cancel()
	if err != nil {
		b.noteErr(r.Repo, "read runner", err)
		b.mu.Lock()
		if rec.ErrSince.IsZero() {
			rec.ErrSince = now
		}
		since := rec.ErrSince
		b.mu.Unlock()
		if now.Sub(since) >= identifyTimeout && now.Sub(r.Started) >= identifyTimeout {
			// Can't tell whether it took a job, or which: stop it.
			b.log.Warn("runner can't be checked; stopping it", "runner", r.Name, "device", r.Device, "repo", r.Repo)
			b.mu.Lock()
			b.stopLocked(rec)
			b.quarantineLocked(r.Device, "a runner's state could not be read for 30 s")
			b.mu.Unlock()
			b.tryDelete(ctx, f, r)
		}
		return
	}
	b.mu.Lock()
	rec.ErrSince = time.Time{}
	b.mu.Unlock()
	if !st.Exists {
		// Gone at the forge while the worker still runs it: it ran a job
		// between two checks, or someone deleted it. Stop it; when it ends,
		// the verdict decides.
		b.log.Warn("runner gone at the forge while still running; stopping it", "runner", r.Name, "device", r.Device)
		b.mu.Lock()
		b.addStopLocked(rec)
		b.mu.Unlock()
		return
	}
	if !st.Busy && !r.Busy {
		if now.Sub(r.Started) >= b.idleTTL {
			b.removeIdle(ctx, f, rec, r, "idle past idle_ttl")
		}
		return
	}
	b.mu.Lock()
	if !rec.Busy {
		rec.Busy, rec.BusySince = true, now
	}
	busySince := rec.BusySince
	b.mu.Unlock()
	j, ok, err := b.busyJob(ctx, f, r, st)
	if err != nil {
		b.noteErr(r.Repo, "read job", err)
		b.unidentified(ctx, f, rec, r, busySince, now, "its job could not be read")
		return
	}
	if !ok {
		b.unidentified(ctx, f, rec, r, busySince, now, "no running job names this runner")
		return
	}
	b.tookLocked(rec, j)
	if j.FromFork {
		b.forkCaught(ctx, f, rec, r, j)
		return
	}
	b.log.Info("runner took a job", "runner", r.Name, "device", r.Device, "repo", r.Repo, "job_id", j.ID,
		"job", j.Name, "granted_for", r.Job.ID)
}

// busyJob finds the job a busy runner took: by the job ID the forge
// reports, else among the repo's running jobs by runner ID or name (GitHub's
// runner API never says which job a busy runner has). ok is false when no
// job names this runner.
func (b *Broker) busyJob(ctx context.Context, f core.Forge, r grantRec, st core.RunnerStatus) (core.Job, bool, error) {
	fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	defer cancel()
	if st.JobID != 0 {
		j, err := f.JobByID(fctx, r.Repo, st.JobID)
		return j, err == nil, err
	}
	jobs, err := f.RunningJobs(fctx, r.Repo)
	if err != nil {
		return core.Job{}, false, err
	}
	for _, j := range jobs {
		if ranOn(j, r) {
			return j, true, nil
		}
	}
	return core.Job{}, false, nil
}

// endedJob finds the job a runner that is gone at the forge ran: still
// listed as running, or the job it was granted for when that job names it.
// A runner that took another job and ended between two checks is not found.
func (b *Broker) endedJob(ctx context.Context, f core.Forge, r grantRec) (core.Job, bool, error) {
	if j, ok, err := b.busyJob(ctx, f, r, core.RunnerStatus{}); err != nil || ok {
		return j, ok, err
	}
	if r.Job.ID == 0 {
		return core.Job{}, false, nil
	}
	fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	defer cancel()
	j, err := f.JobByID(fctx, r.Repo, r.Job.ID)
	if errors.Is(err, core.ErrNotFound) {
		return core.Job{}, false, nil
	}
	if err != nil {
		return core.Job{}, false, err
	}
	return j, ranOn(j, r), nil
}

// ranOn reports whether job j names runner r. Runner IDs decide when both
// are known; else the name, which the broker made unique.
func ranOn(j core.Job, r grantRec) bool {
	if j.RunnerID != 0 && r.RunnerID != 0 {
		return j.RunnerID == r.RunnerID
	}
	return j.RunnerName != "" && strings.EqualFold(j.RunnerName, r.Name)
}

// tookLocked records the job a runner took. A same-repo job settles it.
func (b *Broker) tookLocked(rec *grantRec, j core.Job) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec.TookJob, rec.TookRun, rec.Identified, rec.Fork = j.ID, j.RunID, true, j.FromFork
	rec.TookEvent, rec.TookBranch = j.Event, j.HeadBranch
	rec.Settled = !j.FromFork
}

func (b *Broker) unidentified(ctx context.Context, f core.Forge, rec *grantRec, r grantRec, busySince, now time.Time, why string) {
	if now.Sub(busySince) < identifyTimeout {
		return
	}
	b.log.Warn("busy runner's job unidentified for 30 s; stopping it", "runner", r.Name, "device", r.Device,
		"repo", r.Repo, "why", why)
	b.mu.Lock()
	b.stopLocked(rec)
	b.quarantineLocked(r.Device, "a runner took a job tentmaker could not identify")
	b.mu.Unlock()
	b.tryDelete(ctx, f, r)
}

// forkCaught: a fork's job reached our runner (a JIT runner takes any job
// its labels match). Stop it, cancel the run, delete the runner, and make
// the device reset before it takes more work.
func (b *Broker) forkCaught(ctx context.Context, f core.Forge, rec *grantRec, r grantRec, j core.Job) {
	b.log.Warn("fork job reached a runner; stopping it and cancelling the run", "runner", r.Name,
		"device", r.Device, "repo", r.Repo, "job_id", j.ID, "run_id", j.RunID)
	b.mu.Lock()
	b.stopLocked(rec)
	b.quarantineLocked(r.Device, "a fork's job reached one of its runners")
	b.mu.Unlock()
	fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	if err := f.CancelRun(fctx, r.Repo, j.RunID); err != nil {
		b.noteErr(r.Repo, "cancel fork run", err)
	}
	cancel()
	b.tryDelete(ctx, f, r)
}

func (b *Broker) removeIdle(ctx context.Context, f core.Forge, rec *grantRec, r grantRec, why string) {
	fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	err := f.DeleteRunner(fctx, r.Repo, r.RunnerID)
	cancel()
	if err != nil {
		// The forge refuses to delete a runner that just took a job; the
		// next check sees it busy.
		b.log.Info("idle runner not deleted; checking again", "runner", r.Name, "why", why, "error", err)
		return
	}
	b.log.Info("idle runner removed", "runner", r.Name, "device", r.Device, "repo", r.Repo, "why", why)
	b.mu.Lock()
	b.stopLocked(rec)
	b.mu.Unlock()
	b.release(&r)
}

func (b *Broker) tryDelete(ctx context.Context, f core.Forge, r grantRec) {
	fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	defer cancel()
	if err := f.DeleteRunner(fctx, r.Repo, r.RunnerID); err != nil {
		b.log.Info("runner not deleted (it may be busy; it ends with its stop)", "runner", r.Name, "error", err)
	}
}

// verdict decides what an ended, unsettled runner did. A runner that never
// took a job is still registered and idle: delete it. A runner that is gone
// ran a job; if we never learned which, the device is quarantined.
func (b *Broker) verdict(ctx context.Context, f core.Forge, rec *grantRec, r grantRec, now time.Time) {
	settle := func() {
		b.mu.Lock()
		if b.grants[rec.Name] == rec {
			b.queueReportLocked(rec)
			delete(b.grants, rec.Name)
		}
		b.mu.Unlock()
	}
	giveUp := func(why string) {
		b.log.Warn("ended runner's job unidentified; quarantining the device", "runner", r.Name,
			"device", r.Device, "repo", r.Repo, "why", why)
		b.mu.Lock()
		b.quarantineLocked(r.Device, "a runner ended after a job tentmaker could not identify")
		b.mu.Unlock()
		settle()
	}
	fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	st, err := f.Runner(fctx, r.Repo, r.RunnerID)
	cancel()
	if err != nil {
		b.noteErr(r.Repo, "read ended runner", err)
		if now.Sub(r.EndedAt) >= identifyTimeout {
			giveUp("runner state unreadable")
		}
		return
	}
	switch {
	case st.Exists && !st.Busy:
		fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
		err := f.DeleteRunner(fctx, r.Repo, r.RunnerID)
		cancel()
		if err != nil {
			b.noteErr(r.Repo, "delete unused runner", err)
			if now.Sub(r.EndedAt) >= identifyTimeout {
				giveUp("unused runner could not be deleted")
			}
			return
		}
		b.log.Info("runner ended without taking a job", "runner", r.Name, "device", r.Device, "repo", r.Repo)
		b.release(&r)
		settle()
	case st.Exists && st.Busy:
		j, ok, err := b.busyJob(ctx, f, r, st)
		if err != nil || !ok {
			if err != nil {
				b.noteErr(r.Repo, "read ended runner's job", err)
			}
			if now.Sub(r.EndedAt) >= identifyTimeout {
				b.tryDelete(ctx, f, r)
				giveUp(unknownWhy(err, "busy with an unknown job"))
			}
			return
		}
		b.tookLocked(rec, j)
		if j.FromFork {
			b.forkCaught(ctx, f, rec, r, j)
		} else {
			b.tryDelete(ctx, f, r)
		}
		settle()
	default:
		j, ok, err := b.endedJob(ctx, f, r)
		if err != nil || !ok {
			if err != nil {
				b.noteErr(r.Repo, "read ended runner's job", err)
			}
			if now.Sub(r.EndedAt) >= identifyTimeout {
				giveUp(unknownWhy(err, "runner gone after an unseen job"))
			}
			return
		}
		b.tookLocked(rec, j)
		if j.FromFork {
			b.forkCaught(ctx, f, rec, r, j)
		} else {
			b.log.Info("ended runner's job identified", "runner", r.Name, "device", r.Device, "repo", r.Repo,
				"job_id", j.ID, "job", j.Name)
		}
		settle()
	}
}

// unknownWhy says why an ended runner's job stayed unknown.
func unknownWhy(err error, why string) string {
	if err != nil {
		return "job unreadable"
	}
	return why
}

func (b *Broker) addStopLocked(rec *grantRec) {
	d := b.device(rec.Device)
	for _, n := range d.Stop {
		if n == rec.Name {
			return
		}
	}
	d.Stop = append(d.Stop, rec.Name)
}

// stopLocked tells the worker to stop the runner; nothing more is learned
// about it.
func (b *Broker) stopLocked(rec *grantRec) {
	rec.Settled = true
	if rec.Ended {
		delete(b.grants, rec.Name)
		return
	}
	b.addStopLocked(rec)
}

func (b *Broker) quarantineLocked(device, why string) {
	d := b.device(device)
	if !d.Quarantine {
		b.log.Warn("device quarantined until it resets", "device", device, "why", why)
		b.errs.add(b.now(), "quarantine "+device, why)
	}
	d.Quarantine, d.QuarantineWhy, d.QuarantineSent = true, why, false
}

// sweepSilent handles workers silent for 2 minutes: their runners are
// deleted and their check jobs get an error status.
func (b *Broker) sweepSilent(ctx context.Context, now time.Time) {
	b.mu.Lock()
	for _, d := range b.devices {
		if !d.Offline && now.Sub(d.LastSeen) >= workerSilence {
			d.Offline = true
			b.log.Warn("device silent for 2 minutes; dropping its runners", "device", d.Name)
		}
	}
	var recs []*grantRec
	for _, rec := range b.grants {
		d := b.devices[rec.Device]
		if d == nil || !d.Offline || rec.pending {
			continue
		}
		if rec.Kind == core.KindCheck {
			b.queuePost(rec, "error", "device went offline", now)
			b.log.Warn("check job lost with its device", "runner", rec.Name, "device", rec.Device, "repo", rec.Repo, "context", rec.Context)
			delete(b.grants, rec.Name)
			continue
		}
		if !rec.checked.IsZero() && now.Sub(rec.checked) < monitorEvery {
			continue
		}
		rec.checked = now
		recs = append(recs, rec)
	}
	b.mu.Unlock()
	for _, rec := range recs {
		f := b.o.Forges[rec.Forge]
		if f == nil {
			continue
		}
		b.mu.Lock()
		r := *rec
		b.mu.Unlock()
		fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
		err := f.DeleteRunner(fctx, r.Repo, r.RunnerID)
		cancel()
		if err != nil {
			b.log.Info("silent device's runner not deleted yet", "runner", r.Name, "error", err)
			continue
		}
		b.log.Info("silent device's runner deleted", "runner", r.Name, "device", r.Device)
		b.release(&r)
		b.mu.Lock()
		if b.grants[rec.Name] == rec {
			delete(b.grants, rec.Name)
		}
		b.mu.Unlock()
	}
}

// ---------------------------------------------------------------- checks mode

func (b *Broker) pollChecks(ctx context.Context, r *repoState, now time.Time) {
	sf := b.o.StatusForges[r.forge]
	if sf == nil {
		b.repoFailed(r, now, "poll commits", fmt.Errorf("no status forge named %q", r.forge))
		return
	}
	if b.o.Planner == nil {
		b.repoFailed(r, now, "plan", errors.New("checks mode needs a workflow planner"))
		return
	}
	fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
	commits, err := sf.PendingCommits(fctx, r.cfg.Name, r.cfg.Branches...)
	cancel()
	if err != nil {
		b.repoFailed(r, now, "poll commits", err)
		return
	}
	authors := r.cfg.Authors
	if len(authors) == 0 {
		authors = []string{ownerOf(r.cfg.Name)}
	}
	seen := map[string]bool{}
	var todo []core.Commit
	for _, c := range commits {
		if c.Repo == "" {
			c.Repo = r.cfg.Name
		}
		key := "ref:" + c.Ref
		if c.PR > 0 {
			key = fmt.Sprintf("pr:%d", c.PR)
		}
		if c.FromFork {
			b.noteCommit(r, c, "from a fork")
			continue
		}
		if c.PR > 0 && !containsFold(authors, c.Author) {
			b.noteCommit(r, c, "author "+c.Author+" is not in authors")
			continue
		}
		if c.SHA == "" || seen[key] {
			continue // the forge lists the newest head first
		}
		seen[key] = true
		todo = append(todo, c)
	}
	// The last poll's heads by SHA: their statuses are reused when nothing
	// can have changed them but this broker, which keeps its copy current.
	b.mu.Lock()
	prev := map[string]*checkHead{}
	var shas []string
	for _, h := range r.heads {
		prev[h.commit.SHA] = h
	}
	for _, c := range todo {
		shas = append(shas, c.SHA)
	}
	changed := strings.Join(shas, ",") != r.seen
	r.seen = strings.Join(shas, ",")
	b.mu.Unlock()
	// Heads are planned and read at once, at most headWorkers at a time, so
	// a new commit costs one round trip more than an idle poll.
	got := make([]*checkHead, len(todo))
	sem := make(chan struct{}, headWorkers)
	var wg sync.WaitGroup
	for i, c := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			got[i] = b.readHead(ctx, sf, r, c, prev[c.SHA], now)
		}()
	}
	wg.Wait()
	var heads []*checkHead
	busy := false
	for _, h := range got {
		if h == nil {
			continue
		}
		for _, j := range h.jobs {
			if _, ok := h.statuses[CheckContext(j)]; !ok {
				busy = true
			}
		}
		heads = append(heads, h)
	}
	b.mu.Lock()
	r.heads, r.lastErr = heads, ""
	live := map[string]bool{}
	for _, h := range heads {
		live[r.key()+"@"+h.commit.SHA] = true
	}
	for k := range b.plans {
		if strings.HasPrefix(k, r.key()+"@") && !live[k] {
			delete(b.plans, k)
		}
	}
	for k := range b.retries {
		if head, _, ok := strings.Cut(k, "#"); ok && strings.HasPrefix(k, r.key()+"@") && !live[head] {
			delete(b.retries, k)
		}
	}
	// Forget ignored commits the forge no longer lists (a closed fork PR,
	// a force-push), or the map grows with every fork push for months.
	listed := map[string]bool{}
	for _, c := range commits {
		listed[r.key()+"@"+c.SHA] = true
	}
	for k := range b.noted {
		if strings.HasPrefix(k, r.key()+"@") && !listed[k] {
			delete(b.noted, k)
		}
	}
	b.polledLocked(r, now, busy, changed)
	b.mu.Unlock()
	b.foundWork(busy)
}

// readHead plans commit c and reads its statuses. A head with no planned
// job needs no statuses. A head seen last poll (old) whose jobs all had a
// status keeps that copy until statusRecheck; one with a job waiting, and a
// new head, are read now. A new head is planned and read at once. It
// returns nil when the forge failed; the error is noted.
func (b *Broker) readHead(ctx context.Context, sf core.StatusForge, r *repoState, c core.Commit, old *checkHead, now time.Time) *checkHead {
	h := &checkHead{commit: c, statuses: map[string]string{}}
	read := func() error {
		fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
		defer cancel()
		sts, err := sf.Statuses(fctx, r.cfg.Name, c.SHA)
		if err != nil {
			return err
		}
		h.statuses, h.read = sts, now
		return nil
	}
	var (
		jobs    []core.CheckJob
		planErr error
		readErr error
		early   bool
	)
	if old == nil {
		// A new commit: read its statuses while planning it.
		early = true
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); readErr = read() }()
		jobs, planErr = b.plan(ctx, sf, r, c.SHA)
		wg.Wait()
	} else {
		jobs, planErr = b.plan(ctx, sf, r, c.SHA)
	}
	if planErr != nil {
		b.noteErr(r.cfg.Name, "plan "+short(c.SHA), planErr)
		return nil
	}
	h.jobs = jobs
	if len(jobs) == 0 {
		return h
	}
	if !early {
		b.mu.Lock()
		settled := now.Sub(old.read) < statusRecheck
		for _, j := range jobs {
			if _, ok := old.statuses[CheckContext(j)]; !ok {
				settled = false
			}
		}
		if settled {
			h.statuses, h.read = maps.Clone(old.statuses), old.read
		}
		b.mu.Unlock()
		if !settled {
			readErr = read()
		}
	}
	if readErr != nil {
		b.noteErr(r.cfg.Name, "read statuses "+short(c.SHA), readErr)
		return nil
	}
	return h
}

func (b *Broker) noteCommit(r *repoState, c core.Commit, why string) {
	k := r.key() + "@" + c.SHA
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.noted[k] == why {
		return
	}
	b.noted[k] = why
	b.log.Info("commit ignored", "repo", r.cfg.Name, "sha", short(c.SHA), "pr", c.PR, "why", why)
}

// plan reads the workflows at sha and plans their jobs, cached per sha.
// Errors reading the forge are not cached; a workflow the planner rejects is.
func (b *Broker) plan(ctx context.Context, sf core.StatusForge, r *repoState, sha string) ([]core.CheckJob, error) {
	key := r.key() + "@" + sha
	b.mu.Lock()
	if jobs, ok := b.plans[key]; ok {
		b.mu.Unlock()
		return jobs, nil
	}
	b.mu.Unlock()
	paths := append([]string(nil), r.cfg.Workflows...)
	if len(paths) == 0 && r.kind == "gitlab" {
		paths = []string{".gitlab-ci.yml"}
	}
	if len(paths) == 0 {
		for _, dir := range []string{".github/workflows", ".forgejo/workflows"} {
			fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
			names, err := sf.ListDir(fctx, r.cfg.Name, sha, dir)
			cancel()
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			for _, n := range names {
				p := n
				if !strings.HasPrefix(n, dir+"/") {
					p = dir + "/" + strings.TrimPrefix(n, "/")
				}
				if strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml") {
					paths = append(paths, p)
				}
			}
			if len(paths) > 0 {
				break
			}
		}
	}
	sort.Strings(paths)
	var out []core.CheckJob
	seen := map[string]bool{}
	for _, p := range paths {
		fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
		src, err := sf.FileAt(fctx, r.cfg.Name, sha, p)
		cancel()
		if errors.Is(err, core.ErrNotFound) {
			b.log.Info("workflow not found at sha", "repo", r.cfg.Name, "sha", short(sha), "workflow", p)
			continue
		}
		if err != nil {
			return nil, err
		}
		jobs, err := b.o.Planner(p, src, r.kind)
		if err != nil {
			b.log.Warn("workflow not planned", "repo", r.cfg.Name, "sha", short(sha), "workflow", p, "error", err)
			continue
		}
		for _, j := range jobs {
			c := CheckContext(j)
			if seen[c] {
				b.log.Warn("two jobs share a status context; running the first", "repo", r.cfg.Name, "context", c)
				continue
			}
			seen[c] = true
			if j.Workflow == "" {
				j.Workflow = p
			}
			out = append(out, j)
		}
	}
	b.mu.Lock()
	b.plans[key] = out
	b.mu.Unlock()
	return out, nil
}

func (b *Broker) setHeadStatusLocked(forge, repo, sha, ctxName, state string) {
	for _, r := range b.repos {
		if r.forge != forge || r.cfg.Name != repo {
			continue
		}
		for _, h := range r.heads {
			if h.commit.SHA == sha {
				h.statuses[ctxName] = state
			}
		}
	}
}

func (b *Broker) queuePost(rec *grantRec, state, desc string, now time.Time) {
	b.posts = append(b.posts, &statusPost{Forge: rec.Forge, Repo: rec.Repo, SHA: rec.SHA, Context: rec.Context,
		State: state, Description: truncate(desc, 140), Queued: now})
	b.setHeadStatusLocked(rec.Forge, rec.Repo, rec.SHA, rec.Context, state)
}

// flushPosts posts queued final statuses; failures stay queued (and in the
// state file) and are retried every pass for a day.
func (b *Broker) flushPosts(ctx context.Context, now time.Time) {
	b.mu.Lock()
	var todo []*statusPost
	for _, p := range b.posts {
		if !p.inflight {
			p.inflight = true
			todo = append(todo, p)
		}
	}
	b.mu.Unlock()
	for _, p := range todo {
		sf := b.o.StatusForges[p.Forge]
		var err error
		if sf == nil {
			err = fmt.Errorf("no status forge named %q", p.Forge)
		} else {
			fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
			err = sf.PostStatus(fctx, p.Repo, p.SHA, p.Context, p.State, p.Description, "")
			cancel()
		}
		b.mu.Lock()
		p.inflight = false
		done := err == nil
		if err != nil {
			p.Attempts++
			if now.Sub(p.Queued) >= postGiveUp {
				done = true
				b.log.Error("status never posted; giving up", "repo", p.Repo, "sha", p.SHA, "context", p.Context, "state", p.State)
			}
		} else {
			b.log.Info("status posted", "repo", p.Repo, "sha", short(p.SHA), "context", p.Context, "state", p.State)
		}
		if done {
			for i, q := range b.posts {
				if q == p {
					b.posts = append(b.posts[:i], b.posts[i+1:]...)
					break
				}
			}
		}
		b.mu.Unlock()
		if err != nil {
			b.noteErr(p.Repo, "post status", err)
		}
	}
}

// ---------------------------------------------------------------- shutdown, state, status

// Shutdown stops granting and deletes the runners that have not taken a job
// (nobody watches them while this broker is down). Busy runners keep going.
func (b *Broker) Shutdown(ctx context.Context) {
	b.tickMu.Lock()
	defer b.tickMu.Unlock()
	b.mu.Lock()
	b.closed = true
	b.archives = map[string]*archiveRec{}
	var idle []*grantRec
	for _, rec := range b.grants {
		if rec.Kind == core.KindRunner && !rec.pending && !rec.Busy && !rec.Ended && !rec.Settled {
			idle = append(idle, rec)
		}
	}
	b.mu.Unlock()
	for _, rec := range idle {
		f := b.o.Forges[rec.Forge]
		if f == nil {
			continue
		}
		b.mu.Lock()
		r := *rec
		b.mu.Unlock()
		// A last look: a runner that just took a job stays.
		fctx, cancel := context.WithTimeout(ctx, forgeTimeout)
		st, err := f.Runner(fctx, r.Repo, r.RunnerID)
		cancel()
		if err == nil && st.Busy {
			b.mu.Lock()
			rec.Busy, rec.BusySince = true, b.now()
			b.mu.Unlock()
			continue
		}
		b.removeIdle(ctx, f, rec, r, "broker shutting down")
	}
	b.flushPosts(ctx, b.now())
	if err := b.save(); err != nil {
		b.log.Error("broker state not saved", "error", err)
	}
	b.log.Info("broker stopped")
}

func (b *Broker) load(now time.Time) {
	if b.statePath == "" {
		return
	}
	data, err := os.ReadFile(b.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		b.log.Error("broker state unreadable; starting empty", "error", err)
		return
	}
	var f brokerFile
	if err := json.Unmarshal(data, &f); err != nil {
		b.log.Error("broker state corrupt; starting empty (unknown runners will be stopped)", "error", err)
		return
	}
	for _, g := range f.Grants {
		if g != nil && g.Name != "" {
			b.grants[g.Name] = g
		}
	}
	for _, d := range f.Devices {
		if d != nil && d.Name != "" {
			// A broker restart is not the device's silence: start its clock now.
			d.LastSeen, d.Offline = now, false
			b.devices[d.Name] = d
		}
	}
	for _, p := range f.Posts {
		if p != nil {
			b.posts = append(b.posts, p)
		}
	}
	b.lastSaved = data
	b.log.Info("broker state loaded", "runners", len(b.grants), "devices", len(b.devices), "posts", len(b.posts))
}

func (b *Broker) save() error {
	if b.statePath == "" {
		return nil
	}
	b.saveMu.Lock()
	defer b.saveMu.Unlock()
	b.mu.Lock()
	f := brokerFile{Version: 1, Grants: []*grantRec{}, Devices: []*deviceRec{}, Posts: []*statusPost{}}
	for _, g := range b.grants {
		if !g.pending {
			cp := *g
			f.Grants = append(f.Grants, &cp)
		}
	}
	sort.Slice(f.Grants, func(i, k int) bool { return f.Grants[i].Name < f.Grants[k].Name })
	for _, d := range b.devices {
		cp := *d
		f.Devices = append(f.Devices, &cp)
	}
	sort.Slice(f.Devices, func(i, k int) bool { return f.Devices[i].Name < f.Devices[k].Name })
	for _, p := range b.posts {
		cp := *p
		f.Posts = append(f.Posts, &cp)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	same := bytes.Equal(data, b.lastSaved)
	b.mu.Unlock()
	if err != nil || same {
		return err
	}
	if err := config.WriteAtomic(b.statePath, data, 0o600); err != nil {
		return err
	}
	b.mu.Lock()
	b.lastSaved = data
	b.mu.Unlock()
	return nil
}

// BrokerStatus is the broker's view for the CLI and MCP.
type BrokerStatus struct {
	Name            string         `json:"name"`
	ShuttingDown    bool           `json:"shutting_down"`
	Repos           []RepoStatus   `json:"repos"`
	Runners         []GrantStatus  `json:"runners"`
	Devices         []DeviceStatus `json:"devices"`
	PendingStatuses int            `json:"pending_statuses"`
	LastErrors      []ErrorEntry   `json:"last_errors"`
	// API counts each forge's API calls since start, by forge name, for
	// forges that count them (core.APICounter).
	API map[string]core.APIStats `json:"api,omitempty"`
}

// RepoStatus is one repo's polling health.
type RepoStatus struct {
	Forge     string    `json:"forge"`
	Name      string    `json:"name"`
	Mode      string    `json:"mode"`
	Denied    bool      `json:"denied"`
	Refused   string    `json:"refused,omitempty"`
	Healthy   bool      `json:"healthy"`
	Queued    int       `json:"queued"`  // runner mode: jobs in the last poll
	Pending   int       `json:"pending"` // checks mode: planned jobs without a status
	LastPoll  time.Time `json:"last_poll"`
	NextPoll  time.Time `json:"next_poll"`
	LastError string    `json:"last_error,omitempty"`
}

// GrantStatus is one granted runner.
type GrantStatus struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Device   string    `json:"device"`
	Repo     string    `json:"repo"`
	Job      string    `json:"job"`
	JobID    int64     `json:"job_id,omitempty"`
	RunnerID int64     `json:"runner_id,omitempty"`
	Busy     bool      `json:"busy"`
	TookJob  int64     `json:"took_job,omitempty"`
	Granted  time.Time `json:"granted"`
	Started  time.Time `json:"started,omitempty"`
	Ended    bool      `json:"ended"`
	SHA      string    `json:"sha,omitempty"`
	Context  string    `json:"context,omitempty"`
}

// DeviceStatus is one worker as the broker sees it.
type DeviceStatus struct {
	Name        string    `json:"name"`
	LastSeen    time.Time `json:"last_seen"`
	Online      bool      `json:"online"`
	Quarantined bool      `json:"quarantined"`
	Why         string    `json:"why,omitempty"`
	Runners     int       `json:"runners"`
}

// Status reports the broker's repos, runners and devices.
func (b *Broker) Status() BrokerStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	s := BrokerStatus{Name: b.name, ShuttingDown: b.closed, PendingStatuses: len(b.posts), LastErrors: b.errs.list(),
		Repos: []RepoStatus{}, Runners: []GrantStatus{}, Devices: []DeviceStatus{}}
	for _, r := range b.repos {
		rs := RepoStatus{Forge: r.forge, Name: r.cfg.Name, Mode: r.mode, Denied: r.denied, LastPoll: r.lastPoll,
			NextPoll: r.next, LastError: r.lastErr, Queued: len(r.queued)}
		if r.mode == "runner" && !r.denied && !r.lastPoll.IsZero() {
			rs.Refused = b.repoRefusalLocked(r, now)
		}
		for _, h := range r.heads {
			for _, j := range h.jobs {
				if _, ok := h.statuses[CheckContext(j)]; !ok {
					rs.Pending++
				}
			}
		}
		rs.Healthy = !r.denied && r.lastErr == "" && rs.Refused == ""
		s.Repos = append(s.Repos, rs)
	}
	count := map[string]int{}
	for _, g := range b.grants {
		count[g.Device]++
		s.Runners = append(s.Runners, GrantStatus{Name: g.Name, Kind: g.Kind, Device: g.Device, Repo: g.Repo,
			Job: g.Job.Name, JobID: g.Job.ID, RunnerID: g.RunnerID, Busy: g.Busy, TookJob: g.TookJob,
			Granted: g.Granted, Started: g.Started, Ended: g.Ended, SHA: g.SHA, Context: g.Context})
	}
	sort.Slice(s.Runners, func(i, k int) bool { return s.Runners[i].Name < s.Runners[k].Name })
	for _, d := range b.devices {
		s.Devices = append(s.Devices, DeviceStatus{Name: d.Name, LastSeen: d.LastSeen, Online: !d.Offline,
			Quarantined: d.Quarantine, Why: d.QuarantineWhy, Runners: count[d.Name]})
	}
	sort.Slice(s.Devices, func(i, k int) bool { return s.Devices[i].Name < s.Devices[k].Name })
	s.API = b.apiStats()
	return s
}

// apiStats reads every counting forge once, by name.
func (b *Broker) apiStats() map[string]core.APIStats {
	out := map[string]core.APIStats{}
	add := func(name string, f any) {
		if ac, ok := f.(core.APICounter); ok {
			if _, done := out[name]; !done {
				out[name] = ac.APIStats()
			}
		}
	}
	for n, f := range b.o.Forges {
		add(n, f)
	}
	for n, f := range b.o.StatusForges {
		add(n, f)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------- small helpers

// ErrorEntry is one recent error, for status.
type ErrorEntry struct {
	At    time.Time `json:"at"`
	Where string    `json:"where"`
	What  string    `json:"what"`
}

type errorLog struct{ items []ErrorEntry }

func (l *errorLog) add(at time.Time, where, what string) {
	l.items = append(l.items, ErrorEntry{At: at, Where: where, What: truncate(what, 300)})
	if len(l.items) > 20 {
		l.items = l.items[len(l.items)-20:]
	}
}

func (l *errorLog) list() []ErrorEntry { return append([]ErrorEntry{}, l.items...) }

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func dedupe(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

func short(sha string) string { return truncate(sha, 12) }

// ---------------------------------------------------------------- check logs

// MaxCheckLogs is how many finished check logs a broker keeps under
// <state>/checks; the oldest go first.
const MaxCheckLogs = 200

type checkLog struct {
	name string
	data []byte
}

var unsafeLogName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// newCheckLog is a finished check's log tail with a short header, named
// <owner>_<repo>-<sha12>-<job>.log so a commit status leads to its file.
func newCheckLog(rec *grantRec, state, desc, tail string) checkLog {
	job := strings.TrimPrefix(rec.Context, "tentmaker/")
	name := unsafeLogName.ReplaceAllString(rec.Repo, "_") + "-" + unsafeLogName.ReplaceAllString(short(rec.SHA), "_") + "-" + unsafeLogName.ReplaceAllString(job, "_") + ".log"
	head := fmt.Sprintf("repo: %s\nsha: %s\ncheck: %s\nstate: %s (%s)\ndevice: %s\nrunner: %s\n\n",
		rec.Repo, rec.SHA, rec.Context, state, desc, rec.Device, rec.Name)
	return checkLog{name: name, data: append([]byte(head), tail...)}
}

// saveCheckLogs writes finished checks' logs to <state>/checks, root-only,
// and prunes the oldest past MaxCheckLogs. A failure is logged, never fatal.
func (b *Broker) saveCheckLogs(logs []checkLog) {
	if len(logs) == 0 || b.o.StateDir == "" {
		return
	}
	dir := filepath.Join(b.o.StateDir, "checks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.log.Warn("check log not saved", "error", err)
		return
	}
	for _, l := range logs {
		if err := config.WriteAtomic(filepath.Join(dir, l.name), l.data, 0o600); err != nil {
			b.log.Warn("check log not saved", "file", l.name, "error", err)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) <= MaxCheckLogs {
		return
	}
	type aged struct {
		name string
		mod  time.Time
	}
	var all []aged
	for _, e := range ents {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			all = append(all, aged{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	for i := 0; i < len(all)-MaxCheckLogs; i++ {
		os.Remove(filepath.Join(dir, all[i].name))
	}
}
