package budget

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/power"
)

const gib = int64(config.GiB)

// zone is the schedule's local time in these tests (UTC-6, no DST).
var zone = time.FixedZone("test", -6*3600)

// at returns a time in zone. 2026-11-22 is a Sunday, 2026-11-23 a Monday.
func at(day, hour, min int) time.Time {
	return time.Date(2026, 11, day, hour, min, 0, 0, zone)
}

var (
	sat  = 21
	sun  = 22
	mon  = 23
	noon = at(mon, 12, 0)

	desktop = power.Fake{State: core.PowerState{Known: true, OnAC: true}}
	plugged = func(p int) power.Fake {
		return power.Fake{State: core.PowerState{Known: true, OnAC: true, HasBattery: true, BatteryPercent: p}}
	}
	battery = func(p int) power.Fake {
		return power.Fake{State: core.PowerState{Known: true, HasBattery: true, BatteryPercent: p}}
	}
	away     = power.FakeIdle{Idle: time.Hour}
	healthy  = guards.Fake{Memory: 32 * gib, Disk: 500 * gib, Load: 0.1, Procs: []string{"init"}}
	everyday = core.Resources{MemoryBytes: 4 * gib, CPUs: 4}
	boost    = core.Resources{MemoryBytes: 8 * gib, CPUs: 8}
)

func baseConfig() config.Config {
	c := config.Default(32*gib, 16)
	c.StateDir = "/var/lib/tentmaker"
	c.Budget.Everyday = config.Profile{Memory: 4 * config.GiB, CPUs: 4}
	c.Budget.Boost = config.Profile{Memory: 8 * config.GiB, CPUs: 8}
	c.Budget.IdleAfter = config.Duration(10 * time.Minute)
	c.Budget.PauseBelow = 40
	c.Budget.StopBelow = 15
	c.Budget.OnBattery = "everyday"
	c.Job.Memory = 2 * config.GiB
	c.Job.CPUs = 2
	return c
}

func nights(profile string, days ...string) config.Window {
	return config.Window{Name: "nights", Days: days, From: "22:00", To: "07:00", Profile: profile}
}

type tc struct {
	name    string
	cfg     func(*config.Config)
	power   core.PowerSource
	idle    core.IdleSource
	probe   func(*guards.Fake)
	now     time.Time
	running int

	profile string
	limit   core.Resources
	slots   int
	admit   bool
	evict   bool
	full    bool
	reasons []string // each must appear in some reason, in order
	quiet   bool     // no reasons at all
}

