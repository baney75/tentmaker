//go:build windows

package checks

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// setProcGroup starts the step in a new process group.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killTree kills the step and every process it started with taskkill /T.
func killTree(pid int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// reapGroup is a no-op on Windows: once the step's own process exits,
// taskkill /T can no longer find its children. The executor's Job Object
// bounds what's left.
func reapGroup(pid int) {}
