//go:build !linux && !darwin && !windows

package guards

import (
	"context"
	"fmt"
	"runtime"
)

type unsupported struct{}

func errUnsupported(what string) error {
	return fmt.Errorf("%s: not supported on %s", what, runtime.GOOS)
}

func (unsupported) FreeMemory(context.Context) (int64, error) {
	return 0, errUnsupported("free memory")
}
func (unsupported) FreeDisk(context.Context, string) (int64, error) {
	return 0, errUnsupported("free disk")
}
func (unsupported) LoadPerCPU(context.Context) (float64, error) {
	return 0, errUnsupported("load")
}
func (unsupported) Processes(context.Context) ([]string, error) {
	return nil, errUnsupported("process list")
}

// NewHostProbe returns a probe whose every reading fails, so Check fails
// closed on this OS.
func NewHostProbe(stateDir string) Probe { return unsupported{} }
