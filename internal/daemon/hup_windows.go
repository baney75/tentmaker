package daemon

import "context"

// hupLoop does nothing on Windows, which has no SIGHUP; reload goes through
// the control API.
func (d *Daemon) hupLoop(ctx context.Context) {}
