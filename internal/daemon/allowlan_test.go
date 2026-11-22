package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	fakeexec "github.com/baney75/tentmaker/internal/executor/fake"
)

// fenceHost stands in for the host's loaded network rules. The executors
// it builds check them against their own allow_lan at Start and in
// Preflight, as the Linux and Windows executors' checkNet does: rules that
// open the LAN when the config says no refuse the job.
type fenceHost struct {
	mu       sync.Mutex
	rulesLAN bool   // the loaded rules were written with allow_lan = true
	built    []bool // allow_lan of each executor built
	last     *fakeexec.Executor
}

var errRulesLAN = errors.New("the loaded network rules for CI jobs lack 10.0.0.0/8")

type fencedExec struct {
	*fakeexec.Executor
	h        *fenceHost
	allowLAN bool
}

func (e fencedExec) mismatch() bool {
	e.h.mu.Lock()
	defer e.h.mu.Unlock()
	return e.h.rulesLAN && !e.allowLAN
}

func (e fencedExec) Start(ctx context.Context, spec core.ExecSpec) error {
	if e.mismatch() {
		return errors.New(errRulesLAN.Error() + "; run sudo tentmaker setup")
	}
	return e.Executor.Start(ctx, spec)
}

func (e fencedExec) Preflight(ctx context.Context) []core.Problem {
	if e.mismatch() {
		return []core.Problem{{Check: "network", What: errRulesLAN.Error(), Fix: "run sudo tentmaker setup"}}
	}
	return nil
}

func (h *fenceHost) factory(cfg config.Config, _ func(core.Platform) (string, error), _ *slog.Logger) ([]core.Executor, []core.Problem) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ex := fakeexec.New("fake-linux", 0, linuxX64)
	h.built = append(h.built, cfg.Guards.AllowLAN)
	h.last = ex
	return []core.Executor{fencedExec{Executor: ex, h: h, allowLAN: cfg.Guards.AllowLAN}}, nil
}

func (h *fenceHost) snapshot() ([]bool, *fakeexec.Executor) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bool(nil), h.built...), h.last
}

// Turning allow_lan off and reloading must rebuild the executors with the
// new policy, so the next job is checked against it and refused while the
// loaded rules still open the LAN. Before, the worker key left allow_lan
// out, the old executors stayed, and jobs kept running under the weaker
// policy. Review r1 partB sol-F1.
func TestDaemonReloadAllowLANRebuildsExecutors(t *testing.T) {
	h := &fenceHost{rulesLAN: true}
	r := newRig(t, func(c *config.Config) { c.Guards.AllowLAN = true })
	r.executors = h.factory
	r.start()
	defer r.stop()
	if built, _ := h.snapshot(); len(built) != 1 || !built[0] {
		t.Fatalf("executors built %v, want [true]", built)
	}

	r.cfg.Guards.AllowLAN = false
	r.save()
	if err := r.client().Post(context.Background(), "/v1/reload", nil); err != nil {
		t.Fatal(err)
	}
	built, ex := h.snapshot()
	if len(built) != 2 || built[1] {
		t.Fatalf("after turning allow_lan off, executors built %v, want [true false]", built)
	}

	// Doctor checks the rules against the new policy and says to rerun
	// setup.
	rep := r.d.Doctor(context.Background())
	found := false
	for _, f := range rep.Checks {
		if f.Check == "network" && f.Status == Fail && strings.Contains(f.Fix, "setup") {
			found = true
		}
	}
	if !found {
		t.Errorf("doctor does not flag the rules: %+v", rep.Checks)
	}

	// A job arrives: the new executor checks the rules and refuses it.
	r.forge.AddJob(core.Job{Repo: "baney75/app", Name: "build", Labels: []string{"self-hosted", "tentmaker", "linux"}})
	r.eventually("a refused start", func() bool {
		st := r.d.Status(context.Background())
		return st.Worker != nil && len(st.Worker.Executors) == 1 && strings.Contains(st.Worker.Executors[0].LastError, "run sudo tentmaker setup")
	})
	time.Sleep(200 * time.Millisecond)
	if n := len(ex.Running()); n != 0 {
		t.Fatalf("%d jobs run under rules that open the LAN", n)
	}
}

// Doctor fails when someone other than root could change the binary the
// root service runs (an older setup pointed it at ~/.local/bin). Review r1
// partB second-F7.
func TestDoctorFlagsUserWritableServiceBinary(t *testing.T) {
	in := DoctorInput{Binary: "/home/alice/.local/bin/tentmaker", BinaryProblem: func(p string) (string, error) {
		if strings.HasPrefix(p, "/home/alice/") {
			return p + " is owned by uid 1000, not root", nil
		}
		return "", nil
	}}
	f := checkBinary(in)
	if len(f) != 1 || f[0].Status != Fail || !strings.Contains(f[0].Fix, "sudo tentmaker setup") {
		t.Fatalf("user-writable: %+v", f)
	}
	in.Binary = "/usr/local/bin/tentmaker"
	if f := checkBinary(in); len(f) != 1 || f[0].Status != OK {
		t.Fatalf("root-owned: %+v", f)
	}
	if f := checkBinary(DoctorInput{Binary: "/x"}); f != nil {
		t.Fatalf("no checker: %+v", f)
	}
}
