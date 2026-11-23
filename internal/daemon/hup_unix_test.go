//go:build !windows

package daemon

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"

	"github.com/baney75/tentmaker/internal/config"
)

// SIGHUP to this test process reloads the config, like systemctl reload.
func TestSIGHUPReloads(t *testing.T) {
	// Catch SIGHUP here too, so one sent before the daemon listens can't
	// end the test process. The guard stays registered after the test: a
	// SIGHUP sent by the last resend can still be in flight when the test
	// returns, and with no channel left Go falls back to the default
	// action, which kills the whole test binary ("signal: hangup").
	guard := make(chan os.Signal, 16)
	signal.Notify(guard, syscall.SIGHUP)
	r := newRig(t, func(c *config.Config) { c.Mesh.Enabled = false })
	r.hup = true
	r.start()
	defer r.stop()
	ctx := context.Background()
	r.eventually("boost decision", func() bool { return r.d.Status(ctx).Decision.Limit.MemoryBytes == 8<<30 })
	r.cfg.Budget.Boost.Memory = 10 * config.GiB
	r.save()
	// The loop registers its handler when Run starts; resend until it lands.
	r.eventually("the reloaded budget", func() bool {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		return r.d.Status(ctx).Decision.Limit.MemoryBytes == 10<<30
	})
}
