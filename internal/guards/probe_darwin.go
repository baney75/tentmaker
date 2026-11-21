//go:build darwin

package guards

import "runtime"

// NewHostProbe reads vm_stat, sysctl, ps and statfs. FreeDisk with an empty
// path measures stateDir.
func NewHostProbe(stateDir string) Probe {
	return macProbe{stateDir: stateDir, run: runCommand, statfs: statfsFree, cpus: runtime.NumCPU}
}
