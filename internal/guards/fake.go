package guards

import "context"

// Fake is a Probe with fixed readings. A nil error field means the reading
// succeeds. Other packages' tests use it in place of the host.
type Fake struct {
	Memory  int64
	Disk    int64
	Load    float64
	Procs   []string
	MemErr  error
	DiskErr error
	LoadErr error
	ProcErr error
}

func (f Fake) FreeMemory(context.Context) (int64, error)       { return f.Memory, f.MemErr }
func (f Fake) FreeDisk(context.Context, string) (int64, error) { return f.Disk, f.DiskErr }
func (f Fake) LoadPerCPU(context.Context) (float64, error)     { return f.Load, f.LoadErr }
func (f Fake) Processes(context.Context) ([]string, error)     { return f.Procs, f.ProcErr }
