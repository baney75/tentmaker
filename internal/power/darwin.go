package power

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// pmsetPower reads `pmset -g batt`.
type pmsetPower struct{ run runner }

func (p pmsetPower) Power(ctx context.Context) (core.PowerState, error) {
	out, err := p.run(ctx, "/usr/bin/pmset", "-g", "batt")
	if err != nil {
		return core.PowerState{}, err
	}
	return parsePmset(string(out))
}

var (
	pmsetSourceRe  = regexp.MustCompile(`drawing from '([^']+)'`)
	pmsetPercentRe = regexp.MustCompile(`\t\s*(\d{1,3})%;`)
)

// parsePmset reads `pmset -g batt`. "AC Power" is AC; "Battery Power" is the
// internal battery; "UPS Power" is a UPS battery during an outage, which
// counts as on battery. A Mac with no battery line (a Mac mini) has no
// battery.
func parsePmset(text string) (core.PowerState, error) {
	m := pmsetSourceRe.FindStringSubmatch(text)
	if m == nil {
		return core.PowerState{}, errors.New("pmset output has no power source line")
	}
	var internal, other = -1, -1
	for _, line := range strings.Split(text, "\n") {
		pm := pmsetPercentRe.FindStringSubmatch(line)
		if pm == nil {
			continue
		}
		n, err := strconv.Atoi(pm[1])
		if err != nil {
			return core.PowerState{}, fmt.Errorf("pmset battery percent %q", pm[1])
		}
		if strings.Contains(line, "InternalBattery") {
			if internal < 0 {
				internal = n
			}
		} else if other < 0 {
			other = n
		}
	}
	switch m[1] {
	case "AC Power":
		st := core.PowerState{Known: true, OnAC: true}
		if internal >= 0 {
			st.HasBattery, st.BatteryPercent = true, clampPercent(internal)
		}
		return st, nil
	case "Battery Power":
		if internal < 0 {
			return core.PowerState{}, errors.New("pmset says Battery Power but shows no battery percent")
		}
		return core.PowerState{Known: true, HasBattery: true, BatteryPercent: clampPercent(internal)}, nil
	case "UPS Power":
		pct := other
		if pct < 0 {
			pct = internal
		}
		if pct < 0 {
			return core.PowerState{}, errors.New("pmset says UPS Power but shows no percent")
		}
		return core.PowerState{Known: true, HasBattery: true, BatteryPercent: clampPercent(pct)}, nil
	default:
		return core.PowerState{}, fmt.Errorf("pmset power source %q is not one Tentmaker knows", m[1])
	}
}

// ioregIdle reads HIDIdleTime from `ioreg -c IOHIDSystem`.
type ioregIdle struct{ run runner }

func (i ioregIdle) IdleFor(ctx context.Context) (time.Duration, error) {
	out, err := i.run(ctx, "/usr/sbin/ioreg", "-c", "IOHIDSystem")
	if err != nil {
		return 0, err
	}
	return parseIoregIdle(string(out))
}

var hidIdleRe = regexp.MustCompile(`"HIDIdleTime"\s*=\s*(\d+)`)

// parseIoregIdle returns the smallest HIDIdleTime (nanoseconds) in the
// output, so any recently used input device counts as activity.
func parseIoregIdle(text string) (time.Duration, error) {
	ms := hidIdleRe.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return 0, errors.New("ioreg output has no HIDIdleTime")
	}
	var best int64 = -1
	for _, m := range ms {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("ioreg HIDIdleTime %q is not a number", m[1])
		}
		if best < 0 || n < best {
			best = n
		}
	}
	return time.Duration(best), nil
}

// parseInt64 reads one integer (sysctl -n hw.memsize).
func parseInt64(text, what string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: %q is not a positive number", what, strings.TrimSpace(text))
	}
	return n, nil
}

func darwinTotalMemory(ctx context.Context, run runner) (int64, error) {
	out, err := run(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize")
	if err != nil {
		return 0, err
	}
	return parseInt64(string(out), "sysctl hw.memsize")
}
