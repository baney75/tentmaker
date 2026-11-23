package daemon

import (
	"context"
	"time"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
)

// lendTimeout bounds invite, leave, devices and remove: each is a state
// file write and, at most, a few calls to peers that have their own limits.
const lendTimeout = 30 * time.Second

// Invite makes a one-time code to lend a device to this pool
// (control.LendHandler).
func (h ctlHandler) Invite(ctx context.Context, req control.InviteRequest) (any, error) {
	n := h.d.currentNode()
	if n == nil {
		return nil, control.ErrMeshOff
	}
	ctx, cancel := context.WithTimeout(ctx, lendTimeout)
	defer cancel()
	return n.Invite(ctx, req.Trust, req.Repos, time.Duration(req.TTLS)*time.Second)
}

// Join lends this device to the pool whose code it is.
func (h ctlHandler) Join(ctx context.Context, code string) (any, error) {
	n := h.d.currentNode()
	if n == nil {
		return nil, control.ErrMeshOff
	}
	ctx, cancel := context.WithTimeout(ctx, control.JoinTimeout)
	defer cancel()
	l, err := n.Join(ctx, code)
	if err != nil {
		return nil, err
	}
	h.d.kickWorker()
	return l, nil
}

// Leave drops this device out of the pool it joined.
func (h ctlHandler) Leave(ctx context.Context) (any, error) {
	n := h.d.currentNode()
	if n == nil {
		return nil, control.ErrMeshOff
	}
	ctx, cancel := context.WithTimeout(ctx, lendTimeout)
	defer cancel()
	l, err := n.Leave(ctx)
	if err != nil {
		return nil, err
	}
	h.d.kickWorker()
	return l, nil
}

// Devices lists the pool's devices, lent ones included.
func (h ctlHandler) Devices(ctx context.Context) (any, error) {
	n := h.d.currentNode()
	if n == nil {
		return nil, control.ErrMeshOff
	}
	ctx, cancel := context.WithTimeout(ctx, lendTimeout)
	defer cancel()
	return n.Devices(ctx), nil
}

// RemoveDevice revokes a lent device at once.
func (h ctlHandler) RemoveDevice(ctx context.Context, name string) (any, error) {
	n := h.d.currentNode()
	if n == nil {
		return nil, control.ErrMeshOff
	}
	ctx, cancel := context.WithTimeout(ctx, lendTimeout)
	defer cancel()
	return n.RevokeGuest(ctx, name)
}

var _ control.LendHandler = ctlHandler{}

// lentStatus is what this device shows the pool it is lent to: its version,
// whether it takes work, and its slots. Not its repos, its jobs (from any
// source), its schedule names, problems or warnings: those are the
// lender's own.
func (d *Daemon) lentStatus(ctx context.Context) any {
	s := d.Status(ctx)
	prof := s.Decision.Profile
	switch prof {
	case "everyday", "boost", "off":
	default:
		prof = "schedule"
	}
	out := Status{Version: s.Version, Device: s.Device, Service: s.Service, Paused: s.Paused,
		Decision: core.Decision{Profile: prof, Slots: s.Decision.Slots, Admit: s.Decision.Admit, Reasons: []string{}},
		Warnings: []string{}, Problems: []core.Problem{}}
	if s.Worker != nil {
		out.Worker = &dispatch.WorkerStatus{Device: s.Worker.Device, Slots: s.Worker.Slots, Paused: s.Worker.Paused}
	}
	return out
}
