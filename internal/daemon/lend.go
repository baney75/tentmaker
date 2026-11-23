package daemon

import (
	"context"
	"time"

	"github.com/baney75/tentmaker/internal/control"
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
