// Package budget decides how much of this machine CI may use right now. It
// reads the power source, the user's idle time, the schedule and the host
// guards, and returns a core.Decision: a profile, a resource limit, a slot
// count and whether to admit new jobs or evict running ones.
package budget

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/guards"
)

// Profiles a Decision can carry.
const (
	Everyday = "everyday"
	Boost    = "boost"
	Off      = "off"
)

// Engine implements core.Budgeter.
type Engine struct {
	cfg   config.Config
	power core.PowerSource
	idle  core.IdleSource
	probe guards.Probe
	loc   *time.Location
}

var _ core.Budgeter = (*Engine)(nil)

// New builds an Engine. A nil power source counts as "can't read power", a
// nil idle source as "not idle", and a nil probe fails the guards closed.
func New(cfg config.Config, power core.PowerSource, idle core.IdleSource, probe guards.Probe) *Engine {
	return &Engine{cfg: cfg, power: power, idle: idle, probe: probe, loc: time.Local}
}

// WithLocation sets the time zone schedule windows are read in (default:
// the machine's local time) and returns e.
func (e *Engine) WithLocation(loc *time.Location) *Engine {
	if loc != nil {
		e.loc = loc
	}
	return e
}

// The budget's own guards, as core.Decision.Guards names them next to the
// host guards (guards.Guard*).
const (
	GuardPower       = "power"        // power can't be read
	GuardBattery     = "battery"      // on battery below pause_below_percent
	GuardBatteryStop = "battery_stop" // on battery below stop_below_percent
)

// Decide returns the budget for now with running jobs already running.
//
// The profile comes from the first of these that applies: power unknown
// (everyday, no new jobs); battery below stop_below_percent (off, evict);
// battery below pause_below_percent (off); a schedule window; plugged in and
// idle enough (boost); on battery (on_battery); everyday. Slots, the guards
// and the running count can then only lower the answer.
func (e *Engine) Decide(ctx context.Context, now time.Time, running int) core.Decision {
	if running < 0 {
		running = 0 // a bad count must not open a slot that isn't there
	}
	d := core.Decision{Admit: true}
	why := func(format string, a ...any) { d.Reasons = append(d.Reasons, fmt.Sprintf(format, a...)) }
	b := e.cfg.Budget

	ps, perr := e.readPower(ctx)
	onBattery := ps.HasBattery && !ps.OnAC
	onBatteryProfile := func() string {
		if strings.EqualFold(b.OnBattery, Off) {
			return Off
		}
		return Everyday
	}

	switch {
	case perr != nil:
		d.Profile, d.Admit = Everyday, false
		d.Guards = append(d.Guards, GuardPower)
		why("can't read power (%v), so no new jobs", perr)

	case onBattery && ps.BatteryPercent < b.StopBelow:
		d.Profile, d.Evict = Off, true
		d.Guards = append(d.Guards, GuardBatteryStop)
		why("on battery at %d%%, below %d%%, stopping running jobs", ps.BatteryPercent, b.StopBelow)

	case onBattery && ps.BatteryPercent < b.PauseBelow:
		d.Profile = Off
		d.Guards = append(d.Guards, GuardBattery)
		why("on battery at %d%%, below %d%%", ps.BatteryPercent, b.PauseBelow)

	default:
		if w, ok := e.window(now); ok {
			d.Profile = strings.ToLower(w.Profile)
			why("schedule %s (%s to %s) sets %s", windowName(w), w.From, w.To, d.Profile)
			if d.Profile == Boost && onBattery {
				d.Profile = onBatteryProfile()
				why("boost needs AC power, but the machine is on battery at %d%%, so %s", ps.BatteryPercent, d.Profile)
			}
			break
		}
		if !onBattery {
			if !b.BoostNeedsIdle {
				d.Profile = Boost
				break
			}
			idle, ierr := e.readIdle(ctx)
			switch {
			case ierr != nil:
				d.Profile = Everyday
				why("plugged in, but boost waits for you to be away and idle time is unknown (%v)", ierr)
			case idle < b.IdleAfter.D():
				d.Profile = Everyday
				why("plugged in, but boost waits until you have been away %s (away %s)", fmtDur(b.IdleAfter.D()), fmtDur(idle))
			default:
				d.Profile = Boost
			}
			break
		}
		d.Profile = onBatteryProfile()
		if d.Profile == Off {
			why("on battery at %d%% and budget.on_battery is off", ps.BatteryPercent)
		} else {
			why("on battery at %d%%, so everyday instead of boost", ps.BatteryPercent)
		}
	}

	if d.Profile == Boost && b.Boost.Memory == 0 {
		d.Profile = Everyday
		why("no boost budget is set (budget.boost.memory = 0), so boost is the same as everyday")
	}

	// Everyday is the reserved budget: it keeps running while the user plays
	// a game, takes a call or loads the machine. Only boost gives way to
	// them. Memory, disk and battery guards still stop both.
	g := guards.Check(ctx, e.cfg.Guards, e.cfg.Job, e.probe, e.cfg.StateDir, running)
	if !g.Admit {
		d.Admit = false
	}
	if g.Evict {
		d.Evict = true
	}
	d.Reasons = append(d.Reasons, g.Reasons...)
	d.Guards = append(d.Guards, g.Guards...)
	if g.Busy && d.Profile == Boost {
		d.Profile = Everyday
		// Only here does a busy guard hold CI back.
		d.Guards = append(d.Guards, g.BusyGuards...)
		why("you are using the machine, so the reserved everyday budget instead of boost")
	}

	d.Limit = e.limit(d.Profile)
	d.Slots = slots(d.Limit, e.cfg.Job)
	if d.Profile != Off && d.Slots == 0 {
		why("the %s budget (%s, %g CPUs) fits no %s, %g CPU job", d.Profile,
			guards.FormatBytes(d.Limit.MemoryBytes), d.Limit.CPUs, guards.FormatBytes(int64(e.cfg.Job.Memory)), e.cfg.Job.CPUs)
	}

	if running >= d.Slots {
		d.Admit = false
		if d.Slots > 0 {
			why("%d of %d slots in use", running, d.Slots)
		}
	}
	if d.Evict {
		d.Admit = false
	}
	return d
}

