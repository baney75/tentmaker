package daemon

import (
	"context"
	"slices"
	"time"

	"github.com/baney75/tentmaker/internal/budget"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/mesh"
)

// wakeInfo is this device's Wake-on-LAN facts for its /v1/hello: its LAN,
// power, opt-out, pause (do not disturb), and what its worker can run.
func (d *Daemon) wakeInfo(ctx context.Context) mesh.WakeInfo {
	d.mu.RLock()
	cfg, w, execs := d.cfg, d.worker, d.execs
	d.mu.RUnlock()
	info := mesh.WakeInfo{
		OptOut: !cfg.Mesh.Wake,
		Labels: dispatch.WorkerLabels(cfg.Device, cfg.Labels),
		Slots:  budget.PluggedSlots(cfg),
	}
	info.Schedule = slices.Clone(cfg.Budget.Schedule)
	_, info.UTCOffsetS = time.Now().Zone()
	if lan, err := d.o.LAN(); err != nil {
		info.LANError = err.Error()
	} else {
		info.LAN = lan
	}
	if p, err := d.o.Power.Power(ctx); err == nil {
		info.Power = &p
	}
	if w != nil {
		info.DoNotDisturb = w.Status().Paused
	}
	for _, e := range execs {
		for _, p := range e.Platforms() {
			if !slices.Contains(info.Platforms, p) {
				info.Platforms = append(info.Platforms, p)
			}
		}
	}
	return info
}

// wakeNeeds is the local broker's waiting jobs, as the mesh reads them.
func (d *Daemon) wakeNeeds() []mesh.WakeNeed {
	b := d.currentBroker()
	if b == nil {
		return nil
	}
	var out []mesh.WakeNeed
	for _, j := range b.Waiting() {
		out = append(out, mesh.WakeNeed{
			Key: j.Key, Job: j.Repo + ": " + j.Name, Since: j.Since,
			Fits: func(labels []string, platforms []core.Platform) bool { return j.Fits(labels, platforms) },
		})
	}
	return out
}
