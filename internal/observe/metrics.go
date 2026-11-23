package observe

import (
	"bytes"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/budget"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/runs"
)

// MetricsContentType is the Prometheus text exposition format, version
// 0.0.4, which every Prometheus and compatible scraper reads.
const MetricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// LatencyBuckets are the job start latency histogram's upper bounds, in
// seconds: queued at the forge to running on the device.
var LatencyBuckets = []float64{1, 2, 5, 10, 15, 30, 60, 120, 300, 600}

// KnownGuards lists every guard tentmaker_guard_active reports, 0 or 1, so
// a guard that is not tripped is a 0 rather than a missing series.
var KnownGuards = []string{
	budget.GuardPower, budget.GuardBattery, budget.GuardBatteryStop,
	guards.GuardProbe, guards.GuardMemory, guards.GuardDisk, guards.GuardDiskStop,
	guards.GuardLoad, guards.GuardProcess,
}

// knownResults are the job results tentmaker_jobs_finished_total always
// reports.
var knownResults = []string{runs.Success, runs.Failure, runs.Cancelled, runs.Error, runs.Unknown}

// Metrics writes this device's metrics in the Prometheus text format.
func Metrics(in MetricsInput) []byte {
	m := &metricWriter{}
	s := in.Self
	dev := s.Device
	d := s.Decision

	m.family("tentmaker_info", "gauge", "This device: its name, Tentmaker version and pool. Always 1.")
	m.sample("tentmaker_info", 1, "device", dev, "version", s.Version, "pool", s.Pool)

	m.family("tentmaker_paused", "gauge", "1 when CI is paused on this device by tentmaker pause.")
	m.sample("tentmaker_paused", b2f(s.Paused), "device", dev)

	m.family("tentmaker_budget_profile", "gauge", "The budget profile in force (everyday, boost, off or a schedule name). Always 1.")
	m.sample("tentmaker_budget_profile", 1, "device", dev, "profile", orDefault(d.Profile, budget.Everyday))

	m.family("tentmaker_budget_cpus", "gauge", "CPUs CI may use in total on this device now.")
	m.sample("tentmaker_budget_cpus", d.Limit.CPUs, "device", dev)

	m.family("tentmaker_budget_memory_bytes", "gauge", "Memory CI may use in total on this device now.")
	m.sample("tentmaker_budget_memory_bytes", float64(d.Limit.MemoryBytes), "device", dev)

	m.family("tentmaker_budget_admit", "gauge", "1 when the device may start new jobs.")
	m.sample("tentmaker_budget_admit", b2f(d.Admit), "device", dev)

	m.family("tentmaker_budget_evict", "gauge", "1 when a hard guard stops running jobs.")
	m.sample("tentmaker_budget_evict", b2f(d.Evict), "device", dev)

	m.family("tentmaker_guard_active", "gauge", "1 for each guard that holds CI back now.")
	active := map[string]bool{}
	for _, g := range d.Guards {
		active[g] = true
	}
	names := append([]string(nil), KnownGuards...)
	for g := range active {
		if !contains(names, g) {
			names = append(names, g)
		}
	}
	sort.Strings(names)
	for _, g := range names {
		m.sample("tentmaker_guard_active", b2f(active[g]), "device", dev, "guard", g)
	}

	m.family("tentmaker_problems", "gauge", "Problems status reports on this device (tentmaker doctor says how to fix each).")
	m.sample("tentmaker_problems", float64(len(s.Problems)), "device", dev)

	if w := s.Worker; w != nil {
		m.family("tentmaker_slots", "gauge", "Job slots on this device: total the budget allows, used and free now.")
		m.sample("tentmaker_slots", float64(w.Slots.Total), "device", dev, "state", "total")
		m.sample("tentmaker_slots", float64(w.Slots.Used), "device", dev, "state", "used")
		m.sample("tentmaker_slots", float64(w.Slots.Free), "device", dev, "state", "free")

		m.family("tentmaker_jobs_running", "gauge", "Jobs running on this device.")
		m.sample("tentmaker_jobs_running", float64(len(w.Running)), "device", dev)

		m.family("tentmaker_executor_ready", "gauge", "1 when the executor can take a job now.")
		for _, e := range w.Executors {
			m.sample("tentmaker_executor_ready", b2f(e.Ready), "device", dev, "executor", e.Name)
		}
	}

	if b := s.Broker; b != nil {
		m.family("tentmaker_jobs_queued", "gauge", "Jobs waiting at the forge for a runner, by repo, as this device's broker last read them (checks mode: planned jobs with no status yet).")
		for _, r := range b.Repos {
			n := r.Queued
			if r.Mode == "checks" {
				n = r.Pending
			}
			m.sample("tentmaker_jobs_queued", float64(n), "device", dev, "forge", r.Forge, "repo", r.Name, "mode", r.Mode)
		}

		m.family("tentmaker_broker_device_runners", "gauge", "Runners this broker has granted to each device.")
		for _, x := range b.Devices {
			m.sample("tentmaker_broker_device_runners", float64(x.Runners), "device", dev, "worker", x.Name)
		}
		m.family("tentmaker_broker_device_online", "gauge", "1 when this broker heard from the device in the last 2 minutes.")
		for _, x := range b.Devices {
			m.sample("tentmaker_broker_device_online", b2f(x.Online), "device", dev, "worker", x.Name)
		}
		m.family("tentmaker_broker_device_quarantined", "gauge", "1 when this broker gives the device no work (it lost track of a job).")
		for _, x := range b.Devices {
			m.sample("tentmaker_broker_device_quarantined", b2f(x.Quarantined), "device", dev, "worker", x.Name)
		}

		forges := make([]string, 0, len(b.API))
		for f := range b.API {
			forges = append(forges, f)
		}
		sort.Strings(forges)
		apiCounter := func(name, help string, v func(core.APIStats) int64) {
			m.family(name, "counter", help)
			for _, f := range forges {
				m.sample(name, float64(v(b.API[f])), "device", dev, "forge", f)
			}
		}
		apiCounter("tentmaker_forge_api_requests_total", "Forge API requests this broker sent since it started.", func(a core.APIStats) int64 { return a.Requests })
		apiCounter("tentmaker_forge_api_billed_total", "Forge API requests the rate limit counts (every request but a 304).", func(a core.APIStats) int64 { return a.Billed })
		apiCounter("tentmaker_forge_api_not_modified_total", "Conditional forge API requests answered 304, which cost no rate limit.", func(a core.APIStats) int64 { return a.NotModified })
		apiCounter("tentmaker_forge_api_rate_limited_total", "Forge API calls refused for the rate limit, by the forge or held back locally.", func(a core.APIStats) int64 { return a.RateLimited })

		m.family("tentmaker_forge_api_rate_remaining", "gauge", "Forge API requests left before the rate limit resets, as the forge last said.")
		for _, f := range forges {
			if a := b.API[f]; a.RateLeft >= 0 && a.RateLimit > 0 {
				m.sample("tentmaker_forge_api_rate_remaining", float64(a.RateLeft), "device", dev, "forge", f)
			}
		}
		m.family("tentmaker_forge_api_rate_limit", "gauge", "The forge API rate limit per window, as the forge last said.")
		for _, f := range forges {
			if a := b.API[f]; a.RateLimit > 0 {
				m.sample("tentmaker_forge_api_rate_limit", float64(a.RateLimit), "device", dev, "forge", f)
			}
		}
		m.family("tentmaker_forge_api_rate_reset_timestamp_seconds", "gauge", "When the forge API rate limit resets, in Unix seconds.")
		for _, f := range forges {
			if a := b.API[f]; !a.RateReset.IsZero() {
				m.sample("tentmaker_forge_api_rate_reset_timestamp_seconds", float64(a.RateReset.Unix()), "device", dev, "forge", f)
			}
		}
	}

	writeJobMetrics(m, in)
	return m.buf.Bytes()
}

