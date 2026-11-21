//go:build !windows

package linux

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A job that leaves a FIFO as result.json must not hang the daemon.
func TestCheckResultFIFO(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	out := filepath.Join(v.e.jobDir("chk-fifo"), "out")
	must(t, os.MkdirAll(out, 0o700))
	must(t, syscall.Mkfifo(filepath.Join(out, "result.json"), 0o600))
	must(t, syscall.Mkfifo(filepath.Join(out, "log.txt"), 0o600))
	done := make(chan string, 1)
	go func() {
		r, _ := v.e.CheckResult(context.Background(), "chk-fifo")
		done <- r.State
	}()
	select {
	case st := <-done:
		if st != "error" {
			t.Fatalf("state %s", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CheckResult hung on a FIFO")
	}
}
