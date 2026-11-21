//go:build darwin

package power

import (
	"context"
	"runtime"

	"github.com/baney75/tentmaker/internal/core"
)

// New reads `pmset -g batt`.
func New() core.PowerSource { return pmsetPower{run: runCommand} }

// NewIdle reads HIDIdleTime from `ioreg -c IOHIDSystem`.
func NewIdle() core.IdleSource { return ioregIdle{run: runCommand} }

// TotalMemory reads `sysctl -n hw.memsize`.
func TotalMemory() (int64, error) { return darwinTotalMemory(context.Background(), runCommand) }

// CPUs is the number of CPUs this process may use.
func CPUs() int { return runtime.NumCPU() }
