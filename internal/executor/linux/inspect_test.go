//go:build !windows

package linux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobUsageFromCgroup(t *testing.T) {
	en := newEnv(t)
	cg := filepath.Join(en.dir, Slice, unitName("tm-box-1"))
	must(t, os.MkdirAll(cg, 0o755))
	must(t, os.WriteFile(filepath.Join(cg, "memory.peak"), []byte("1073741824\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(cg, "cpu.stat"), []byte("usage_usec 2500000\nuser_usec 2000000\n"), 0o644))
	u, err := en.e.JobUsage(context.Background(), "tm-box-1")
	if err != nil || u.PeakMemoryBytes != 1<<30 || u.CPUSeconds != 2.5 {
		t.Fatalf("usage %+v %v", u, err)
	}
	if _, err := en.e.JobUsage(context.Background(), "tm-box-gone"); err == nil {
		t.Fatal("a missing cgroup read as usage")
	}
	if _, err := en.e.JobUsage(context.Background(), "../x"); err == nil {
		t.Fatal("bad name accepted")
	}
}

func TestJobLogFromJobFolder(t *testing.T) {
	en := newEnv(t)
	diag := filepath.Join(en.e.jobsDir(), "tm-box-1", "runner", "_diag")
	must(t, os.MkdirAll(diag, 0o755))
	must(t, os.WriteFile(filepath.Join(diag, "Runner_1.log"), []byte("Job test completed with result: Failed\n"), 0o644))
	l, err := en.e.JobLog(context.Background(), "tm-box-1", 1<<20)
	if err != nil || !strings.Contains(string(l.Data), "result: Failed") {
		t.Fatalf("log %q %v", l.Data, err)
	}
}
