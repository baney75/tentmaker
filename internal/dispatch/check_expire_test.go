package dispatch

import (
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

func (r *rig) archiveCount() int {
	r.broker.mu.Lock()
	defer r.broker.mu.Unlock()
	return len(r.broker.archives)
}

// A check grant whose Work response never reached the worker used to stay
// pending forever: the worker never runs it, and only started grants count
// as ended when omitted. It expires after the start window, its archive ref
// is revoked, the check gets an error status, and it is offered again
// (review r1 partA sol-F5).
func TestUnstartedCheckGrantExpires(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	w := r.addWorker("alpha", 1)
	src := &source{inner: r.broker, loseOffers: 1}
	w.sources = []core.WorkSource{src}
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main"}})

	r.tick()
	if n := len(started(w.ex)); n != 0 {
		t.Fatalf("starts = %d; the offer was lost", n)
	}
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "pending" {
		t.Fatalf("status = %q, want pending", got)
	}
	if r.archiveCount() != 1 {
		t.Fatalf("archives = %d, want the lost grant's", r.archiveCount())
	}
	r.run(14*time.Minute, 5*time.Second)
	if n := len(started(w.ex)); n != 0 {
		t.Fatalf("offered again before the start window ended (%d starts)", n)
	}

	r.run(2*time.Minute, 5*time.Second)
	sawError := false
	for _, p := range r.forge.Posts() {
		if p.State == "error" && strings.Contains(p.Description, "never started") {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("no error status for the lost grant; posts %+v", r.forge.Posts())
	}
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v; the check should be offered again", names)
	}
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "pending" {
		t.Fatalf("status = %q after the new offer, want pending", got)
	}
	if r.archiveCount() != 1 {
		t.Fatalf("archives = %d; the lost grant's ref must be revoked", r.archiveCount())
	}

	w.ex.SetResult(names[0], core.CheckResult{State: "success", Description: "1 step passed"})
	w.ex.Exit(names[0])
	r.run(6*time.Second, 2*time.Second)
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "success" {
		t.Fatalf("status = %q, want success", got)
	}
}

// A check whose offers keep getting lost is offered again twice, then left
// with its error status.
func TestUnstartedCheckGrantRetriesAreBounded(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	w := r.addWorker("alpha", 1)
	src := &source{inner: r.broker, loseOffers: 100}
	w.sources = []core.WorkSource{src}
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main"}})
	r.tick()
	r.run(80*time.Minute, 10*time.Second)
	pending := 0
	for _, p := range r.forge.Posts() {
		if p.State == "pending" {
			pending++
		}
	}
	if pending != 3 {
		t.Fatalf("pending posts = %d, want 3 (the first offer and 2 more)", pending)
	}
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
	if n := len(r.broker.Status().Runners); n != 0 || r.archiveCount() != 0 {
		t.Fatalf("broker still holds %d grants, %d archives", n, r.archiveCount())
	}
}
