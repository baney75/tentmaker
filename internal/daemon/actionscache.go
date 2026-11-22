package daemon

import (
	"context"
	"net/netip"

	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
)

// startCache serves the GitHub Actions cache on its own loopback listener.
// When it can't listen, jobs keep GitHub's cache: the worker grants no job
// while the service has no URL.
func (d *Daemon) startCache(ctx context.Context) *comp {
	ln, err := control.Listen(d.o.CacheAddr)
	if err != nil {
		d.setProblems("actions-cache", []core.Problem{{Check: "actions-cache",
			What: "the device's Actions cache is not served, so jobs use GitHub's: " + err.Error(),
			Fix:  "stop whatever uses " + d.o.CacheAddr + ", then restart the service"}})
		d.log.Error("actions cache not served", "error", err)
		return nil
	}
	return startComp(ctx, d.log, "actions-cache", func(ctx context.Context) error { return d.actions.Serve(ctx, ln) })
}

// uplinker is an executor whose jobs reach the host's loopback only through
// a per-job uplink process (the Linux executor's pasta).
type uplinker interface {
	UplinkCgroup(name string) string
	ProcDir() string
}

// cacheJobOf names the job behind a connection to the cache service, from
// /proc and the job's uplink cgroup. A connection no executor claims is no
// job's, and the service answers it 404.
func (d *Daemon) cacheJobOf() func(remote, local netip.AddrPort, jobs []string) (string, bool) {
	return func(remote, local netip.AddrPort, jobs []string) (string, bool) {
		d.mu.RLock()
		execs := d.execs
		d.mu.RUnlock()
		for _, ex := range execs {
			if u, ok := ex.(uplinker); ok {
				p := procPeers(u)
				if j, ok := p.JobOf(remote, local, jobs); ok {
					return j, true
				}
			}
		}
		return "", false
	}
}
