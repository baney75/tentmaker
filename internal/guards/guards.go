// Package guards keeps CI out of the way of what the machine is for. It
// probes free memory, free disk, load and the process list, and Check turns
// those readings and the configured limits into an admit/evict answer.
//
// Every probe that fails, fails closed: Check then says "no new jobs".
package guards

import (
	"context"
	"fmt"
	"strings"

	"github.com/baney75/tentmaker/internal/config"
)

// Probe reads the host. Tests use Fake.
type Probe interface {
	FreeMemory(ctx context.Context) (int64, error)            // available to new processes
	FreeDisk(ctx context.Context, path string) (int64, error) // bytes free to unprivileged users
	LoadPerCPU(ctx context.Context) (float64, error)          // 1-min load / CPUs; Windows: CPU busy fraction
	Processes(ctx context.Context) ([]string, error)          // lowercase executable names
}

// Result is what the guards allow right now.
type Result struct {
	Admit bool `json:"admit"` // may start new jobs
	Evict bool `json:"evict"` // must stop running jobs (stop_below_disk)
	// Busy: the user is using the machine (a watched process runs, or load
	// is high). It never blocks the reserved everyday budget, only boost. A
	// busy probe that can't be read counts as busy.
	Busy    bool     `json:"busy"`
	Reasons []string `json:"reasons"` // one plain sentence per tripped guard
}

// diskPerRunningJob is the extra free disk kept per running job, since each
// may still grow.
const diskPerRunningJob = int64(config.GiB)

// Check runs every guard and joins the results. Each guard runs even after
// another trips, so the reasons list all of them. Load and the process list
// are read only when their guard is on.
func Check(ctx context.Context, g config.Guards, job config.Job, p Probe, stateDir string, running int) Result {
	r := Result{Admit: true}
	block := func(format string, a ...any) {
		r.Admit = false
		r.Reasons = append(r.Reasons, fmt.Sprintf(format, a...))
	}
	if p == nil {
		block("no host probe, so no new jobs")
		return r
	}
	if running < 0 {
		running = 0
	}

	if free, err := p.FreeMemory(ctx); err != nil {
		block("can't read free memory (%v), so no new jobs", err)
	} else if need := int64(job.Memory) + int64(g.MinFreeMemory); free < need {
		block("free memory %s is below %s (a %s job plus %s kept free)",
			FormatBytes(free), FormatBytes(need), FormatBytes(int64(job.Memory)), FormatBytes(int64(g.MinFreeMemory)))
	}

	if free, err := p.FreeDisk(ctx, stateDir); err != nil {
		block("can't read free disk at %s (%v), so no new jobs", diskName(stateDir), err)
	} else {
		need := int64(g.MinFreeDisk) + int64(running)*diskPerRunningJob
		if free < need {
			if running > 0 {
				block("free disk %s at %s is below %s (%s plus 1 GiB for each of %d running jobs)",
					FormatBytes(free), diskName(stateDir), FormatBytes(need), FormatBytes(int64(g.MinFreeDisk)), running)
			} else {
				block("free disk %s at %s is below %s", FormatBytes(free), diskName(stateDir), FormatBytes(need))
			}
		}
		if stop := int64(g.StopBelowDisk); stop > 0 && free < stop {
			r.Evict = true
			block("free disk %s at %s is below %s, stopping running jobs", FormatBytes(free), diskName(stateDir), FormatBytes(stop))
		}
	}

	busy := func(format string, a ...any) {
		r.Busy = true
		r.Reasons = append(r.Reasons, fmt.Sprintf(format, a...))
	}
	if g.MaxLoadPerCPU > 0 {
		if load, err := p.LoadPerCPU(ctx); err != nil {
			busy("can't read load (%v), so it counts as busy", err)
		} else if load > g.MaxLoadPerCPU {
			busy("load %.2f per CPU is above %.2f", load, g.MaxLoadPerCPU)
		}
	}

	if names := wanted(g.PauseWhenRunning); len(names) > 0 {
		if procs, err := p.Processes(ctx); err != nil {
			busy("can't read the process list (%v), so it counts as busy", err)
		} else {
			for _, w := range names {
				if matchAny(w.norm, procs) {
					busy("%s is running", w.label)
				}
			}
		}
	}
	return r
}

type want struct{ label, norm string }

func wanted(list []string) []want {
	var out []want
	seen := map[string]bool{}
	for _, s := range list {
		label := strings.TrimSpace(s)
		n := NormalizeProcess(label)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, want{label: label, norm: n})
	}
	return out
}

// commLimit is how many characters Linux keeps of a process name
// (/proc/<pid>/comm); macOS keeps 16.
const commLimit = 15

func matchAny(want string, procs []string) bool {
	for _, p := range procs {
		p = NormalizeProcess(p)
		if p == "" {
			continue
		}
		if p == want || (len(p) >= commLimit && len(want) > len(p) && strings.HasPrefix(want, p)) {
			return true
		}
	}
	return false
}

// NormalizeProcess lowercases a process name, drops any directory and a
// trailing ".exe", so "C:\Games\Steam.exe", "steam.exe" and "steam" match.
func NormalizeProcess(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSuffix(s, ".exe")
}

func diskName(path string) string {
	if path == "" {
		return "the state directory"
	}
	return path
}

// FormatBytes writes n as "3.1 GiB" or "512 MiB".
func FormatBytes(n int64) string {
	const gib, mib = 1 << 30, 1 << 20
	neg := ""
	if n < 0 {
		neg, n = "-", -n
	}
	if n >= gib {
		s := fmt.Sprintf("%.1f", float64(n)/gib)
		return neg + strings.TrimSuffix(s, ".0") + " GiB"
	}
	return fmt.Sprintf("%s%d MiB", neg, (n+mib/2)/mib)
}
