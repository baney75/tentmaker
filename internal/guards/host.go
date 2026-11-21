package guards

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// freeDiskAt measures free disk for path. A path that doesn't exist yet
// (a state directory before its first job) is measured at its nearest
// existing parent, which is the file system it will live on.
func freeDiskAt(path string, statfs func(string) (int64, error)) (int64, error) {
	if path == "" {
		return 0, errors.New("no state directory to measure")
	}
	p := filepath.Clean(path)
	for {
		n, err := statfs(p)
		if err == nil {
			return n, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return 0, err
		}
		p = parent
	}
}

// procProbe reads Linux's /proc through fsys (rooted at /proc).
type procProbe struct {
	stateDir string
	proc     fs.FS
	statfs   func(string) (int64, error)
	cpus     func() int
}

func (p procProbe) FreeMemory(context.Context) (int64, error) {
	b, err := fs.ReadFile(p.proc, "meminfo")
	if err != nil {
		return 0, err
	}
	return parseMeminfo(string(b), "MemAvailable")
}

func (p procProbe) FreeDisk(_ context.Context, path string) (int64, error) {
	if path == "" {
		path = p.stateDir
	}
	return freeDiskAt(path, p.statfs)
}

func (p procProbe) LoadPerCPU(context.Context) (float64, error) {
	b, err := fs.ReadFile(p.proc, "loadavg")
	if err != nil {
		return 0, err
	}
	l, err := parseProcLoadavg(string(b))
	if err != nil {
		return 0, err
	}
	return perCPU(l, p.cpus())
}

// Processes reads every /proc/<pid>/comm. A process that exits mid-scan is
// skipped; an unreadable /proc is an error.
func (p procProbe) Processes(context.Context) ([]string, error) {
	entries, err := fs.ReadDir(p.proc, ".")
	if err != nil {
		return nil, fmt.Errorf("read /proc: %w", err)
	}
	var out []string
	for _, e := range entries {
		if !isPID(e.Name()) {
			continue
		}
		b, err := fs.ReadFile(p.proc, e.Name()+"/comm")
		if err != nil {
			continue
		}
		if n := strings.ToLower(strings.TrimSpace(string(b))); n != "" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no processes found in /proc")
	}
	return out, nil
}

func isPID(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// macProbe reads macOS through commands.
type macProbe struct {
	stateDir string
	run      runner
	statfs   func(string) (int64, error)
	cpus     func() int
}

func (m macProbe) FreeMemory(ctx context.Context) (int64, error) {
	out, err := m.run(ctx, "/usr/bin/vm_stat")
	if err != nil {
		return 0, err
	}
	return parseVMStat(string(out))
}

func (m macProbe) FreeDisk(_ context.Context, path string) (int64, error) {
	if path == "" {
		path = m.stateDir
	}
	return freeDiskAt(path, m.statfs)
}

func (m macProbe) LoadPerCPU(ctx context.Context) (float64, error) {
	out, err := m.run(ctx, "/usr/sbin/sysctl", "-n", "vm.loadavg")
	if err != nil {
		return 0, err
	}
	l, err := parseSysctlLoadavg(string(out))
	if err != nil {
		return 0, err
	}
	return perCPU(l, m.cpus())
}

func (m macProbe) Processes(ctx context.Context) ([]string, error) {
	out, err := m.run(ctx, "/bin/ps", "-axco", "comm")
	if err != nil {
		return nil, err
	}
	return parsePsComm(string(out))
}