// writeJobMetrics counts the jobs this device ran and ended since the
// daemon started: results, start latency and cache outcomes. A runner job
// still waiting for the forge's result is left out until it has one, so a
// result never moves from one series to another.
func writeJobMetrics(m *metricWriter, in MetricsInput) {
	dev := in.Self.Device
	results := map[string]int{}
	type cacheKey struct{ cache, result string }
	caches := map[cacheKey]int{}
	var waits []float64
	for _, r := range in.Runs {
		if r.Device != dev || !r.Ended() || r.AwaitingForge(in.Now) {
			continue
		}
		if !in.Self.StartedAt.IsZero() && r.EndedAt.Before(in.Self.StartedAt) {
			continue
		}
		results[r.Conclusion]++
		if !r.QueuedAt.IsZero() && !r.StartedAt.IsZero() {
			waits = append(waits, max(0, r.StartedAt.Sub(r.QueuedAt).Seconds()))
		}
		if r.RepoCache != "" {
			caches[cacheKey{"repo", r.RepoCache}]++
		}
		if r.ContainerStore != "" {
			caches[cacheKey{"container_store", r.ContainerStore}]++
		}
	}

	m.family("tentmaker_jobs_finished_total", "counter", "Jobs that ran on this device and ended since the daemon started, by result.")
	names := append([]string(nil), knownResults...)
	for c := range results {
		if !contains(names, c) {
			names = append(names, c)
		}
	}
	sort.Strings(names)
	for _, c := range names {
		m.sample("tentmaker_jobs_finished_total", float64(results[c]), "device", dev, "result", c)
	}

	const lat = "tentmaker_job_start_latency_seconds"
	m.family(lat, "histogram", "Seconds from a job queued at the forge to it running on this device, for jobs ended since the daemon started.")
	sort.Float64s(waits)
	sum := 0.0
	for _, v := range waits {
		sum += v
	}
	i := 0
	for _, le := range LatencyBuckets {
		for i < len(waits) && waits[i] <= le {
			i++
		}
		m.sample(lat+"_bucket", float64(i), "device", dev, "le", formatFloat(le))
	}
	m.sample(lat+"_bucket", float64(len(waits)), "device", dev, "le", "+Inf")
	m.sample(lat+"_sum", sum, "device", dev)
	m.sample(lat+"_count", float64(len(waits)), "device", dev)

	m.family("tentmaker_cache_jobs_total", "counter", "Jobs ended since the daemon started that had a repo cache (cache=repo) or used containers (cache=container_store), by whether they started warm.")
	for _, c := range []string{"repo", "container_store"} {
		for _, res := range []string{core.CacheHit, core.CacheMiss} {
			m.sample("tentmaker_cache_jobs_total", float64(caches[cacheKey{c, res}]), "device", dev, "cache", c, "result", res)
		}
	}
}

