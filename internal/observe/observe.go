// Package observe renders a device's Prometheus metrics and the pool's
// read-only status page. The gauges come from the status `tentmaker status
// --json` prints (the daemon's Status, a peer's as the pool view carries
// it); the job counters from a Tally the daemon feeds as jobs end, so they
// never follow the run history's eviction; the page's jobs from the run
// history.
//
// The daemon serves both on the control API (GET /metrics and GET /), on
// the mesh peer API (this device's own data only) and, when configured, on
// a loopback metrics listener, behind each listener's own caller checks.
// Every free text it shows passes through runs.Redact, so a token that
// reached an error message or a job name still never leaves the device.
package observe

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/runs"
)

// Device is one device's status, the fields of the daemon's Status (its
// JSON, the contract of `tentmaker status --json`) that the views read.
type Device struct {
	Version     string                 `json:"version"`
	Device      string                 `json:"device"`
	Pool        string                 `json:"pool"`
	StartedAt   time.Time              `json:"started_at"`
	Why         string                 `json:"why"`
	Decision    core.Decision          `json:"decision"`
	Paused      bool                   `json:"paused"`
	PauseReason string                 `json:"pause_reason"`
	Worker      *dispatch.WorkerStatus `json:"worker"`
	Broker      *dispatch.BrokerStatus `json:"broker"`
	Problems    []core.Problem         `json:"problems"`
}

// ParseDevice reads a Status as JSON. ok is false when raw isn't one.
func ParseDevice(raw json.RawMessage) (Device, bool) {
	var d Device
	if len(raw) == 0 || json.Unmarshal(raw, &d) != nil || d.Device == "" {
		return Device{}, false
	}
	return d, true
}

// MetricsInput is what WriteMetrics reads.
type MetricsInput struct {
	Now  time.Time
	Self Device
	// Jobs are this device's job counters since the daemon started.
	Jobs JobCounts
	// BuildCache are the build cache's counters per tool since the daemon
	// started (internal/buildcache); none when the device serves none.
	BuildCache []BuildCacheCounts
}

// BuildCacheCounts are one tool's build cache counters.
type BuildCacheCounts struct {
	Tool                     string
	Hits, Misses             int64
	Stored, NotStored        int64
	BytesServed, BytesStored int64
}

// PageInput is what WritePage reads.
type PageInput struct {
	Now  time.Time
	Pool string
	// Devices is the pool view, this device included.
	Devices []mesh.DeviceStatus
	// Runs is the pool's recent run history, newest first, running jobs
	// included.
	Runs []runs.Record
	// Failures are the pool's failed jobs, newest first: every one that
	// ended in the 24 hours before Now, and at least the last MaxFailures
	// failures, read apart from Runs so newer successes can't push them
	// out.
	Failures []runs.Record
	// HistoryFrom, when not zero, is where some device's run history
	// starts because history_max dropped older jobs; failures before it
	// are not counted.
	HistoryFrom time.Time
	// Errors are devices whose history could not be read.
	Errors []string
}

// Source makes the two views' inputs. The daemon implements it.
type Source interface {
	MetricsInput(ctx context.Context) (MetricsInput, error)
	PageInput(ctx context.Context) (PageInput, error)
}

// viewTimeout bounds one view: the pool view asks every peer (2 s each,
// in parallel), and the mesh cuts an answer that takes 10 s.
const viewTimeout = 8 * time.Second

// Handler serves GET /metrics and the status page (GET / and GET
// /status) from src. It checks no caller: mount it behind a listener that
// does.
func Handler(src Source) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), viewTimeout)
		defer cancel()
		in, err := src.MetricsInput(ctx)
		if err != nil {
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
			return
		}
		body := Metrics(in)
		h := w.Header()
		h.Set("Content-Type", MetricsContentType)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(body)
	})
	page := func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), viewTimeout)
		defer cancel()
		in, err := src.PageInput(ctx)
		if err != nil {
			http.Error(w, "status unavailable", http.StatusServiceUnavailable)
			return
		}
		body, err := Page(in)
		if err != nil {
			http.Error(w, "status unavailable", http.StatusInternalServerError)
			return
		}
		SetPageHeaders(w.Header())
		_, _ = w.Write(body)
	}
	mux.HandleFunc("GET /{$}", page)
	mux.HandleFunc("GET /status", page)
	return mux
}

// SetPageHeaders sets the status page's headers: no caching, no framing,
// no referrer to the forge links, and a CSP that runs only the page's own
// style and script.
func SetPageHeaders(h http.Header) {
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", pageCSP)
}