func TestDecide(t *testing.T) {
	boom := errors.New("pmset timed out")
	tests := []tc{
		// Power sources.
		{name: "desktop with no battery boosts", power: desktop,
			profile: Boost, limit: boost, slots: 4, admit: true, quiet: true},
		{name: "desktop that reports OnAC false but no battery still boosts",
			power:   power.Fake{State: core.PowerState{Known: true}},
			profile: Boost, limit: boost, slots: 4, admit: true, quiet: true},
		{name: "laptop plugged in boosts", power: plugged(80),
			profile: Boost, limit: boost, slots: 4, admit: true, quiet: true},
		{name: "laptop plugged in with a low battery still boosts", power: plugged(5),
			profile: Boost, limit: boost, slots: 4, admit: true, quiet: true},
		{name: "laptop unplugged uses everyday", power: battery(80),
			profile: Everyday, limit: everyday, slots: 2, admit: true,
			reasons: []string{"on battery at 80%, so everyday instead of boost"}},
		{name: "laptop unplugged with on_battery off", power: battery(80),
			cfg:     func(c *config.Config) { c.Budget.OnBattery = "off" },
			profile: Off, slots: 0, admit: false,
			reasons: []string{"on battery at 80% and budget.on_battery is off"}},

		// Battery thresholds at their boundaries.
		{name: "battery at pause level runs", power: battery(40),
			profile: Everyday, limit: everyday, slots: 2, admit: true},
		{name: "battery one below pause level pauses", power: battery(39),
			profile: Off, slots: 0, admit: false, reasons: []string{"on battery at 39%, below 40%"}},
		{name: "battery 31 percent pauses", power: battery(31),
			profile: Off, slots: 0, admit: false, reasons: []string{"on battery at 31%, below 40%"}},
		{name: "battery at stop level pauses but keeps jobs", power: battery(15), running: 1,
			profile: Off, slots: 0, admit: false, evict: false, reasons: []string{"on battery at 15%, below 40%"}},
		{name: "battery one below stop level evicts", power: battery(14), running: 1,
			profile: Off, slots: 0, admit: false, evict: true,
			reasons: []string{"on battery at 14%, below 15%, stopping running jobs"}},
		{name: "battery at zero evicts", power: battery(0),
			profile: Off, admit: false, evict: true},
		{name: "thresholds of zero never pause", power: battery(0),
			cfg:     func(c *config.Config) { c.Budget.PauseBelow, c.Budget.StopBelow = 0, 0 },
			profile: Everyday, limit: everyday, slots: 2, admit: true},

		// Idle.
		{name: "idle required and met", power: desktop, idle: away,
			cfg:     func(c *config.Config) { c.Budget.BoostNeedsIdle = true },
			profile: Boost, limit: boost, slots: 4, admit: true, quiet: true},
		{name: "idle required and exactly met", power: desktop, idle: power.FakeIdle{Idle: 10 * time.Minute},
			cfg:     func(c *config.Config) { c.Budget.BoostNeedsIdle = true },
			profile: Boost, limit: boost, slots: 4, admit: true},
		{name: "idle required and not met", power: desktop, idle: power.FakeIdle{Idle: 2*time.Minute + 30*time.Second},
			cfg:     func(c *config.Config) { c.Budget.BoostNeedsIdle = true },
			profile: Everyday, limit: everyday, slots: 2, admit: true,
			reasons: []string{"plugged in, but boost waits until you have been away 10m (away 2m30s)"}},
		{name: "idle error means not idle", power: desktop, idle: power.FakeIdle{Idle: time.Hour, Err: errors.New("no session")},
			cfg:     func(c *config.Config) { c.Budget.BoostNeedsIdle = true },
			profile: Everyday, limit: everyday, slots: 2, admit: true,
			reasons: []string{"idle time is unknown (no session)"}},
		{name: "nil idle source means not idle", power: desktop, idle: nil,
			cfg:     func(c *config.Config) { c.Budget.BoostNeedsIdle = true },
			profile: Everyday, limit: everyday, slots: 2, admit: true},
		{name: "idle not needed ignores idle errors", power: desktop, idle: power.FakeIdle{Err: errors.New("x")},
			profile: Boost, limit: boost, slots: 4, admit: true},

		// Schedule.
		{name: "schedule off at night", power: desktop, now: at(mon, 23, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("off")} },
			profile: Off, slots: 0, admit: false, reasons: []string{`schedule "nights" (22:00 to 07:00) sets off`}},
		{name: "schedule off early morning", power: desktop, now: at(mon, 6, 59),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("off")} },
			profile: Off, admit: false},
		{name: "schedule ends at its To time", power: desktop, now: at(mon, 7, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("off")} },
			profile: Boost, limit: boost, slots: 4, admit: true, quiet: true},
		{name: "schedule starts at its From time", power: desktop, now: at(mon, 22, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("off")} },
			profile: Off, admit: false},
		{name: "schedule everyday on a desktop overrides boost", power: desktop, now: at(mon, 10, 0),
			cfg: func(c *config.Config) {
				c.Budget.Schedule = []config.Window{{Name: "work", Days: []string{"mon", "tue"}, From: "09:00", To: "17:00", Profile: "everyday"}}
			},
			profile: Everyday, limit: everyday, slots: 2, admit: true, reasons: []string{`schedule "work" (09:00 to 17:00) sets everyday`}},
		{name: "same-day window ends at its To time", power: desktop, now: at(mon, 17, 0),
			cfg: func(c *config.Config) {
				c.Budget.Schedule = []config.Window{{Name: "work", Days: []string{"mon"}, From: "09:00", To: "17:00", Profile: "everyday"}}
			},
			profile: Boost, limit: boost, slots: 4, admit: true, quiet: true},
		{name: "same-day window starts at its From time", power: desktop, now: at(mon, 9, 0),
			cfg: func(c *config.Config) {
				c.Budget.Schedule = []config.Window{{Name: "work", Days: []string{"mon"}, From: "09:00", To: "17:00", Profile: "everyday"}}
			},
			profile: Everyday, limit: everyday, slots: 2, admit: true},
		{name: "schedule on another day does not apply", power: desktop, now: at(sun, 10, 0),
			cfg: func(c *config.Config) {
				c.Budget.Schedule = []config.Window{{Name: "work", Days: []string{"mon", "tue"}, From: "09:00", To: "17:00", Profile: "everyday"}}
			},
			profile: Boost, limit: boost, slots: 4, admit: true},
		{name: "schedule boost on AC skips the idle wait", power: plugged(90), idle: power.FakeIdle{},
			now: at(mon, 23, 0),
			cfg: func(c *config.Config) {
				c.Budget.BoostNeedsIdle = true
				c.Budget.Schedule = []config.Window{nights("boost")}
			},
			profile: Boost, limit: boost, slots: 4, admit: true},
		{name: "schedule boost on battery falls back to everyday", power: battery(80), now: at(mon, 23, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("boost")} },
			profile: Everyday, limit: everyday, slots: 2, admit: true,
			reasons: []string{`schedule "nights"`, "boost needs AC power, but the machine is on battery at 80%, so everyday"}},
		{name: "schedule boost on battery with on_battery off", power: battery(80), now: at(mon, 23, 0),
			cfg: func(c *config.Config) {
				c.Budget.OnBattery = "off"
				c.Budget.Schedule = []config.Window{nights("boost")}
			},
			profile: Off, admit: false, reasons: []string{"so off"}},
		{name: "schedule can't beat a low battery", power: battery(30), now: at(mon, 23, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("boost")} },
			profile: Off, admit: false, reasons: []string{"on battery at 30%, below 40%"}},
		{name: "first matching window wins", power: desktop, now: at(mon, 23, 0),
			cfg: func(c *config.Config) {
				c.Budget.Schedule = []config.Window{
					{Name: "late", From: "23:00", To: "23:30", Profile: "everyday"},
					nights("off"),
				}
			},
			profile: Everyday, limit: everyday, slots: 2, admit: true, reasons: []string{`schedule "late"`}},
		{name: "a bad window is skipped", power: desktop, now: at(mon, 23, 0),
			cfg: func(c *config.Config) {
				c.Budget.Schedule = []config.Window{{Name: "bad", From: "25:00", To: "07:00", Profile: "off"}, nights("everyday")}
			},
			profile: Everyday, limit: everyday, slots: 2, admit: true, reasons: []string{`schedule "nights"`}},
		{name: "profile names are case-insensitive", power: desktop, now: at(mon, 23, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("Off", "Mon")} },
			profile: Off, admit: false},

		// A window that crosses midnight from Sunday into Monday.
		{name: "sunday window covers sunday night", power: desktop, now: at(sun, 22, 30),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("off", "sun")} },
			profile: Off, admit: false},
		{name: "sunday window covers monday early morning", power: desktop, now: at(mon, 3, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("off", "sun")} },
			profile: Off, admit: false},
		{name: "sunday window ends monday at 07:00", power: desktop, now: at(mon, 7, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("off", "sun")} },
			profile: Boost, admit: true, slots: 4, limit: boost},
		{name: "sunday window does not cover sunday early morning", power: desktop, now: at(sun, 3, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("off", "sun")} },
			profile: Boost, admit: true, slots: 4, limit: boost},
		{name: "sunday window does not cover monday night", power: desktop, now: at(mon, 23, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("off", "sun")} },
			profile: Boost, admit: true, slots: 4, limit: boost},
		{name: "saturday window covers sunday morning", power: desktop, now: at(sun, 1, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("off", "sat")} },
			profile: Off, admit: false},
		{name: "a saturday-night window started the day before", power: desktop, now: at(sat, 1, 0),
			cfg:     func(c *config.Config) { c.Budget.Schedule = []config.Window{nights("off", "sat")} },
			profile: Boost, admit: true, slots: 4, limit: boost},
		{name: "schedule reads local time, not UTC", power: desktop,
			// 04:30 UTC Monday is 22:30 Sunday in the test zone.
			now: time.Date(2026, 11, 23, 4, 30, 0, 0, time.UTC),
			cfg: func(c *config.Config) {
				c.Budget.Schedule = []config.Window{{Name: "sun-late", Days: []string{"sun"}, From: "22:00", To: "23:00", Profile: "off"}}
			},
			profile: Off, admit: false},
		{name: "a 24 hour window covers the whole day", power: desktop, now: at(mon, 13, 0),
			cfg: func(c *config.Config) {
				c.Budget.Schedule = []config.Window{{Name: "monday", Days: []string{"mon"}, From: "00:00", To: "00:00", Profile: "off"}}
			},
			profile: Off, admit: false},

		// Power read failures fail closed on new work and keep running jobs.
		{name: "power read error", power: power.Fake{Err: boom}, running: 1,
			profile: Everyday, limit: everyday, slots: 2, admit: false, evict: false,
			reasons: []string{"can't read power (pmset timed out), so no new jobs"}},
		{name: "power state unknown", power: power.Fake{State: core.PowerState{Known: false, OnAC: true}},
			profile: Everyday, limit: everyday, slots: 2, admit: false, reasons: []string{"can't read power (power state unknown)"}},
		{name: "nil power source", power: nil,
			profile: Everyday, limit: everyday, slots: 2, admit: false, reasons: []string{"can't read power (no power source)"}},
		{name: "power error still lets a disk guard evict", power: power.Fake{Err: boom},
			probe:   func(f *guards.Fake) { f.Disk = 1 * gib },
			profile: Everyday, admit: false, evict: true, slots: 2, limit: everyday,
			reasons: []string{"can't read power", "stopping running jobs"}},

		// Guards.
		{name: "guard memory low", power: desktop, probe: func(f *guards.Fake) { f.Memory = 3 * gib },
			profile: Boost, limit: boost, slots: 4, admit: false, reasons: []string{"free memory 3 GiB is below 4 GiB"}},
		{name: "guard game running", power: desktop,
			cfg:     func(c *config.Config) { c.Guards.PauseWhenRunning = []string{"steam.exe"} },
			probe:   func(f *guards.Fake) { f.Procs = []string{"steam.exe"} },
			profile: Everyday, limit: everyday, slots: 2, admit: true,
			reasons: []string{"steam.exe is running", "so the reserved everyday budget instead of boost"}},
		{name: "game running on everyday keeps the reserved budget", power: battery(80),
			cfg:     func(c *config.Config) { c.Guards.PauseWhenRunning = []string{"steam.exe"} },
			probe:   func(f *guards.Fake) { f.Procs = []string{"steam.exe"} },
			profile: Everyday, limit: everyday, slots: 2, admit: true,
			reasons: []string{"on battery at 80%", "steam.exe is running"}},
		{name: "busy load drops boost and full everyday slots refuse", power: desktop, running: 2,
			cfg:     func(c *config.Config) { c.Guards.MaxLoadPerCPU = 1 },
			probe:   func(f *guards.Fake) { f.Load = 3 },
			profile: Everyday, limit: everyday, slots: 2, admit: false, full: true,
			reasons: []string{"load 3.00 per CPU", "reserved everyday budget", "2 of 2 slots in use"}},
		{name: "busy and low memory still refuses", power: desktop,
			cfg:     func(c *config.Config) { c.Guards.PauseWhenRunning = []string{"zoom"} },
			probe:   func(f *guards.Fake) { f.Procs = []string{"zoom"}; f.Memory = 3 * gib },
			profile: Everyday, limit: everyday, slots: 2, admit: false,
			reasons: []string{"free memory", "zoom is running"}},
		{name: "guard probe error fails closed", power: desktop, probe: func(f *guards.Fake) { f.MemErr = errors.New("vm_stat timed out") },
			profile: Boost, limit: boost, slots: 4, admit: false, reasons: []string{"can't read free memory (vm_stat timed out)"}},
		{name: "guard disk below stop evicts", power: desktop, running: 2, probe: func(f *guards.Fake) { f.Disk = 4 * gib },
			profile: Boost, limit: boost, slots: 4, admit: false, evict: true, reasons: []string{"stopping running jobs"}},
		{name: "guard runs while off for battery and adds its reason", power: battery(30), probe: func(f *guards.Fake) { f.Memory = 1 },
			profile: Off, admit: false, reasons: []string{"on battery at 30%, below 40%", "free memory"}},

		// Slots.
		{name: "running below slots admits", power: desktop, running: 3,
			profile: Boost, limit: boost, slots: 4, admit: true, quiet: true},
		{name: "running equal to slots", power: desktop, running: 4,
			profile: Boost, limit: boost, slots: 4, admit: false, full: true, reasons: []string{"4 of 4 slots in use"}},
		{name: "running above slots after a downgrade", power: battery(80), running: 3,
			profile: Everyday, limit: everyday, slots: 2, admit: false, evict: false, full: true, reasons: []string{"3 of 2 slots in use"}},
		{name: "max_slots caps", power: desktop, cfg: func(c *config.Config) { c.Job.MaxSlots = 1 },
			profile: Boost, limit: boost, slots: 1, admit: true},
		{name: "cpu bound slots", power: desktop, cfg: func(c *config.Config) { c.Budget.Boost.CPUs = 3 },
			profile: Boost, limit: core.Resources{MemoryBytes: 8 * gib, CPUs: 3}, slots: 1, admit: true},
		{name: "fractional cpu slots", power: desktop, cfg: func(c *config.Config) { c.Job.CPUs = 1.5; c.Budget.Boost.CPUs = 4.5 },
			profile: Boost, limit: core.Resources{MemoryBytes: 8 * gib, CPUs: 4.5}, slots: 3, admit: true},
		{name: "boost with zero memory is everyday", power: desktop, cfg: func(c *config.Config) { c.Budget.Boost = config.Profile{} },
			profile: Everyday, limit: everyday, slots: 2, admit: true, reasons: []string{"boost is the same as everyday"}},
		{name: "boost with zero CPUs takes everyday CPUs", power: desktop, cfg: func(c *config.Config) { c.Budget.Boost.CPUs = 0 },
			profile: Boost, limit: core.Resources{MemoryBytes: 8 * gib, CPUs: 4}, slots: 2, admit: true},
		{name: "budget smaller than one job", power: battery(80), cfg: func(c *config.Config) { c.Budget.Everyday.Memory = config.GiB },
			profile: Everyday, limit: core.Resources{MemoryBytes: gib, CPUs: 4}, slots: 0, admit: false,
			reasons: []string{"the everyday budget (1 GiB, 4 CPUs) fits no 2 GiB, 2 CPU job"}},
		{name: "job size unset fits nothing", power: desktop, cfg: func(c *config.Config) { c.Job.CPUs = 0 },
			profile: Boost, limit: boost, slots: 0, admit: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { runCase(t, tt) })
	}
}