// metricWriter writes the text format: a HELP and TYPE line per family,
// then its samples.
type metricWriter struct{ buf bytes.Buffer }

func (m *metricWriter) family(name, typ, help string) {
	m.buf.WriteString("# HELP " + name + " " + escapeHelp(help) + "\n")
	m.buf.WriteString("# TYPE " + name + " " + typ + "\n")
}

// sample writes one line; labels are name, value pairs. Every label value
// is redacted, since repo, device and executor names come from config and
// peers.
func (m *metricWriter) sample(name string, v float64, labels ...string) {
	m.buf.WriteString(name)
	if len(labels) > 0 {
		m.buf.WriteByte('{')
		for i := 0; i+1 < len(labels); i += 2 {
			if i > 0 {
				m.buf.WriteByte(',')
			}
			m.buf.WriteString(labels[i])
			m.buf.WriteString(`="`)
			m.buf.WriteString(escapeLabel(clean(labels[i+1], 200)))
			m.buf.WriteByte('"')
		}
		m.buf.WriteByte('}')
	}
	m.buf.WriteByte(' ')
	m.buf.WriteString(formatFloat(v))
	m.buf.WriteByte('\n')
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		// Whole numbers in full: memory in bytes reads 17179869184, not
		// 1.7179869184e+10.
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// clean is free text fit to show: redacted, one line, at most n runes.
func clean(s string, n int) string {
	s = runs.RedactString(s)
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if rs := []rune(s); len(rs) > n {
		s = string(rs[:n-1]) + "…"
	}
	return s
}

// ago says how long before now t was: "45s", "14m", "3h12m", "2d4h".
func ago(now, t time.Time) string {
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) - h*60; m > 0 {
			return strconv.Itoa(h) + "h" + strconv.Itoa(m) + "m"
		}
		return strconv.Itoa(h) + "h"
	default:
		days := int(d.Hours()) / 24
		return strconv.Itoa(days) + "d" + strconv.Itoa(int(d.Hours())-days*24) + "h"
	}
}