func (e *Engine) readPower(ctx context.Context) (core.PowerState, error) {
	if e.power == nil {
		return core.PowerState{}, fmt.Errorf("no power source")
	}
	ps, err := e.power.Power(ctx)
	if err != nil {
		return core.PowerState{}, err
	}
	if !ps.Known {
		return core.PowerState{}, fmt.Errorf("power state unknown")
	}
	return ps, nil
}

func (e *Engine) readIdle(ctx context.Context) (time.Duration, error) {
	if e.idle == nil {
		return 0, fmt.Errorf("no idle source")
	}
	d, err := e.idle.IdleFor(ctx)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, nil
	}
	return d, nil
}

// limit is the resources a profile allows. A boost profile with no memory
// set is the everyday profile; a boost profile with no CPUs set takes the
// everyday CPUs.
func (e *Engine) limit(profile string) core.Resources {
	b := e.cfg.Budget
	switch profile {
	case Boost:
		cpus := b.Boost.CPUs
		if cpus <= 0 {
			cpus = b.Everyday.CPUs
		}
		return core.Resources{MemoryBytes: int64(b.Boost.Memory), CPUs: cpus}
	case Everyday:
		return core.Resources{MemoryBytes: int64(b.Everyday.Memory), CPUs: b.Everyday.CPUs}
	default:
		return core.Resources{}
	}
}

// slots is how many jobs of size job fit in limit:
// min(floor(memory / job memory), floor(CPUs / job CPUs), max_slots if > 0).
func slots(limit core.Resources, job config.Job) int {
	if job.Memory <= 0 || job.CPUs <= 0 || limit.MemoryBytes <= 0 || limit.CPUs <= 0 {
		return 0
	}
	n := limit.MemoryBytes / int64(job.Memory)
	if c := int64(math.Floor(limit.CPUs/job.CPUs + 1e-9)); c < n {
		n = c
	}
	if job.MaxSlots > 0 && int64(job.MaxSlots) < n {
		n = int64(job.MaxSlots)
	}
	if n < 0 {
		return 0
	}
	return int(n)
}

// window returns the first schedule window covering now, in e.loc. A window
// whose From is not before its To crosses midnight, and it belongs to the
// day it started: "sun 22:00 to 07:00" covers Sunday night and Monday until
// 07:00. From equal to To covers a full 24 hours.
func (e *Engine) window(now time.Time) (config.Window, bool) {
	t := now.In(e.loc)
	minute := t.Hour()*60 + t.Minute()
	today := t.Weekday()
	yesterday := (today + 6) % 7
	for _, w := range e.cfg.Budget.Schedule {
		from, err1 := config.ParseClock(w.From)
		to, err2 := config.ParseClock(w.To)
		if err1 != nil || err2 != nil {
			continue
		}
		switch strings.ToLower(w.Profile) {
		case Everyday, Boost, Off:
		default:
			continue
		}
		if from < to {
			if onDay(w.Days, today) && minute >= from && minute < to {
				return w, true
			}
			continue
		}
		if onDay(w.Days, today) && minute >= from {
			return w, true
		}
		if onDay(w.Days, yesterday) && minute < to {
			return w, true
		}
	}
	return config.Window{}, false
}

func onDay(days []string, d time.Weekday) bool {
	if len(days) == 0 {
		return true
	}
	for _, s := range days {
		if wd, ok := config.Weekdays[strings.ToLower(strings.TrimSpace(s))]; ok && wd == d {
			return true
		}
	}
	return false
}

func windowName(w config.Window) string {
	if w.Name == "" {
		return "window"
	}
	return fmt.Sprintf("%q", w.Name)
}

// fmtDur writes a duration the way people say it: "10m", "1h30m", "45s".
func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d <= 0 {
		return "0s"
	}
	h, m, s := d/time.Hour, (d%time.Hour)/time.Minute, (d%time.Minute)/time.Second
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 && h == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	if b.Len() == 0 {
		return "0s"
	}
	return b.String()
}
