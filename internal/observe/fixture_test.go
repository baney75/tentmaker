package observe

import (
	"encoding/json"
	"time"

	"github.com/baney75/tentmaker/internal/budget"
	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/runs"
)

// fixtureNow is the fixture's clock.
var fixtureNow = time.Date(2026, 11, 23, 18, 30, 0, 0, time.UTC)

// studio is a broker device on AC with boost, two jobs running and a
// GitHub API reading.
func studio() Device {
	now := fixtureNow
	return Device{
		Version: "0.4.0", Device: "studio", Pool: "home", StartedAt: now.Add(-6 * time.Hour),
		Why:      "boost: plugged in, idle 42m",
		Decision: core.Decision{Profile: budget.Boost, Admit: true, Slots: 4, Limit: core.Resources{CPUs: 8, MemoryBytes: int64(16 * config.GiB)}},
		Worker: &dispatch.WorkerStatus{
			Device: "studio", Slots: dispatch.SlotStatus{Total: 4, Used: 2, Free: 2},
			Running: []dispatch.RunningJob{
				{Name: "tm-studio-7", Repo: "baney75/tentmaker", Job: "test (linux)", Started: now.Add(-3 * time.Minute)},
				{Name: "tm-studio-8", Repo: "baney75/site", Job: "build", Started: now.Add(-40 * time.Second)},
			},
			Executors: []dispatch.ExecutorStatus{{Name: "linux-podman", Ready: true, MaxSlots: 4, InUse: 2}},
		},
		Broker: &dispatch.BrokerStatus{
			Name: "studio",
			Repos: []dispatch.RepoStatus{
				{Forge: "github", Name: "baney75/tentmaker", Mode: "runner", Healthy: true, Queued: 3},
				{Forge: "github", Name: "baney75/site", Mode: "runner", Healthy: true},
				{Forge: "gitlab", Name: "baney75/notes", Mode: "checks", Healthy: true, Pending: 1},
			},
			Devices: []dispatch.DeviceStatus{
				{Name: "laptop", Online: true, Runners: 1, LastSeen: now.Add(-5 * time.Second)},
				{Name: "studio", Online: true, Runners: 2, LastSeen: now},
			},
			API: map[string]core.APIStats{"github": {Since: now.Add(-6 * time.Hour), Requests: 5120, NotModified: 4300, Billed: 820,
				RateLeft: 4180, RateLimit: 5000, RateReset: now.Add(37 * time.Minute)}},
		},
	}
}

// laptop is on battery below pause_below_percent with the memory guard
// tripped.
func laptop() Device {
	return Device{
		Version: "0.4.0", Device: "laptop", Pool: "home", StartedAt: fixtureNow.Add(-2 * time.Hour),
		Why: "off: on battery at 31%, below 40%, free memory 1.2 GiB is below 2.5 GiB",
		Decision: core.Decision{Profile: budget.Off, Admit: false, Limit: core.Resources{},
			Guards: []string{budget.GuardBattery, guards.GuardMemory}},
		Worker: &dispatch.WorkerStatus{Device: "laptop", Slots: dispatch.SlotStatus{Total: 0, Used: 1},
			Running: []dispatch.RunningJob{{Name: "tm-laptop-2", Repo: "baney75/notes", Job: "check", Started: fixtureNow.Add(-9 * time.Minute)}}},
	}
}

func rawStatus(d Device) json.RawMessage {
	b, _ := json.Marshal(d)
	return b
}

// fixtureRuns is studio's and laptop's history.
func fixtureRuns() []runs.Record {
	now := fixtureNow
	rec := func(id, dev, repo, job, concl string, queued, started, ended time.Duration) runs.Record {
		r := runs.Record{ID: id, Kind: core.KindRunner, Forge: "github", Repo: repo, Job: job, Device: dev, Conclusion: concl,
			RunURL: "https://github.com/" + repo + "/actions/runs/1900" + id[len(id)-1:], LogSource: runs.LogForge}
		r.QueuedAt = now.Add(-queued)
		r.StartedAt = now.Add(-started)
		if ended > 0 {
			r.EndedAt = now.Add(-ended)
		}
		return r
	}
	ok := rec("tm-studio-1", "studio", "baney75/tentmaker", "test (linux)", runs.Success, 70*time.Minute, 69*time.Minute+57*time.Second, 64*time.Minute)
	ok.RepoCache, ok.ContainerStore = core.CacheHit, core.CacheHit
	cold := rec("tm-studio-2", "studio", "baney75/site", "build", runs.Success, 50*time.Minute, 49*time.Minute+48*time.Second, 46*time.Minute)
	cold.RepoCache, cold.ContainerStore = core.CacheMiss, core.CacheMiss
	fail := rec("tm-studio-3", "studio", "baney75/tentmaker", "lint", runs.Failure, 30*time.Minute, 29*time.Minute+58*time.Second, 27*time.Minute)
	fail.FailedStep, fail.RepoCache = "golangci-lint", core.CacheHit
	slow := rec("tm-studio-4", "studio", "baney75/tentmaker", "test (windows)", runs.Cancelled, 20*time.Minute, 15*time.Minute, 12*time.Minute)
	slow.Note = "stopped: the budget went off"
	// Ended before this daemon started: not counted.
	old := rec("tm-studio-5", "studio", "baney75/tentmaker", "test (linux)", runs.Failure, 8*time.Hour, 8*time.Hour-time.Minute, 7*time.Hour)
	// Still waiting for the forge's result: not counted yet.
	pending := rec("tm-studio-6", "studio", "baney75/site", "deploy", runs.Unknown, 3*time.Minute, 2*time.Minute, 30*time.Second)
	pending.LogSource = runs.LogRunnerDiag
	// Ran on laptop: counts on laptop's metrics, not studio's.
	lap := rec("tm-laptop-1", "laptop", "baney75/site", "e2e", runs.Failure, 90*time.Minute, 88*time.Minute, 80*time.Minute)
	lap.FailedStep = "playwright test"
	run1 := rec("tm-studio-7", "studio", "baney75/tentmaker", "test (linux)", runs.Running, 3*time.Minute+2*time.Second, 3*time.Minute, 0)
	run2 := rec("tm-studio-8", "studio", "baney75/site", "build", runs.Running, 45*time.Second, 40*time.Second, 0)
	run3 := rec("tm-laptop-2", "laptop", "baney75/notes", "check", runs.Running, 10*time.Minute, 9*time.Minute, 0)
	run3.Kind, run3.Forge, run3.RunURL = core.KindCheck, "gitlab", "https://gitlab.com/baney75/notes/-/pipelines/77"
	return []runs.Record{ok, cold, fail, slow, old, pending, lap, run1, run2, run3}
}

func fixtureMetrics() MetricsInput {
	return MetricsInput{Now: fixtureNow, Self: studio(), Runs: fixtureRuns()}
}

func fixturePage() PageInput {
	return PageInput{
		Now: fixtureNow, Self: "studio", Pool: "home",
		Devices: []mesh.DeviceStatus{
			{Device: "laptop", Version: "0.4.0", Status: rawStatus(laptop())},
			{Device: "studio", Self: true, Broker: true, Version: "0.4.0", Status: rawStatus(studio())},
			{Device: "nas", Version: "0.3.9", Error: "no answer in 2s"},
		},
		Runs: fixtureRuns(),
	}
}