func runCase(t *testing.T, tt tc) {
	t.Helper()
	cfg := baseConfig()
	if tt.cfg != nil {
		tt.cfg(&cfg)
	}
	probe := healthy
	if tt.probe != nil {
		tt.probe(&probe)
	}
	now := tt.now
	if now.IsZero() {
		now = noon
	}
	d := New(cfg, tt.power, tt.idle, probe).WithLocation(zone).Decide(context.Background(), now, tt.running)
	if d.Full != tt.full {
		t.Errorf("full %v, want %v (only full slots may set it)", d.Full, tt.full)
	}
	if d.Profile != tt.profile || d.Slots != tt.slots || d.Admit != tt.admit || d.Evict != tt.evict {
		t.Fatalf("got profile=%s slots=%d admit=%v evict=%v, want %s %d %v %v; reasons %q",
			d.Profile, d.Slots, d.Admit, d.Evict, tt.profile, tt.slots, tt.admit, tt.evict, d.Reasons)
	}
	if d.Limit != tt.limit {
		t.Fatalf("limit %+v, want %+v", d.Limit, tt.limit)
	}
	if tt.quiet && len(d.Reasons) != 0 {
		t.Fatalf("want no reasons, got %q", d.Reasons)
	}
	if !d.Admit && len(d.Reasons) == 0 && d.Profile != Off {
		t.Fatalf("refused with no reason")
	}
	i := 0
	for _, want := range tt.reasons {
		for i < len(d.Reasons) && !strings.Contains(d.Reasons[i], want) {
			i++
		}
		if i == len(d.Reasons) {
			t.Fatalf("reason %q missing or out of order in %q", want, d.Reasons)
		}
	}
	for _, r := range d.Reasons {
		if strings.ContainsAny(r, "\u2013\u2014") {
			t.Fatalf("reason has a dash: %q", r)
		}
	}
}

