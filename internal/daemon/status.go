package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/budget"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/mesh"
)

// Status is the daemon's view for `tentmaker status`, the control API and
// the pool.
type Status struct {
	Version     string                 `json:"version"`
	Device      string                 `json:"device"`
	Pool        string                 `json:"pool,omitempty"`
	Service     string                 `json:"service"` // "running", or "not running" when computed locally
	StartedAt   time.Time              `json:"started_at,omitempty"`
	Power       *core.PowerState       `json:"power,omitempty"`
	IdleS       int64                  `json:"idle_s"` // -1 when unknown
	Why         string                 `json:"why"`    // "boost: plugged in, idle 14m"
	Decision    core.Decision          `json:"decision"`
	InUse       core.Resources         `json:"in_use"`
	Paused      bool                   `json:"paused"`
	PauseReason string                 `json:"pause_reason,omitempty"`
	PauseUntil  *time.Time             `json:"pause_until,omitempty"`
	Worker      *dispatch.WorkerStatus `json:"worker,omitempty"`
	Broker      *dispatch.BrokerStatus `json:"broker,omitempty"`
	Mesh        MeshStatus             `json:"mesh"`
	// Wake is this device's LAN and the Wake-on-LAN state of the pool
	// devices it knows; absent when the mesh is off.
	Wake      *mesh.WakeStatus `json:"wake,omitempty"`
	Warnings  []string         `json:"warnings"`
	Problems  []core.Problem   `json:"problems"`
	LastError string           `json:"last_error,omitempty"`
	Next      string           `json:"next,omitempty"`
	// Runtime is the daemon process itself; absent when the service isn't
	// running and status was computed locally.
	Runtime *RuntimeStatus `json:"runtime,omitempty"`
}

// MeshStatus is this device's place in the pool.
type MeshStatus struct {
	Enabled bool   `json:"enabled"`
	Up      bool   `json:"up"`
	Self    string `json:"self,omitempty"`
	Warning string `json:"warning,omitempty"`
	// Pool is the pool this device works for: its config's, or the one it
	// joined as a lent device.
	Pool string `json:"pool,omitempty"`
	// Lent is the pool this device is lent to and its trust there.
	Lent *mesh.Lent `json:"lent,omitempty"`
	// Guests are the devices lent to this pool, revoked ones included.
	Guests []mesh.Guest `json:"guests,omitempty"`
}

// Why explains a decision in a few words: "boost: plugged in, idle 14m",
// "everyday: on battery at 80%", "off: on battery at 31%, below 40%".
func Why(d core.Decision, p *core.PowerState, idle time.Duration, idleKnown bool) string {
	var parts []string
	powerSaid := false
	for _, r := range d.Reasons {
		if strings.Contains(r, "battery") || strings.Contains(r, "power") {
			powerSaid = true
		}
	}
	if p != nil && p.Known && !powerSaid {
		switch {
		case p.OnAC && p.HasBattery:
			parts = append(parts, fmt.Sprintf("plugged in at %d%%", p.BatteryPercent))
		case p.OnAC:
			parts = append(parts, "plugged in")
		default:
			parts = append(parts, fmt.Sprintf("on battery at %d%%", p.BatteryPercent))
		}
	}
	if idleKnown {
		parts = append(parts, "idle "+ShortDuration(idle))
	}
	for _, r := range d.Reasons {
		if !containsFold(parts, r) {
			parts = append(parts, r)
		}
	}
	prof := d.Profile
	if prof == "" {
		prof = budget.Everyday
	}
	if len(parts) == 0 {
		return prof
	}
	return prof + ": " + strings.Join(parts, ", ")
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// ShortDuration writes d as "45s", "14m", "3h12m" or "2d4h".
func ShortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		days := int(d.Hours()) / 24
		h := int(d.Hours()) - days*24
		return fmt.Sprintf("%dd%dh", days, h)
	}
}

// NextStep is the one thing to do when something is wrong, or "".
func NextStep(s Status) string {
	for _, p := range s.Problems {
		if p.Fix != "" {
			return p.Fix
		}
	}
	if s.Paused {
		return "tentmaker resume (sudo) when you want CI back"
	}
	if s.Mesh.Enabled && !s.Mesh.Up {
		return "start Tailscale (`tailscale up`) to join the pool"
	}
	return ""
}
