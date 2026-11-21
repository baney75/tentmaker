//go:build live

package power

import (
	"context"
	"testing"
)

// Reads the real host. Run with: go test -tags live ./internal/power
func TestLiveHost(t *testing.T) {
	ctx := context.Background()
	st, err := New().Power(ctx)
	t.Logf("power: %+v, %v", st, err)
	if err != nil || !st.Known {
		t.Fatalf("power: %+v, %v", st, err)
	}
	idle, err := NewIdle().IdleFor(ctx)
	t.Logf("idle: %v, %v", idle, err)
	mem, err := TotalMemory()
	t.Logf("total memory: %d, %v; CPUs %d", mem, err, CPUs())
	if err != nil || mem <= 0 {
		t.Fatalf("total memory: %d, %v", mem, err)
	}
}
