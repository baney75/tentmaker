//go:build linux

package power

import (
	"os"
	"runtime"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// New reads /sys/class/power_supply.
func New() core.PowerSource { return sysfsPower{fsys: os.DirFS("/sys/class/power_supply")} }

// NewIdle reads the active seat0 session's idle hint through loginctl.
func NewIdle() core.IdleSource { return loginctlIdle{run: runCommand, now: time.Now} }

// TotalMemory reads MemTotal from /proc/meminfo.
func TotalMemory() (int64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	return parseMeminfo(string(b), "MemTotal")
}

// CPUs is the number of CPUs this process may use.
func CPUs() int { return runtime.NumCPU() }
