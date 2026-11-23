package guards

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/baney75/tentmaker/internal/config"
)

const gib = int64(config.GiB)

var (
	baseGuards = config.Guards{MinFreeDisk: 20 * config.GiB, StopBelowDisk: 5 * config.GiB, MinFreeMemory: 2 * config.GiB}
	baseJob    = config.Job{Memory: 2 * config.GiB, CPUs: 2}
	healthy    = Fake{Memory: 16 * gib, Disk: 100 * gib, Load: 0.2, Procs: []string{"launchd", "zsh"}}
)

// countingProbe counts reads of the probes that have an off switch.
type countingProbe struct {
	Fake
	loads, procs int
}

func (c *countingProbe) LoadPerCPU(ctx context.Context) (float64, error) {
	c.loads++
	return c.Fake.LoadPerCPU(ctx)
}

func (c *countingProbe) Processes(ctx context.Context) ([]string, error) {
	c.procs++
	return c.Fake.Processes(ctx)
}

func TestCheck(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		g       func(*config.Guards)
		probe   func(*Fake)
		running int
		admit   bool
		evict   bool
		busy    bool     // the user is using the machine: boost drops to everyday
		reasons []string // substrings, in order
	}{
		{name: "healthy", admit: true},
		{name: "memory exactly job plus min free", probe: func(f *Fake) { f.Memory = 4 * gib }, admit: true},
		{name: "memory one byte short", probe: func(f *Fake) { f.Memory = 4*gib - 1 }, admit: false,
			reasons: []string{"free memory 4 GiB is below 4 GiB (a 2 GiB job plus 2 GiB kept free)"}},
		{name: "memory low", probe: func(f *Fake) { f.Memory = 3*gib + gib/10 }, admit: false,
			reasons: []string{"free memory 3.1 GiB is below 4 GiB"}},
		{name: "memory probe fails closed", probe: func(f *Fake) { f.MemErr = boom }, admit: false,
			reasons: []string{"can't read free memory (boom), so no new jobs"}},
		{name: "disk exactly min free", probe: func(f *Fake) { f.Disk = 20 * gib }, admit: true},
		{name: "disk below min free", probe: func(f *Fake) { f.Disk = 19 * gib }, admit: false,
			reasons: []string{"free disk 19 GiB at /state is below 20 GiB"}},
		{name: "disk grows 1 GiB per running job", probe: func(f *Fake) { f.Disk = 22 * gib }, running: 3, admit: false,
			reasons: []string{"free disk 22 GiB at /state is below 23 GiB (20 GiB plus 1 GiB for each of 3 running jobs)"}},
		{name: "disk enough for running jobs", probe: func(f *Fake) { f.Disk = 23 * gib }, running: 3, admit: true},
		{name: "disk exactly stop level does not evict", probe: func(f *Fake) { f.Disk = 5 * gib }, admit: false},
		{name: "disk below stop evicts", probe: func(f *Fake) { f.Disk = 4 * gib }, admit: false, evict: true,
			reasons: []string{"below 20 GiB", "free disk 4 GiB at /state is below 5 GiB, stopping running jobs"}},
		{name: "stop below disk zero never evicts", g: func(g *config.Guards) { g.StopBelowDisk = 0 }, probe: func(f *Fake) { f.Disk = 0 }, admit: false},
		{name: "disk probe fails closed", probe: func(f *Fake) { f.DiskErr = boom }, admit: false,
			reasons: []string{"can't read free disk at /state (boom)"}},
		{name: "load off ignores high load", probe: func(f *Fake) { f.Load = 9; f.LoadErr = boom }, admit: true},
		{name: "load at limit", g: func(g *config.Guards) { g.MaxLoadPerCPU = 1 }, probe: func(f *Fake) { f.Load = 1 }, admit: true},
		{name: "load above limit", g: func(g *config.Guards) { g.MaxLoadPerCPU = 1 }, probe: func(f *Fake) { f.Load = 1.4 }, admit: true, busy: true,
			reasons: []string{"load 1.40 per CPU is above 1.00"}},
		{name: "load probe fails closed", g: func(g *config.Guards) { g.MaxLoadPerCPU = 1 }, probe: func(f *Fake) { f.LoadErr = boom }, admit: true, busy: true,
			reasons: []string{"can't read load (boom)"}},
		{name: "configured exe matches plain process", g: func(g *config.Guards) { g.PauseWhenRunning = []string{"Steam.exe"} },
			probe: func(f *Fake) { f.Procs = []string{"steam"} }, admit: true, busy: true, reasons: []string{"Steam.exe is running"}},
		{name: "plain name matches windows exe", g: func(g *config.Guards) { g.PauseWhenRunning = []string{"zoom"} },
			probe: func(f *Fake) { f.Procs = []string{"zoom.exe"} }, admit: true, busy: true, reasons: []string{"zoom is running"}},
		{name: "case-insensitive", g: func(g *config.Guards) { g.PauseWhenRunning = []string{"OBS"} },
			probe: func(f *Fake) { f.Procs = []string{"obs"} }, admit: true, busy: true, reasons: []string{"OBS is running"}},
		{name: "truncated linux comm matches long name", g: func(g *config.Guards) { g.PauseWhenRunning = []string{"wine64-preloader"} },
			probe: func(f *Fake) { f.Procs = []string{"wine64-preloade"} }, admit: true, busy: true, reasons: []string{"wine64-preloader is running"}},
		{name: "short prefix does not match", g: func(g *config.Guards) { g.PauseWhenRunning = []string{"steamwebhelper"} },
			probe: func(f *Fake) { f.Procs = []string{"steam"} }, admit: true},
		{name: "process not running", g: func(g *config.Guards) { g.PauseWhenRunning = []string{"steam.exe", "zoom"} }, admit: true},
		{name: "process list fails closed", g: func(g *config.Guards) { g.PauseWhenRunning = []string{"steam"} },
			probe: func(f *Fake) { f.ProcErr = boom }, admit: true, busy: true, reasons: []string{"can't read the process list (boom)"}},
		{name: "every tripped guard is listed",
			g:     func(g *config.Guards) { g.MaxLoadPerCPU = 0.5; g.PauseWhenRunning = []string{"steam"} },
			probe: func(f *Fake) { f.Memory = gib; f.Disk = 3 * gib; f.Load = 2; f.Procs = []string{"steam"} },
			admit: false, evict: true, busy: true,
			reasons: []string{"free memory", "below 20 GiB", "stopping running jobs", "load 2.00", "steam is running"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := baseGuards
			if tt.g != nil {
				tt.g(&g)
			}
			f := healthy
			if tt.probe != nil {
				tt.probe(&f)
			}
			r := Check(context.Background(), g, baseJob, f, "/state", tt.running)
			if r.Admit != tt.admit || r.Evict != tt.evict || r.Busy != tt.busy {
				t.Fatalf("admit=%v evict=%v busy=%v, want %v %v %v; reasons %q", r.Admit, r.Evict, r.Busy, tt.admit, tt.evict, tt.busy, r.Reasons)
			}
			if tt.admit && !tt.busy && len(r.Reasons) != 0 {
				t.Fatalf("admitted with reasons %q", r.Reasons)
			}
			if (!tt.admit || tt.busy) && len(r.Reasons) == 0 {
				t.Fatal("refused with no reason")
			}
			i := 0
			for _, want := range tt.reasons {
				for i < len(r.Reasons) && !strings.Contains(r.Reasons[i], want) {
					i++
				}
				if i == len(r.Reasons) {
					t.Fatalf("reason %q missing or out of order in %q", want, r.Reasons)
				}
			}
		})
	}
}

