package power

import (
	"errors"

	"github.com/baney75/tentmaker/internal/core"
)

// fromSystemPowerStatus turns the fields of Windows' SYSTEM_POWER_STATUS
// into a PowerState. ac: 0 offline, 1 online, 255 unknown. flag: bit 128 is
// "no system battery", 255 is unknown. pct: 0..100, 255 unknown.
func fromSystemPowerStatus(ac, flag, pct byte) (core.PowerState, error) {
	if flag != 255 && flag&128 != 0 {
		return core.PowerState{Known: true, OnAC: true}, nil
	}
	st := core.PowerState{Known: true, HasBattery: true}
	switch ac {
	case 1:
		st.OnAC = true
	case 0:
	default:
		return core.PowerState{}, errors.New("Windows reports the AC line status as unknown")
	}
	if pct <= 100 {
		st.BatteryPercent = int(pct)
	} else if !st.OnAC {
		return core.PowerState{}, errors.New("Windows reports the battery percent as unknown while on battery")
	}
	return st, nil
}
