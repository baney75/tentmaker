package daemon

import (
	"os"
	"runtime"
	"time"
)

// RuntimeStatus is the daemon process's own health, for people watching a
// long-running device and for soak tests: a number that only grows (heap,
// goroutines, open files) over days points at a leak.
type RuntimeStatus struct {
	PID            int    `json:"pid"`
	UptimeS        int64  `json:"uptime_s"`
	Goroutines     int    `json:"goroutines"`
	HeapInUseBytes uint64 `json:"heap_in_use_bytes"`
	SysBytes       uint64 `json:"sys_bytes"` // memory the Go runtime got from the OS
	GCCycles       uint32 `json:"gc_cycles"`
	// OpenFDs counts this process's open file descriptors on Linux and
	// macOS; -1 where that isn't cheap to read (Windows).
	OpenFDs int `json:"open_fds"`
}

// readRuntime samples this process. started is when the daemon started.
func readRuntime(started, now time.Time) RuntimeStatus {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	up := int64(0)
	if !started.IsZero() && now.After(started) {
		up = int64(now.Sub(started) / time.Second)
	}
	return RuntimeStatus{
		PID:            os.Getpid(),
		UptimeS:        up,
		Goroutines:     runtime.NumGoroutine(),
		HeapInUseBytes: m.HeapInuse,
		SysBytes:       m.Sys,
		GCCycles:       m.NumGC,
		OpenFDs:        openFDs(runtime.GOOS),
	}
}

// openFDs lists the per-process fd directory. Reading it opens one more
// descriptor, which is not counted.
func openFDs(goos string) int {
	dir := ""
	switch goos {
	case "linux":
		dir = "/proc/self/fd"
	case "darwin":
		dir = "/dev/fd"
	default:
		return -1
	}
	f, err := os.Open(dir)
	if err != nil {
		return -1
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil || len(names) == 0 {
		return -1
	}
	return len(names) - 1
}
