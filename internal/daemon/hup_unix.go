//go:build !windows

package daemon

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// hupLoop reloads the config on every SIGHUP until ctx ends.
func (d *Daemon) hupLoop(ctx context.Context) {
	if !d.o.ReloadOnHUP {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			d.log.Info("SIGHUP: reloading the config")
			if err := d.Reload(ctx); err != nil {
				d.log.Error("SIGHUP reload failed", "error", err)
			}
		}
	}
}