func TestDecideNilProbeFailsClosed(t *testing.T) {
	d := New(baseConfig(), desktop, nil, nil).Decide(context.Background(), noon, 0)
	if d.Admit || d.Profile != Boost {
		t.Fatalf("%+v", d)
	}
}

func TestDefaultLocationIsLocal(t *testing.T) {
	if New(baseConfig(), desktop, nil, healthy).WithLocation(nil).loc != time.Local {
		t.Fatal("default location is not time.Local")
	}
}

func TestFmtDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0s", -time.Second: "0s", 45 * time.Second: "45s", 10 * time.Minute: "10m",
		150 * time.Second: "2m30s", 90 * time.Minute: "1h30m", 2 * time.Hour: "2h", time.Hour + 5*time.Second: "1h",
	} {
		if got := fmtDur(d); got != want {
			t.Errorf("fmtDur(%v) = %q, want %q", d, got, want)
		}
	}
}

// The decision names the guards that hold CI back, its own and the host's.
// A busy guard (a watched game, high load) holds CI back only when it
// lowers boost to everyday; under everyday, jobs keep the full budget, so
// it is not named (review second-F6).
func TestDecideNamesGuards(t *testing.T) {
	steam := func(f *guards.Fake) { f.Procs = []string{"steam"} }
	for _, tt := range []struct {
		name    string
		power   core.PowerSource
		probe   func(*guards.Fake)
		want    []string
		profile string
	}{
		{name: "steam lowers boost", power: desktop, probe: steam, want: []string{guards.GuardProcess}, profile: Everyday},
		{name: "steam under everyday", power: battery(60), probe: steam, profile: Everyday},
		{name: "desktop", power: desktop},
		{name: "battery low", power: battery(30), want: []string{GuardBattery}},
		{name: "battery very low", power: battery(10), want: []string{GuardBatteryStop}},
		{name: "power unknown", power: power.Fake{Err: errors.New("boom")}, want: []string{GuardPower}},
		{name: "battery low and memory low", power: battery(30), probe: func(f *guards.Fake) { f.Memory = gib },
			want: []string{GuardBattery, guards.GuardMemory}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probe := healthy
			if tt.probe != nil {
				tt.probe(&probe)
			}
			cfg := baseConfig()
			cfg.Guards.PauseWhenRunning = []string{"steam"}
			d := New(cfg, tt.power, nil, probe).WithLocation(zone).Decide(context.Background(), noon, 0)
			if strings.Join(d.Guards, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("guards %q, want %q (reasons %q)", d.Guards, tt.want, d.Reasons)
			}
			if tt.profile != "" && (d.Profile != tt.profile || !d.Admit) {
				t.Fatalf("profile %q admit %v, want %q and admit", d.Profile, d.Admit, tt.profile)
			}
		})
	}
}
