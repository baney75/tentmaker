package mesh

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
)

// TestQueueToWakeTime measures, on a fake clock, how long a job that only
// a sleeping device can run waits before the wake request goes out: a real
// broker polls a fake forge, and the mesh waker reads its Waiting list as
// the daemon does. Before Wake-on-LAN the job waited until someone woke
// the device by hand. It tries every phase of the 5 s wake tick against
// the queue time and checks the worst case: dispatch.WakeAfter plus one
// wake tick.
func TestQueueToWakeTime(t *testing.T) {
	var worst time.Duration
	for phase := time.Duration(0); phase < wakeTickEvery; phase += time.Second {
		got := queueToWake(t, phase)
		t.Logf("wake tick %v after the queue: wake request sent %v after the job was queued", phase, got)
		worst = max(worst, got)
	}
	if bound := dispatch.WakeAfter + wakeTickEvery; worst > bound {
		t.Fatalf("worst queue to wake %v, want at most %v", worst, bound)
	}
	t.Logf("worst case queue to wake request: %v", worst)
}

func queueToWake(t *testing.T, phase time.Duration) time.Duration {
	t.Helper()
	ctx := context.Background()
	var broker *dispatch.Broker
	needs := func() []WakeNeed {
		var out []WakeNeed
		for _, j := range broker.Waiting() {
			out = append(out, WakeNeed{Key: j.Key, Job: j.Repo + ": " + j.Name, Since: j.Since,
				Fits: func(l []string, p []core.Platform) bool { return j.Fits(l, p) }})
		}
		return out
	}
	snd := &fakeSender{}
	ms := newCluster(t, nodeSpec{device: "broker", pool: "home", broker: &fakeBroker{},
		wake: wakeInfo("a4:5e:60:00:00:09", homeSubnet, homeBcast), sender: snd, needs: needs})
	n, clk := ms[0].node, ms[0].clock

	forge := fakeforge.New("github", clk.Now)
	cfg := config.Default(16<<30, 8)
	cfg.Device = "broker"
	cfg.Repos = []config.Repo{{Name: "alice/app", Mode: "runner"}}
	broker = dispatch.NewBroker(dispatch.BrokerOptions{
		Config: cfg, Forges: map[string]core.Forge{"github": forge}, StateDir: t.TempDir(),
		Log: slog.New(slog.DiscardHandler), Now: clk.Now,
	})
	desk := wakeInfo(deskMAC, homeSubnet, homeBcast)
	desk.Labels = dispatch.WorkerLabels("desk", nil)
	asleep("desk", desk, clk.Now().Add(-time.Hour), n)
	warm(ms)
	if err := broker.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	queued := clk.Now()
	forge.AddJob(core.Job{Repo: "alice/app", Name: "test", Labels: []string{"self-hosted", "tentmaker", "linux"}, CreatedAt: queued})
	for step := time.Duration(1); step <= 120; step++ {
		clk.Add(time.Second)
		since := clk.Now().Sub(queued)
		if err := broker.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if since%(30*time.Second) == 0 {
			heartbeat(ms)
		}
		if (since-phase)%wakeTickEvery == 0 {
			n.WakeTick(ctx)
		}
		if p := snd.packets(); len(p) > 0 {
			if p[0].MAC != deskMAC {
				t.Fatalf("woke %+v", p)
			}
			return since
		}
	}
	t.Fatal("no wake within 2 minutes")
	return 0
}
