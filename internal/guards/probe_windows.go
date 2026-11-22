//go:build windows

package guards

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
)

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

// loadSampleGap is the time between the two GetSystemTimes samples.
const loadSampleGap = 500 * time.Millisecond

type winProbe struct{ stateDir string }

// NewHostProbe reads Windows through kernel32. FreeDisk with an empty path
// measures stateDir.
func NewHostProbe(stateDir string) Probe { return winProbe{stateDir: stateDir} }

func (winProbe) FreeMemory(context.Context) (int64, error) {
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	return int64(m.AvailPhys), nil
}

func (w winProbe) FreeDisk(_ context.Context, path string) (int64, error) {
	if path == "" {
		path = w.stateDir
	}
	return freeDiskAt(path, diskFree)
}

func diskFree(path string) (int64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return int64(avail), nil
}

func systemTimes() (cpuTimes, error) {
	var idle, kernel, user windows.Filetime
	r, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return cpuTimes{}, fmt.Errorf("GetSystemTimes: %w", err)
	}
	ft := func(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }
	return cpuTimes{idle: ft(idle), kernel: ft(kernel), user: ft(user)}, nil
}

// LoadPerCPU is the CPU busy fraction over 500 ms (0..1). Windows has no
// load average.
func (winProbe) LoadPerCPU(ctx context.Context) (float64, error) {
	a, err := systemTimes()
	if err != nil {
		return 0, err
	}
	t := time.NewTimer(loadSampleGap)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-t.C:
	}
	b, err := systemTimes()
	if err != nil {
		return 0, err
	}
	return cpuBusy(a, b)
}

func (winProbe) Processes(context.Context) ([]string, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(snap, &e); err != nil {
		return nil, fmt.Errorf("Process32First: %w", err)
	}
	var out []string
	for {
		if n := strings.ToLower(windows.UTF16ToString(e.ExeFile[:])); n != "" {
			out = append(out, n)
		}
		if err := windows.Process32Next(snap, &e); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, fmt.Errorf("Process32Next: %w", err)
		}
	}
	return out, nil
}