func TestCheckSkipsProbesThatAreOff(t *testing.T) {
	c := &countingProbe{Fake: healthy}
	Check(context.Background(), baseGuards, baseJob, c, "/state", 0)
	if c.loads != 0 || c.procs != 0 {
		t.Fatalf("loads=%d procs=%d, want 0 0", c.loads, c.procs)
	}
	g := baseGuards
	g.MaxLoadPerCPU = 1
	g.PauseWhenRunning = []string{"steam"}
	Check(context.Background(), g, baseJob, c, "/state", 0)
	if c.loads != 1 || c.procs != 1 {
		t.Fatalf("loads=%d procs=%d, want 1 1", c.loads, c.procs)
	}
}

func TestCheckNilProbeFailsClosed(t *testing.T) {
	r := Check(context.Background(), baseGuards, baseJob, nil, "/state", 0)
	if r.Admit || len(r.Reasons) == 0 {
		t.Fatalf("%+v", r)
	}
}

func TestNormalizeProcess(t *testing.T) {
	for in, want := range map[string]string{
		"Steam.exe": "steam", `C:\Program Files\Zoom\bin\Zoom.EXE`: "zoom", "/usr/bin/obs": "obs",
		"  Google Chrome ": "google chrome", "": "", "exe": "exe",
	} {
		if got := NormalizeProcess(in); got != want {
			t.Errorf("NormalizeProcess(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 MiB", 512 << 20: "512 MiB", gib: "1 GiB", 3*gib + gib/10: "3.1 GiB", 20 * gib: "20 GiB", -gib: "-1 GiB",
	} {
		if got := FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// vmStatAppleSilicon is real `vm_stat` output from an Apple silicon Mac.
const vmStatAppleSilicon = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                    10669.
Pages active:                                 559715.
Pages inactive:                               554546.
Pages speculative:                              4203.
Pages throttled:                                   0.
Pages wired down:                             348451.
Pages purgeable:                                9112.
"Translation faults":                    72833589696.
Pages copy-on-write:                      5460956467.
File-backed pages:                            364058.
Anonymous pages:                              754406.
Pages stored in compressor:                  1156707.
Pages occupied by compressor:                 565400.
`

const vmStatIntel = `Mach Virtual Memory Statistics: (page size of 4096 bytes)
Pages free:                               12000.
Pages active:                            900000.
Pages inactive:                          300000.
Pages speculative:                         8000.
Pages throttled:                              0.
Pages wired down:                        400000.
Pages purgeable:                           2000.
`

func TestParseVMStat(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int64
		wantErr bool
	}{
		{"apple silicon", vmStatAppleSilicon, (10669 + 554546 + 4203 + 9112) * 16384, false},
		{"intel", vmStatIntel, (12000 + 300000 + 8000 + 2000) * 4096, false},
		{"no page size", "Pages free: 10.\n", 0, true},
		{"no counts", "Mach Virtual Memory Statistics: (page size of 4096 bytes)\n", 0, true},
		{"bad count", "Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages free: lots.\n", 0, true},
		{"empty", "", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVMStat(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("got %d, %v; want %d, err %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestParseLoads(t *testing.T) {
	if v, err := parseProcLoadavg("0.52 0.58 0.59 1/1234 5678\n"); err != nil || v != 0.52 {
		t.Errorf("proc loadavg = %v, %v", v, err)
	}
	if v, err := parseSysctlLoadavg("{ 39.15 54.42 58.74 }\n"); err != nil || v != 39.15 {
		t.Errorf("sysctl loadavg = %v, %v", v, err)
	}
	for _, bad := range []string{"", "0.5", "x y z 1/2 3", "-1 0 0 1/1 1"} {
		if _, err := parseProcLoadavg(bad); err == nil {
			t.Errorf("parseProcLoadavg(%q) = nil error", bad)
		}
	}
	for _, bad := range []string{"", "1.0 2.0 3.0", "{ 1.0 }", "{ a b c }"} {
		if _, err := parseSysctlLoadavg(bad); err == nil {
			t.Errorf("parseSysctlLoadavg(%q) = nil error", bad)
		}
	}
	if _, err := perCPU(1, 0); err == nil {
		t.Error("perCPU with 0 CPUs = nil error")
	}
}

func TestParsePsComm(t *testing.T) {
	got, err := parsePsComm("COMM\nlaunchd\nGoogle Chrome Helper\nSteam\n\n")
	if err != nil || !reflect.DeepEqual(got, []string{"launchd", "google chrome helper", "steam"}) {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "launchd\n", "COMM\n"} {
		if _, err := parsePsComm(bad); err == nil {
			t.Errorf("parsePsComm(%q) = nil error", bad)
		}
	}
}

func TestCPUBusy(t *testing.T) {
	a := cpuTimes{idle: 1000, kernel: 2000, user: 1000}
	if v, err := cpuBusy(a, cpuTimes{idle: 1600, kernel: 2800, user: 1200}); err != nil || v != 0.4 {
		t.Errorf("busy = %v, %v; want 0.4", v, err)
	}
	if _, err := cpuBusy(a, a); err == nil {
		t.Error("no time passed: want error")
	}
	if _, err := cpuBusy(a, cpuTimes{idle: 0, kernel: 3000, user: 2000}); err == nil {
		t.Error("backwards: want error")
	}
}

func TestFreeDiskAt(t *testing.T) {
	// freeDiskAt cleans with filepath, so on Windows it asks for \var\lib.
	statfs := func(p string) (int64, error) {
		switch filepath.ToSlash(p) {
		case "/var/lib":
			return 42, nil
		case "/denied":
			return 0, fs.ErrPermission
		}
		return 0, fs.ErrNotExist
	}
	if n, err := freeDiskAt("/var/lib/tentmaker/jobs", statfs); err != nil || n != 42 {
		t.Errorf("walk up = %d, %v", n, err)
	}
	if _, err := freeDiskAt("/denied", statfs); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("permission error = %v", err)
	}
	if _, err := freeDiskAt("/nowhere", statfs); err == nil {
		t.Error("root missing: want error")
	}
	if _, err := freeDiskAt("", statfs); err == nil {
		t.Error("empty path: want error")
	}
}

func TestProcProbe(t *testing.T) {
	proc := fstest.MapFS{
		"meminfo":    {Data: []byte("MemTotal: 32000000 kB\nMemFree: 1000000 kB\nMemAvailable: 8000000 kB\n")},
		"loadavg":    {Data: []byte("6.00 5.00 4.00 3/900 1234\n")},
		"1/comm":     {Data: []byte("systemd\n")},
		"812/comm":   {Data: []byte("Steam\n")},
		"9001/comm":  {Data: []byte("wine64-preloade\n")},
		"self/comm":  {Data: []byte("tentmaker\n")},
		"sys/kernel": {Data: []byte("x")},
	}
	var asked string
	p := procProbe{stateDir: "/var/lib/tentmaker", proc: proc, cpus: func() int { return 8 },
		statfs: func(s string) (int64, error) { asked = s; return 7, nil }}
	ctx := context.Background()
	if n, err := p.FreeMemory(ctx); err != nil || n != 8000000*1024 {
		t.Errorf("FreeMemory = %d, %v", n, err)
	}
	if l, err := p.LoadPerCPU(ctx); err != nil || l != 0.75 {
		t.Errorf("LoadPerCPU = %v, %v", l, err)
	}
	if n, err := p.FreeDisk(ctx, ""); err != nil || n != 7 || filepath.ToSlash(asked) != "/var/lib/tentmaker" {
		t.Errorf("FreeDisk = %d, %v at %q", n, err, asked)
	}
	procs, err := p.Processes(ctx)
	if err != nil || !reflect.DeepEqual(procs, []string{"systemd", "steam", "wine64-preloade"}) {
		t.Errorf("Processes = %q, %v", procs, err)
	}
	empty := procProbe{proc: fstest.MapFS{}, cpus: func() int { return 1 }}
	if _, err := empty.FreeMemory(ctx); err == nil {
		t.Error("missing meminfo: want error")
	}
	if _, err := empty.LoadPerCPU(ctx); err == nil {
		t.Error("missing loadavg: want error")
	}
	if _, err := empty.Processes(ctx); err == nil {
		t.Error("no processes: want error")
	}
	if _, err := empty.FreeDisk(ctx, ""); err == nil {
		t.Error("no state dir: want error")
	}
}

func TestMacProbe(t *testing.T) {
	outs := map[string]string{
		"/usr/bin/vm_stat":               vmStatIntel,
		"/usr/sbin/sysctl -n vm.loadavg": "{ 2.00 1.50 1.00 }\n",
		"/bin/ps -axco comm":             "COMM\nlaunchd\nzoom.us\n",
	}
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, ok := outs[strings.Join(append([]string{name}, args...), " ")]
		if !ok {
			return nil, errors.New("timed out")
		}
		return []byte(out), nil
	}
	m := macProbe{stateDir: "/Library/Application Support/Tentmaker", run: run, cpus: func() int { return 4 },
		statfs: func(string) (int64, error) { return 9, nil }}
	ctx := context.Background()
	if n, err := m.FreeMemory(ctx); err != nil || n != (12000+300000+8000+2000)*4096 {
		t.Errorf("FreeMemory = %d, %v", n, err)
	}
	if l, err := m.LoadPerCPU(ctx); err != nil || l != 0.5 {
		t.Errorf("LoadPerCPU = %v, %v", l, err)
	}
	if p, err := m.Processes(ctx); err != nil || !reflect.DeepEqual(p, []string{"launchd", "zoom.us"}) {
		t.Errorf("Processes = %q, %v", p, err)
	}
	if n, err := m.FreeDisk(ctx, ""); err != nil || n != 9 {
		t.Errorf("FreeDisk = %d, %v", n, err)
	}
	outs = map[string]string{}
	if _, err := m.FreeMemory(ctx); err == nil {
		t.Error("vm_stat fails: want error")
	}
	if _, err := m.LoadPerCPU(ctx); err == nil {
		t.Error("sysctl fails: want error")
	}
	if _, err := m.Processes(ctx); err == nil {
		t.Error("ps fails: want error")
	}
}

// Each tripped guard has a fixed name next to its sentence, for metrics.
func TestCheckNamesGuards(t *testing.T) {
	boom := errors.New("boom")
	for _, tt := range []struct {
		name  string
		g     func(*config.Guards)
		probe func(*Fake)
		want  []string
	}{
		{name: "healthy"},
		{name: "memory low", probe: func(f *Fake) { f.Memory = gib }, want: []string{GuardMemory}},
		{name: "memory unreadable", probe: func(f *Fake) { f.MemErr = boom }, want: []string{GuardMemory}},
		{name: "disk below stop", probe: func(f *Fake) { f.Disk = 4 * gib }, want: []string{GuardDisk, GuardDiskStop}},
		{name: "load high", g: func(g *config.Guards) { g.MaxLoadPerCPU = 1 }, probe: func(f *Fake) { f.Load = 3 }, want: []string{GuardLoad}},
		{name: "process", g: func(g *config.Guards) { g.PauseWhenRunning = []string{"steam", "zsh", "launchd"} }, want: []string{GuardProcess}},
		{name: "all", g: func(g *config.Guards) { g.MaxLoadPerCPU = 0.5; g.PauseWhenRunning = []string{"steam"} },
			probe: func(f *Fake) { f.Memory = gib; f.Disk = 3 * gib; f.Load = 2; f.Procs = []string{"steam"} },
			want:  []string{GuardMemory, GuardDisk, GuardDiskStop, GuardLoad, GuardProcess}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := baseGuards
			if tt.g != nil {
				tt.g(&g)
			}
			f := healthy
			if tt.probe != nil {
				tt.probe(&f)
			}
			r := Check(context.Background(), g, baseJob, f, "/state", 0)
			if !reflect.DeepEqual(r.Guards, tt.want) {
				t.Fatalf("guards %q, want %q (reasons %q)", r.Guards, tt.want, r.Reasons)
			}
		})
	}
	if r := Check(context.Background(), baseGuards, baseJob, nil, "/state", 0); !reflect.DeepEqual(r.Guards, []string{GuardProbe}) {
		t.Fatalf("nil probe guards %q", r.Guards)
	}
}
