//go:build linux

package guards

import (
	"os"
	"runtime"
)

// NewHostProbe reads /proc and statfs. FreeDisk with an empty path measures
// stateDir.
func NewHostProbe(stateDir string) Probe {
	return procProbe{stateDir: stateDir, proc: os.DirFS("/proc"), statfs: statfsFree, cpus: runtime.NumCPU}
}
