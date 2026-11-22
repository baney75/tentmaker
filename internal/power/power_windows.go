//go:build windows

package power

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/baney75/tentmaker/internal/core"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemPowerStatus = kernel32.NewProc("GetSystemPowerStatus")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// systemPowerStatus is SYSTEM_POWER_STATUS.
type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// memoryStatusEx is MEMORYSTATUSEX.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

type winPower struct{}

func (winPower) Power(context.Context) (core.PowerState, error) {
	var s systemPowerStatus
	r, _, err := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&s)))
	if r == 0 {
		return core.PowerState{}, fmt.Errorf("GetSystemPowerStatus: %w", err)
	}
	return fromSystemPowerStatus(s.ACLineStatus, s.BatteryFlag, s.BatteryLifePercent)
}

type winIdle struct{}

func (winIdle) IdleFor(context.Context) (time.Duration, error) {
	return 0, errors.New("a Windows service can't see user input, so idle time is unknown")
}

// New calls GetSystemPowerStatus.
func New() core.PowerSource { return winPower{} }

// NewIdle always reports an error ("not idle"): the daemon runs in session 0
// and can't see the user's input.
func NewIdle() core.IdleSource { return winIdle{} }

// TotalMemory calls GlobalMemoryStatusEx.
func TotalMemory() (int64, error) {
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	return int64(m.TotalPhys), nil
}

// CPUs is the number of CPUs this process may use.
func CPUs() int { return runtime.NumCPU() }
