package dispatch

import (
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

func waitingKeys(b *Broker) []string {
	var out []string
	for _, j := range b.Waiting() {
		out = append(out, j.Key)
	}
	return out
}

func TestWaitingListsJobsNoAwakeDeviceTakes(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	j := r.addJob(repoApp)
	r.tick()
	// Queued before the broker started: it waits from the start.
	if got := waitingKeys(r.broker); len(got) != 0 {
		t.Fatalf("listed before WakeAfter: %v", got)
	}
	r.clk.Add(WakeAfter - time.Second)
	if got := waitingKeys(r.broker); len(got) != 0 {
		t.Fatalf("listed early: %v", got)
	}
	r.clk.Add(time.Second)
	got := r.broker.Waiting()
	if len(got) != 1 || got[0].Key != j.Key() || !got[0].Since.Equal(t0) || got[0].Repo != repoApp {
		t.Fatalf("waiting %+v", got)
	}
	if !got[0].Fits([]string{"self-hosted", "tentmaker"}, []core.Platform{linuxX64}) ||
		got[0].Fits([]string{"self-hosted", "tentmaker"}, []core.Platform{macosARM}) {
		t.Error("Fits")
	}

	// A macOS device with free slots can't take it: still waiting.
	r.addWorker("mac", 2, macosARM)
	r.tick()
	if got := waitingKeys(r.broker); len(got) != 1 {
		t.Fatalf("a device that can't run it hid it: %v", got)
	}
	// A full linux device can't take it now either.
	full := r.addWorker("full", 0)
	r.tick()
	if got := waitingKeys(r.broker); len(got) != 1 {
		t.Fatalf("a full device hid it: %v", got)
	}
	// A linux device with a free slot takes it: nothing waits.
	full.budget.set(admit(1))
	r.tick()
	if len(started(full.ex)) != 1 {
		t.Fatalf("not started: %v", started(full.ex))
	}
	if got := waitingKeys(r.broker); len(got) != 0 {
		t.Fatalf("a granted job still waits: %v", got)
	}
}

func TestWaitingSkipsRefusedJobs(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	r.addJob(repoApp, func(j *core.Job) { j.FromFork = true })
	r.addJob(repoApp, func(j *core.Job) { j.Labels = []string{"self-hosted", "linux"} })
	r.tick()
	r.clk.Add(time.Minute)
	if got := waitingKeys(r.broker); len(got) != 0 {
		t.Fatalf("refused jobs are waiting: %v", got)
	}
}

func TestWaitingCountsFromTheQueueTime(t *testing.T) {
	r := newRig(t, runnerRepo(repoApp))
	r.clk.Add(time.Minute)
	j := r.addJob(repoApp, func(j *core.Job) { j.CreatedAt = r.clk.Now() })
	r.tick()
	r.clk.Add(WakeAfter)
	got := r.broker.Waiting()
	if len(got) != 1 || got[0].Key != j.Key() || !got[0].Since.Equal(j.CreatedAt) {
		t.Fatalf("waiting %+v", got)
	}
}

func TestWaitingCheckJobs(t *testing.T) {
	r := newRig(t, checksRepo(repoSite))
	w := r.addWorker("alpha", 8)
	r.forge.SetFile(repoSite, sha1, ciWorkflow, []byte("test\nmac:macos\n"))
	r.forge.SetCommits(repoSite, []core.Commit{{SHA: sha1, Ref: "main", Author: "someone"}})
	r.tick()
	if n := len(started(w.ex)); n != 1 {
		t.Fatalf("started %d", n)
	}
	// First seen now; listed after WakeAfter.
	if got := waitingKeys(r.broker); len(got) != 0 {
		t.Fatalf("listed at once: %v", got)
	}
	r.clk.Add(WakeAfter)
	got := r.broker.Waiting()
	if len(got) != 1 || got[0].Name != "mac" || !got[0].Fits(WorkerLabels("mac", nil), []core.Platform{{OS: "macos", Arch: "x64"}}) || got[0].Fits(WorkerLabels("box", nil), []core.Platform{linuxX64}) {
		t.Fatalf("waiting %+v", got)
	}
}
