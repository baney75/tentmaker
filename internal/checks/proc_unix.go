//go:build unix

package checks

import (
	"os/exec"
	"syscall"
)

// setProcGroup starts the step in its own process group, so a timeout kills
// everything it spawned.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTree kills the step's whole process group.
func killTree(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// reapGroup kills what a finished step left running in its group.
func reapGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
