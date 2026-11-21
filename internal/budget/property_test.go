package budget

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/power"
)

// scenario is one random world: a config, a power reading, an idle
// reading, guard readings, a clock and a running count.
type scenario struct {
	cfg     config.Config
	power   power.Fake
	idle    power.FakeIdle
	probe   guards.Fake
	now     time.Time
	running int
}

func pick[T any](r *rand.Rand, xs ...T) T { return xs[r.Intn(len(xs))] }

func randScenario(r *rand.Rand) scenario {
	sizes := []config.Size{0, 1, 512 * config.MiB, config.GiB, 2 * config.GiB, 3 * config.GiB, 4 * config.GiB, 7 * config.GiB, 16 * config.GiB, config.MaxSize}
	cpus := []float64{0, 0.5, 1, 1.5, 2, 3, 4, 8, 64, -1, math.NaN(), math.Inf(1)}
	c := config.Default(16<<30, 8)
	c.Budget.Everyday = config.Profile{Memory: pick(r, sizes...), CPUs: pick(r, cpus...)}
	c.Budget.Boost = config.Profile{Memory: pick(r, sizes...), CPUs: pick(r, cpus...)}
	c.Budget.BoostNeedsIdle = r.Intn(2) == 0
	c.Budget.IdleAfter = config.Duration(time.Duration(r.Intn(30)) * time.Minute)
	c.Budget.OnBattery = pick(r, "", "everyday", "off", "OFF", "bogus")
	c.Budget.PauseBelow = r.Intn(110) - 5
	c.Budget.StopBelow = r.Intn(110) - 5
	for n := r.Intn(3); n > 0; n-- {
		c.Budget.Schedule = append(c.Budget.Schedule, config.Window{
			Name:    "w",
			Days:    pick(r, []string(nil), []string{"mon"}, []string{"sat", "sun"}, []string{"bogus"}),
			From:    pick(r, "00:00", "07:00", "22:00", "12:30", "25:00", ""),
			To:      pick(r, "00:00", "07:00", "22:00", "12:30", "bad"),
			Profile: pick(r, "everyday", "boost", "off", "Boost", "nope"),
		})
	}
	c.Job = config.Job{Memory: pick(r, sizes...), CPUs: pick(r, cpus...), MaxSlots: r.Intn(5) - 1}
	c.Guards = config.Guards{
		MinFreeDisk:   pick(r, 0, config.GiB, 20*config.GiB),
		StopBelowDisk: pick(r, 0, config.GiB, 5*config.GiB),
		MinFreeMemory: pick(r, 0, config.GiB, 2*config.GiB),
		MaxLoadPerCPU: pick(r, 0.0, 0.5, 2),
	}
	if r.Intn(3) == 0 {
		c.Guards.PauseWhenRunning = []string{"steam.exe"}
	}
	errs := []error{nil, nil, nil, errors.New("probe failed")}
	return scenario{
		cfg: c,
		power: power.Fake{
			State: core.PowerState{Known: r.Intn(6) != 0, OnAC: r.Intn(2) == 0, HasBattery: r.Intn(3) != 0, BatteryPercent: r.Intn(101)},
			Err:   pick(r, errs...),
		},
		idle:  power.FakeIdle{Idle: time.Duration(r.Intn(60)-5) * time.Minute, Err: pick(r, errs...)},
		probe: guards.Fake{
			Memory: int64(pick(r, 0, config.GiB, 4*config.GiB, 64*config.GiB)),
			Disk:   int64(pick(r, 0, 4*config.GiB, 100*config.GiB)),
			Load:   pick(r, 0.1, 1.0, 3),
			Procs:  pick(r, []string{"init"}, []string{"Steam.exe"}),
			MemErr: pick(r, errs...), DiskErr: pick(r, errs...), LoadErr: pick(r, errs...), ProcErr: pick(r, errs...),
		},
		now:     time.Date(2026, 11, 16+r.Intn(7), r.Intn(24), r.Intn(60), 0, 0, zone),
		running: r.Intn(10) - 1,
	}
}

func floorDiv(a int64, b config.Size) int64 { return a / int64(b) }

// checkDecision asserts the budget's invariants for one scenario.
func checkDecision(t *testing.T, s scenario) {
	t.Helper()
	d := New(s.cfg, s.power, s.idle, s.probe).WithLocation(zone).Decide(context.Background(), s.now, s.running)
	fail := func(what string) {
		t.Helper()
		t.Fatalf("%s\ndecision: %+v\nscenario: %+v", what, d, s)
	}
	if d.Slots < 0 {
		fail("negative slots")
	}
	if d.Slots > 0 {
		j := s.cfg.Job
		if j.Memory <= 0 || !(j.CPUs > 0) {
			fail("slots without a valid job size")
		}
		if int64(d.Slots) > floorDiv(d.Limit.MemoryBytes, j.Memory) {
			fail("slots exceed memory / job memory")
		}
		if float64(d.Slots) > math.Floor(d.Limit.CPUs/j.CPUs+1e-9) {
			fail("slots exceed CPUs / job CPUs")
		}
		if j.MaxSlots > 0 && d.Slots > j.MaxSlots {
			fail("slots exceed max_slots")
		}
	}
	switch d.Profile {
	case Everyday, Boost, Off:
	default:
		fail("unknown profile " + d.Profile)
	}
	if d.Profile == Off && (d.Slots != 0 || d.Limit != (core.Resources{})) {
		fail("off with resources")
	}
	ps := s.power.State
	powerKnown := s.power.Err == nil && ps.Known
	if !powerKnown && d.Admit {
		fail("admitted with unknown power")
	}
	if powerKnown && ps.HasBattery && !ps.OnAC && ps.BatteryPercent < s.cfg.Budget.StopBelow && !d.Evict {
		fail("battery below stop_below without evict")
	}
	g := s.probe
	guardErr := g.MemErr != nil || g.DiskErr != nil ||
		(s.cfg.Guards.MaxLoadPerCPU > 0 && g.LoadErr != nil) ||
		(len(s.cfg.Guards.PauseWhenRunning) > 0 && g.ProcErr != nil)
	if guardErr && d.Admit {
		fail("admitted with a guard error")
	}
	if d.Admit && d.Slots <= s.running {
		fail("admitted with every slot in use")
	}
	if d.Admit && d.Slots == 0 {
		fail("admitted with no slots")
	}
	if d.Evict && d.Admit {
		fail("admit and evict together")
	}
}

// TestDecideProperties checks the invariants over many random worlds.
func TestDecideProperties(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		checkDecision(t, randScenario(r))
	}
}

// FuzzDecide lets the fuzzer drive the random world.
func FuzzDecide(f *testing.F) {
	for _, seed := range []int64{0, 1, 2, 42, 1 << 40} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, seed int64) {
		checkDecision(t, randScenario(rand.New(rand.NewSource(seed))))
	})
}
