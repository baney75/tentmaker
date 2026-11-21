package power

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// sysfsPower reads /sys/class/power_supply. fsys is rooted at that
// directory, so tests use an fstest.MapFS.
type sysfsPower struct{ fsys fs.FS }

func (s sysfsPower) Power(context.Context) (core.PowerState, error) {
	return readPowerSupply(s.fsys)
}

// supply is one entry under /sys/class/power_supply.
type supply struct {
	name, kind, scope, status string
	online                    string
	capacity                  string
	energyNow, energyFull     string
	chargeNow, chargeFull     string
}

// readField reads one sysfs attribute. A missing optional attribute is
// empty; any other read error is returned, so the probe fails closed
// instead of guessing (review r1 partB sol-F6).
func readField(fsys fs.FS, name string, required bool) (string, error) {
	b, err := fs.ReadFile(fsys, name)
	switch {
	case err == nil:
		return strings.TrimSpace(string(b)), nil
	case errors.Is(err, fs.ErrNotExist) && !required:
		return "", nil
	default:
		return "", fmt.Errorf("read power supply %s: %w", name, err)
	}
}

// readPowerSupply decides the power state from sysfs. A Mains or USB supply
// that is online means AC. No battery at all (a desktop, a VM, a server)
// means AC. With batteries but no Mains or USB entry, a discharging battery
// means on battery. Batteries whose scope is Device (a wireless mouse) are
// not the machine's own and are ignored.
func readPowerSupply(fsys fs.FS) (core.PowerState, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if errors.Is(err, fs.ErrNotExist) {
		return core.PowerState{Known: true, OnAC: true}, nil
	}
	if err != nil {
		return core.PowerState{}, fmt.Errorf("read power supplies: %w", err)
	}
	var supplies []supply
	for _, e := range entries {
		n := e.Name()
		// Every supply has a type; one we can't read might be the
		// machine's own battery, and skipping it would report AC.
		kind, err := readField(fsys, n+"/type", true)
		if err != nil {
			return core.PowerState{}, err
		}
		s := supply{name: n, kind: kind}
		for _, f := range []struct {
			file string
			dst  *string
		}{
			{"scope", &s.scope}, {"status", &s.status}, {"online", &s.online}, {"capacity", &s.capacity},
			{"energy_now", &s.energyNow}, {"energy_full", &s.energyFull},
			{"charge_now", &s.chargeNow}, {"charge_full", &s.chargeFull},
		} {
			if *f.dst, err = readField(fsys, n+"/"+f.file, false); err != nil {
				return core.PowerState{}, err
			}
		}
		supplies = append(supplies, s)
	}
	return decideSysfs(supplies)
}

func isLineSupply(kind string) bool {
	return kind == "Mains" || kind == "USB" || strings.HasPrefix(kind, "USB_")
}

func decideSysfs(supplies []supply) (core.PowerState, error) {
	var batteries []supply
	lines, linesOnline := 0, 0
	for _, s := range supplies {
		switch {
		case s.kind == "Battery":
			if strings.EqualFold(s.scope, "Device") {
				continue
			}
			batteries = append(batteries, s)
		case isLineSupply(s.kind):
			lines++
			if s.online == "1" {
				linesOnline++
			}
		}
	}
	if len(batteries) == 0 {
		return core.PowerState{Known: true, OnAC: true}, nil
	}
	pct, err := batteryPercent(batteries)
	if err != nil {
		return core.PowerState{}, err
	}
	onAC := linesOnline > 0
	if lines == 0 {
		onAC = true
		for _, b := range batteries {
			if b.status == "Discharging" {
				onAC = false
			}
		}
	}
	return core.PowerState{Known: true, OnAC: onAC, HasBattery: true, BatteryPercent: pct}, nil
}

// batteryPercent combines batteries by stored energy when every battery
// reports it (two ThinkPad packs of different sizes), else averages capacity.
func batteryPercent(bats []supply) (int, error) {
	var now, full float64
	weighted := true
	for _, b := range bats {
		n, f := b.energyNow, b.energyFull
		if n == "" || f == "" {
			n, f = b.chargeNow, b.chargeFull
		}
		nv, err1 := strconv.ParseFloat(n, 64)
		fv, err2 := strconv.ParseFloat(f, 64)
		if err1 != nil || err2 != nil || fv <= 0 {
			weighted = false
			break
		}
		now += nv
		full += fv
	}
	if weighted {
		return clampPercent(int(now/full*100 + 0.5)), nil
	}
	sum := 0
	for _, b := range bats {
		c, err := strconv.Atoi(b.capacity)
		if err != nil {
			return 0, fmt.Errorf("battery %s: can't read capacity %q", b.name, b.capacity)
		}
		sum += c
	}
	return clampPercent((sum + len(bats)/2) / len(bats)), nil
}

func clampPercent(p int) int {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// parseMeminfo returns the value of key ("MemTotal", "MemAvailable") from
// /proc/meminfo in bytes.
func parseMeminfo(text, key string) (int64, error) {
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			break
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("meminfo %s: bad value %q", key, f[0])
		}
		mult := int64(1)
		if len(f) > 1 && strings.EqualFold(f[1], "kB") {
			mult = 1024
		}
		return n * mult, nil
	}
	return 0, fmt.Errorf("meminfo has no %s", key)
}

// loginctlIdle reads the active session on seat0 through loginctl.
type loginctlIdle struct {
	run runner
	now func() time.Time
}

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (l loginctlIdle) IdleFor(ctx context.Context) (time.Duration, error) {
	out, err := l.run(ctx, "loginctl", "show-seat", "seat0", "-p", "ActiveSession", "--value")
	if err != nil {
		return 0, err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return 0, errors.New("no active session on seat0, so there is no user to be idle")
	}
	if !sessionIDRe.MatchString(id) {
		return 0, fmt.Errorf("loginctl returned an odd session id %q", id)
	}
	out, err = l.run(ctx, "loginctl", "show-session", id, "-p", "IdleHint", "-p", "IdleSinceHint")
	if err != nil {
		return 0, err
	}
	return parseLoginctlIdle(string(out), l.now())
}

// parseLoginctlIdle reads `loginctl show-session -p IdleHint -p
// IdleSinceHint`. IdleSinceHint is microseconds since the Unix epoch.
func parseLoginctlIdle(text string, now time.Time) (time.Duration, error) {
	var hint, since string
	var haveHint, haveSince bool
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "IdleHint":
			hint, haveHint = v, true
		case "IdleSinceHint":
			since, haveSince = v, true
		}
	}
	if !haveHint {
		return 0, errors.New("loginctl gave no IdleHint")
	}
	switch hint {
	case "no":
		return 0, nil
	case "yes":
	default:
		return 0, fmt.Errorf("loginctl IdleHint %q is not yes or no", hint)
	}
	if !haveSince {
		return 0, errors.New("loginctl gave no IdleSinceHint")
	}
	us, err := strconv.ParseInt(since, 10, 64)
	if err != nil || us <= 0 {
		return 0, fmt.Errorf("loginctl IdleSinceHint %q is not a time", since)
	}
	d := now.Sub(time.UnixMicro(us))
	if d < 0 {
		return 0, nil
	}
	return d, nil
}
