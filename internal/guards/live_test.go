//go:build live

package guards

import (
	"context"
	"os"
	"testing"

	"github.com/baney75/tentmaker/internal/config"
)

// Reads the real host. Run with: go test -tags live ./internal/guards
func TestLiveHost(t *testing.T) {
	ctx := context.Background()
	p := NewHostProbe(os.TempDir())
	mem, err := p.FreeMemory(ctx)
	t.Logf("free memory: %s, %v", FormatBytes(mem), err)
	disk, err2 := p.FreeDisk(ctx, "")
	t.Logf("free disk: %s, %v", FormatBytes(disk), err2)
	load, err3 := p.LoadPerCPU(ctx)
	t.Logf("load per CPU: %.2f, %v", load, err3)
	procs, err4 := p.Processes(ctx)
	t.Logf("processes: %d, %v", len(procs), err4)
	for _, e := range []error{err, err2, err3, err4} {
		if e != nil {
			t.Fatal(e)
		}
	}
	d := config.Default(mem, 8)
	r := Check(ctx, d.Guards, d.Job, p, os.TempDir(), 0)
	t.Logf("check: %+v", r)
}
