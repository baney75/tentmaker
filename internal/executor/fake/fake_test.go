package fake

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
)

var ctx = context.Background()

func spec(name string) core.ExecSpec {
	return core.ExecSpec{Name: name, Grant: core.Grant{Secret: []byte("s3cret")}}
}

func TestLifecycle(t *testing.T) {
	e := New("fake", 2, core.Platform{OS: "linux", Arch: "x64"})
	if err := e.Start(ctx, spec("tm-a-00000001")); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(ctx, spec("tm-a-00000001")); err == nil {
		t.Fatal("duplicate start accepted")
	}
	if err := e.Start(ctx, spec("Bad Name")); err == nil {
		t.Fatal("bad name accepted")
	}
	if st, _ := e.State(ctx, "tm-a-00000001"); st != core.ExecRunning {
		t.Fatalf("state = %s", st)
	}
	if st, _ := e.State(ctx, "tm-a-nothere1"); st != core.ExecMissing {
		t.Fatalf("unknown = %s", st)
	}
	if err := e.Reset(ctx); !errors.Is(err, ErrResetWhileRunning) || len(e.Violations()) != 1 {
		t.Fatalf("reset while running = %v", err)
	}
	e.Exit("tm-a-00000001")
	e.SetResult("tm-a-00000001", core.CheckResult{State: "success"})
	if r, err := e.CheckResult(ctx, "tm-a-00000001"); err != nil || r.State != "success" {
		t.Fatalf("result = %+v %v", r, err)
	}
	if _, err := e.CheckResult(ctx, "tm-a-00000002"); err == nil {
		t.Fatal("missing result read")
	}
	if err := e.Clean(ctx, "tm-a-00000001"); err != nil {
		t.Fatal(err)
	}
	if names, _ := e.List(ctx); len(names) != 0 {
		t.Fatalf("list = %v", names)
	}
	if err := e.Reset(ctx); err != nil || e.Resets() != 1 {
		t.Fatalf("reset = %v", err)
	}
}

func TestControls(t *testing.T) {
	e := New("fake", 1, core.Platform{OS: "linux", Arch: "x64"})
	boom := errors.New("boom")
	e.FailStarts(boom)
	if err := e.Start(ctx, spec("tm-a-00000001")); err != boom {
		t.Fatalf("first start = %v", err)
	}
	if err := e.Start(ctx, spec("tm-a-00000001")); err != nil {
		t.Fatalf("second start = %v", err)
	}
	if err := e.Start(ctx, spec("tm-a-00000002")); err == nil || len(e.Violations()) != 1 {
		t.Fatal("start beyond MaxSlots accepted")
	}
	s, _ := e.Spec("tm-a-00000001")
	if string(s.Grant.Secret) != "s3cret" {
		t.Fatal("secret not recorded")
	}
	e.SetStateErr("tm-a-00000001", boom)
	if st, err := e.State(ctx, "tm-a-00000001"); err == nil || st != core.ExecUnknown {
		t.Fatalf("state err = %s %v", st, err)
	}
	e.AddLeftover("tm-a-leftover")
	if names, _ := e.List(ctx); len(names) != 2 {
		t.Fatalf("list = %v", names)
	}
	_ = e.SetPoolLimit(ctx, core.Resources{MemoryBytes: 8, CPUs: 2}, core.Resources{MemoryBytes: 2, CPUs: 1})
	if l, u, n := e.PoolLimit(); n != 1 || l.CPUs != 2 || u.CPUs != 1 {
		t.Fatal("pool limit not recorded")
	}
	if len(e.Calls("Start")) != 3 || len(e.Calls("")) < 6 {
		t.Fatalf("calls = %v", e.Calls(""))
	}
}

func TestConcurrentUse(t *testing.T) {
	e := New("fake", 0, core.Platform{OS: "linux", Arch: "x64"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "tm-a-0000000" + string(rune('0'+i))
			_ = e.Start(ctx, spec(name))
			_, _ = e.State(ctx, name)
			_ = e.Stop(ctx, name)
			_ = e.Clean(ctx, name)
			_, _ = e.List(ctx)
		}(i)
	}
	wg.Wait()
}
