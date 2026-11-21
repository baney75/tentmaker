package dispatch

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
)

const (
	sha1 = "1111111111111111111111111111111111111111"
	sha2 = "2222222222222222222222222222222222222222"
	sha3 = "3333333333333333333333333333333333333333"
	sha4 = "4444444444444444444444444444444444444444"
	sha5 = "5555555555555555555555555555555555555555"
)

func checksRepo(name string, authors ...string) config.Repo {
	return config.Repo{Name: name, Mode: "checks", Authors: authors}
}

func bundleOf(t *testing.T, spec core.ExecSpec) core.CheckBundle {
	t.Helper()
	var b core.CheckBundle
	if err := json.Unmarshal(spec.Grant.Secret, &b); err != nil {
		t.Fatalf("check grant is not a CheckBundle: %v", err)
	}
	return b
}

func TestChecksModeEndToEnd(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	w := r.addWorker("alpha", 8)
	for _, sha := range []string{sha1, sha2, sha3, sha4, sha5} {
		r.forge.SetFile(repoSite, sha, ciWorkflow, []byte("test\nlint\nmac:macos\n"))
		r.forge.SetFile(repoSite, sha, ".github/workflows/README.md", []byte("not a workflow"))
	}
	r.forge.SetCommits(repoSite, []core.Commit{
		{SHA: sha1, Ref: "main", Author: "someone"},
		{SHA: sha2, Ref: "feature", PR: 7, Author: "baney75"},
		{SHA: sha5, Ref: "feature", PR: 7, Author: "baney75"}, // an older head of PR 7
		{SHA: sha3, Ref: "mallory", PR: 8, Author: "mallory"},
		{SHA: sha4, Ref: "fork", PR: 9, Author: "baney75", FromFork: true},
	})
	r.forge.SetStatus(repoSite, sha1, "tentmaker/lint", "success")

	r.tick()
	names := started(w.ex)
	if len(names) != 3 {
		t.Fatalf("starts = %v, want sha1/test, sha2/test, sha2/lint", names)
	}
	type key struct{ sha, job string }
	byName := map[string]key{}
	for _, n := range names {
		spec, _ := w.ex.Spec(n)
		if spec.Kind != core.KindCheck || spec.Platform != linuxX64 {
			t.Fatalf("spec = %+v", spec)
		}
		b := bundleOf(t, spec)
		if !strings.Contains(b.ArchiveURL, r.forge.ArchiveToken()) || b.Repo != repoSite || b.Job.Workflow != ciWorkflow {
			t.Fatalf("bundle = %+v", b)
		}
		byName[n] = key{b.SHA, b.Job.Name}
	}
	want := map[key]bool{{sha1, "test"}: true, {sha2, "test"}: true, {sha2, "lint"}: true}
	for n, k := range byName {
		if !want[k] {
			t.Fatalf("ran %v (%s); fork, other author, older head, mac and statused jobs must not run", k, n)
		}
	}
	for _, k := range []key{{sha1, "test"}, {sha2, "test"}, {sha2, "lint"}} {
		if got := r.forge.Status(repoSite, k.sha, "tentmaker/"+k.job); got != "pending" {
			t.Fatalf("%v status = %q, want pending", k, got)
		}
	}
	for _, p := range r.forge.Posts() {
		if p.State == "pending" && p.Description != "running on alpha" {
			t.Fatalf("pending description %q", p.Description)
		}
	}

	for n, k := range byName {
		switch {
		case k == key{sha2, "lint"}:
			// No result.json: the worker reports an error.
		case k == key{sha2, "test"}:
			w.ex.SetResult(n, core.CheckResult{State: "failure", Description: "make test failed"})
		default:
			w.ex.SetResult(n, core.CheckResult{State: "success", Description: "1 step passed"})
		}
		w.ex.Exit(n)
	}
	r.run(2*time.Second, 2*time.Second)
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "success" {
		t.Fatalf("sha1 test = %q", got)
	}
	if got := r.forge.Status(repoSite, sha2, "tentmaker/test"); got != "failure" {
		t.Fatalf("sha2 test = %q", got)
	}
	if got := r.forge.Status(repoSite, sha2, "tentmaker/lint"); got != "error" {
		t.Fatalf("sha2 lint = %q", got)
	}
	r.run(time.Minute, 2*time.Second)
	if n := len(started(w.ex)); n != 3 {
		t.Fatalf("starts = %d; finished checks must not run again", n)
	}
	if n := r.planner.total(); n != 2 {
		t.Fatalf("planner ran %d times; plans are cached per sha", n)
	}
	logs := r.logs.String()
	for _, s := range []string{"author mallory is not in authors", "from a fork"} {
		if !strings.Contains(logs, s) {
			t.Errorf("logs miss %q", s)
		}
	}
	for _, s := range []string{sha3, sha4} {
		for _, p := range r.forge.Posts() {
			if p.SHA == s {
				t.Fatalf("posted a status on %s", s)
			}
		}
	}
	r.assertNoSecrets()
}

