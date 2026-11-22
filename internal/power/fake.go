package power

import (
	"context"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// Fake is a core.PowerSource that returns State and Err. Other packages'
// tests use it in place of the host.
type Fake struct {
	State core.PowerState
	Err   error
}

// Power returns f.State and f.Err.
func (f Fake) Power(context.Context) (core.PowerState, error) { return f.State, f.Err }

// FakeIdle is a core.IdleSource that returns Idle and Err.
type FakeIdle struct {
	Idle time.Duration
	Err  error
}

// IdleFor returns f.Idle and f.Err.
func (f FakeIdle) IdleFor(context.Context) (time.Duration, error) { return f.Idle, f.Err }
