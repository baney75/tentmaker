//go:build !linux && !darwin && !windows

package power

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

type unsupported struct{}

func (unsupported) Power(context.Context) (core.PowerState, error) {
	return core.PowerState{}, fmt.Errorf("power: %s: %w", runtime.GOOS, errUnsupported)
}

func (unsupported) IdleFor(context.Context) (time.Duration, error) {
	return 0, fmt.Errorf("idle: %s: %w", runtime.GOOS, errUnsupported)
}

// New returns a source that always fails, so the budget fails closed.
func New() core.PowerSource { return unsupported{} }

// NewIdle returns a source that always fails ("not idle").
func NewIdle() core.IdleSource { return unsupported{} }

// TotalMemory is not read on this OS.
func TotalMemory() (int64, error) {
	return 0, fmt.Errorf("total memory: %s: %w", runtime.GOOS, errUnsupported)
}

// CPUs is the number of CPUs this process may use.
func CPUs() int { return runtime.NumCPU() }
