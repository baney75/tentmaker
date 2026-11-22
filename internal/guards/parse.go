package guards

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var vmPageSizeRe = regexp.MustCompile(`page size of (\d+) bytes`)

// parseVMStat returns the memory a new job could use, in bytes, from macOS
// `vm_stat`: free + inactive + speculative + purgeable pages. macOS keeps
// "free" small on purpose and reclaims inactive pages on demand, so free
// alone would refuse every job.
func parseVMStat(text string) (int64, error) {
	m := vmPageSizeRe.FindStringSubmatch(text)
	if m == nil {
		return 0, errors.New("no page size in vm_stat output")
	}
	size, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || size <= 0 {
		return 0, fmt.Errorf("vm_stat page size %q", m[1])
	}
	var pages int64
	seen := false
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Pages free", "Pages inactive", "Pages speculative", "Pages purgeable":
			n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), "."), 10, 64)
			if err != nil || n < 0 {
				return 0, fmt.Errorf("vm_stat %s: %q is not a page count", strings.TrimSpace(k), strings.TrimSpace(v))
			}
			pages += n
			seen = true
		}
	}
	if !seen {
		return 0, errors.New("no page counts in vm_stat output")
	}
	return pages * size, nil
}

// parseMeminfo returns key ("MemAvailable") from /proc/meminfo in bytes.
func parseMeminfo(text, key string) (int64, error) {
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			break
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("meminfo %s: bad value %q", key, f[0])
		}
		if len(f) > 1 && strings.EqualFold(f[1], "kB") {
			n *= 1024
		}
		return n, nil
	}
	return 0, fmt.Errorf("meminfo has no %s (kernel older than 3.14?)", key)
}

// parseProcLoadavg reads the 1-minute load from /proc/loadavg
// ("0.52 0.58 0.59 1/1234 5678").
func parseProcLoadavg(text string) (float64, error) {
	f := strings.Fields(text)
	if len(f) < 3 {
		return 0, fmt.Errorf("loadavg %q has too few fields", strings.TrimSpace(text))
	}
	return parseLoad(f[0])
}

// parseSysctlLoadavg reads the 1-minute load from macOS `sysctl -n
// vm.loadavg` ("{ 1.23 1.45 1.67 }").
func parseSysctlLoadavg(text string) (float64, error) {
	s := strings.TrimSpace(text)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return 0, fmt.Errorf("vm.loadavg %q is not { a b c }", s)
	}
	f := strings.Fields(strings.Trim(s, "{} "))
	if len(f) < 3 {
		return 0, fmt.Errorf("vm.loadavg %q has too few fields", s)
	}
	return parseLoad(f[0])
}

func parseLoad(s string) (float64, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("load %q is not a number", s)
	}
	return v, nil
}

// perCPU divides a load by the CPU count.
func perCPU(load float64, cpus int) (float64, error) {
	if cpus < 1 {
		return 0, fmt.Errorf("CPU count %d", cpus)
	}
	return load / float64(cpus), nil
}

// parsePsComm reads `ps -axco comm` (header COMM, one name per line; names
// may hold spaces) into lowercase names.
func parsePsComm(text string) ([]string, error) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "COMM" {
		return nil, errors.New("ps output has no COMM header")
	}
	var out []string
	for _, l := range lines[1:] {
		if n := strings.ToLower(strings.TrimSpace(l)); n != "" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("ps listed no processes")
	}
	return out, nil
}

// cpuTimes is one GetSystemTimes sample, in 100 ns units. Kernel time
// includes idle time, as Windows reports it.
type cpuTimes struct{ idle, kernel, user uint64 }

// cpuBusy is the fraction of CPU time not idle between two samples.
func cpuBusy(a, b cpuTimes) (float64, error) {
	if b.idle < a.idle || b.kernel < a.kernel || b.user < a.user {
		return 0, errors.New("CPU times went backwards")
	}
	idle := b.idle - a.idle
	total := (b.kernel - a.kernel) + (b.user - a.user)
	if total == 0 {
		return 0, errors.New("no CPU time passed between samples")
	}
	if idle > total {
		idle = total
	}
	return float64(total-idle) / float64(total), nil
}