func TestChecksAuthorsListAndForgejoDir(t *testing.T) {
	r := newRig(t, checksRepo(repoSite, "friend"))
	w := r.addWorker("alpha", 4)
	r.forge.SetFile(repoSite, sha1, ".forgejo/workflows/ci.yaml", []byte("test\n"))
	r.forge.SetFile(repoSite, sha2, ".forgejo/workflows/ci.yaml", []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{
		{SHA: sha1, Ref: "a", PR: 1, Author: "Friend"},
		{SHA: sha2, Ref: "b", PR: 2, Author: "baney75"}, // the owner is not in an explicit list
	})
	r.tick()
	names := started(w.ex)
	if len(names) != 1 {
		t.Fatalf("starts = %v", names)
	}
	spec, _ := w.ex.Spec(names[0])
	if b := bundleOf(t, spec); b.SHA != sha1 || b.Job.Workflow != ".forgejo/workflows/ci.yaml" {
		t.Fatalf("bundle = %+v", b)
	}
}

func TestChecksBranches(t *testing.T) {
	repo := checksRepo(repoSite)
	repo.Branches = []string{"release", "e2e", "gone"}
	r := newRig(t, repo)
	w := r.addWorker("alpha", 8)
	for _, sha := range []string{sha1, sha2, sha3} {
		r.forge.SetFile(repoSite, sha, ciWorkflow, []byte("test\n"))
	}
	r.forge.SetCommits(repoSite, []core.Commit{
		{SHA: sha2, Ref: "e2e", PR: 4, Author: "baney75"}, // the PR head is also branch e2e's head
		{SHA: sha1, Ref: "main"},
	})
	r.forge.SetBranch(repoSite, "release", sha3)
	r.forge.SetBranch(repoSite, "e2e", sha2)
	r.forge.SetBranch(repoSite, "other", sha4) // not listed, so never asked for
	r.tick()
	got := map[string]int{}
	for _, n := range started(w.ex) {
		spec, _ := w.ex.Spec(n)
		got[bundleOf(t, spec).SHA]++
	}
	want := map[string]int{sha1: 1, sha2: 1, sha3: 1}
	if len(got) != len(want) || got[sha1] != 1 || got[sha2] != 1 || got[sha3] != 1 {
		t.Fatalf("started per sha = %v, want one each for main, the PR (also branch e2e) and branch release", got)
	}
}

func TestChecksWorkerSilentGetsErrorStatus(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	w := r.addWorker("alpha", 1)
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main"}})
	r.tick()
	name := started(w.ex)[0]
	w.silent = true
	r.run(110*time.Second, 5*time.Second)
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "pending" {
		t.Fatalf("status before 2 min = %q", got)
	}
	r.run(20*time.Second, 5*time.Second)
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "error" {
		t.Fatalf("status = %q, want error", got)
	}
	posts := r.forge.Posts()
	if last := posts[len(posts)-1]; last.Description != "device went offline" {
		t.Fatalf("last post = %+v", last)
	}
	w.silent = false
	r.run(6*time.Second, 2*time.Second)
	if !called(w.ex, "Stop", name) {
		t.Fatalf("returning worker kept running a check the broker gave up on")
	}
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("check ran again (%d starts) after its error status", n)
	}
}

// A result the broker never acknowledged survives a worker restart.
func TestUnackedResultSurvivesRestart(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	w := r.addWorker("alpha", 1)
	src := &source{inner: r.broker}
	w.sources = []core.WorkSource{src}
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main"}})
	r.tick()
	name := started(w.ex)[0]
	src.setFail(errors.New("mesh down"))
	w.ex.SetResult(name, core.CheckResult{State: "success", Description: "ok"})
	w.ex.Exit(name)
	r.run(4*time.Second, 2*time.Second)
	if st := w.w.Status(); st.PendingReports == 0 {
		t.Fatalf("no pending report while the source is down")
	}
	w.w = r.newWorker(w) // restart
	src.setFail(nil)
	r.run(2*time.Second, 2*time.Second)
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "success" {
		t.Fatalf("status = %q after restart", got)
	}
	if st := w.w.Status(); st.PendingReports != 0 {
		t.Fatalf("pending reports = %d after delivery", st.PendingReports)
	}
}

func TestChecksStatusPostRetried(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	w := r.addWorker("alpha", 1)
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main"}})
	r.tick()
	name := started(w.ex)[0]
	r.forge.FailTimes(fakeforge.MPostStatus, repoSite, 3, errBoom)
	w.ex.SetResult(name, core.CheckResult{State: "success", Description: "ok"})
	w.ex.Exit(name)
	r.run(10*time.Second, 2*time.Second)
	if got := r.forge.Status(repoSite, sha1, "tentmaker/test"); got != "success" {
		t.Fatalf("status = %q; a failed post must be retried", got)
	}
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("check ran again while its status post was pending")
	}
}

func TestSecretsNeverLogged(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp), checksRepo(repoSite))
	w := r.addWorker("alpha", 4)
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main"}})
	r.addJob(repoApp)
	r.addJob(repoApp)
	w.ex.FailStarts(nil, errors.New("vm did not boot"))
	r.tick()
	r.run(30*time.Second, 2*time.Second)
	for _, n := range started(w.ex) {
		w.ex.Exit(n)
	}
	r.run(2*time.Minute, 5*time.Second)
	logs := r.logs.String()
	for _, s := range []string{"runner granted", "check granted", "start failed"} {
		if !strings.Contains(logs, s) {
			t.Fatalf("logs miss %q; the capture is not seeing the flow", s)
		}
	}
	if len(r.forge.Grants()) == 0 {
		t.Fatal("no grants made")
	}
	r.assertNoSecrets()
	if strings.Contains(logs, "jit-") || strings.Contains(logs, "token=") {
		t.Fatalf("a secret shape appears in the logs")
	}
}
