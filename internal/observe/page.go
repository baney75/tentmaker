package observe

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/budget"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/runs"
)

//go:embed page.tmpl
var pageTmpl string

//go:embed page.css
var pageStyle string

//go:embed page.js
var pageScript string

//go:embed mark.svg
var markSVG string

// RefreshSeconds is how often an open status page fetches itself again.
// Its script backs off to 5 minutes while fetches fail, waits while the tab
// is hidden and never has two fetches in flight.
const RefreshSeconds = 15

// MaxFailures is how many recent failed jobs the page lists.
const MaxFailures = 10

var (
	tmpl    = template.Must(template.New("page").Parse(pageTmpl))
	pageCSP = "default-src 'none'; style-src " + cspHash(pageStyle) + "; script-src " + cspHash(pageScript) +
		"; img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	iconURL = template.URL("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(markSVG)))
)

func cspHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

type pageData struct {
	CSS     template.CSS
	JS      template.JS
	Icon    template.URL
	Mark    template.HTML
	Every   int
	Pool    string
	Updated string
	Stamp   string

	Online, Total, Running, Queued, Failed int

	Devices  []deviceView
	Jobs     []jobView
	Queue    []queueView
	Failures []jobView
	// Kept says how far back the history the failure count reads goes,
	// when history_max cut it inside the last 24 hours.
	Kept   string
	APIs   []apiView
	Errors []string
}

type deviceView struct {
	Name, Version, Profile, Why, Limits, Slots, Pause, Error, Next string
	Self, Broker, Offline, Admit                                   bool
	Guards                                                         []string
	Problems                                                       int
}

type jobView struct {
	Repo, Job, Device, URL, Age, At, Conclusion, Step string
}

type queueView struct {
	Repo, Forge, Mode string
	N                 int
}

type apiView struct {
	Device, Forge, Left, Reset string
	Low                        bool
}

// Page renders the pool's status page.
func Page(in PageInput) ([]byte, error) {
	now := in.Now
	p := pageData{
		CSS: template.CSS(pageStyle), JS: template.JS(pageScript), Icon: iconURL, Mark: template.HTML(markSVG),
		Every: RefreshSeconds, Pool: clean(in.Pool, 80),
		Updated: now.UTC().Format("15:04:05 UTC"), Stamp: now.UTC().Format(time.RFC3339),
	}
	queued := map[string]queueView{}
	for _, ds := range in.Devices {
		p.Total++
		v := deviceView{Name: clean(ds.Device, 80), Version: clean(ds.Version, 40), Self: ds.Self, Broker: ds.Broker}
		d, ok := ParseDevice(ds.Status)
		if !ok {
			v.Offline = true
			v.Error = clean(orDefault(ds.Error, "no status"), 200)
			p.Devices = append(p.Devices, v)
			continue
		}
		p.Online++
		dec := d.Decision
		v.Profile = clean(orDefault(dec.Profile, budget.Everyday), 40)
		v.Why = clean(d.Why, 300)
		v.Admit = dec.Admit && !d.Paused
		v.Limits = fmt.Sprintf("%g CPUs, %s", dec.Limit.CPUs, guards.FormatBytes(dec.Limit.MemoryBytes))
		if d.Paused {
			v.Pause = "Paused" + suffix(clean(d.PauseReason, 120))
		}
		for _, g := range dec.Guards {
			v.Guards = append(v.Guards, clean(g, 40))
		}
		v.Problems = len(d.Problems)
		for _, pr := range d.Problems {
			if pr.Fix != "" {
				v.Next = clean(pr.Fix, 200)
				break
			}
		}
		if w := d.Worker; w != nil {
			v.Slots = fmt.Sprintf("%d of %d slots used", w.Slots.Used, w.Slots.Total)
			p.Running += len(w.Running)
		}
		if b := d.Broker; b != nil {
			for _, r := range b.Repos {
				n := r.Queued
				if r.Mode == "checks" {
					n = r.Pending
				}
				// Two brokers can poll one repo: count it once.
				k := r.Forge + "\x00" + r.Name
				if q, seen := queued[k]; !seen || n > q.N {
					queued[k] = queueView{Repo: clean(r.Name, 120), Forge: clean(r.Forge, 40), Mode: clean(r.Mode, 20), N: n}
				}
			}
			forges := make([]string, 0, len(b.API))
			for f := range b.API {
				forges = append(forges, f)
			}
			sort.Strings(forges)
			for _, f := range forges {
				a := b.API[f]
				av := apiView{Device: v.Name, Forge: clean(f, 40), Left: "unknown"}
				if a.RateLimit > 0 && a.RateLeft >= 0 {
					av.Left = fmt.Sprintf("%d of %d", a.RateLeft, a.RateLimit)
					av.Low = a.RateLeft*10 < a.RateLimit
				}
				if !a.RateReset.IsZero() && a.RateReset.After(now) {
					av.Reset = "resets in " + ago(a.RateReset, now)
				}
				p.APIs = append(p.APIs, av)
			}
		}
		p.Devices = append(p.Devices, v)
	}
	sort.SliceStable(p.Devices, func(i, k int) bool {
		a, b := p.Devices[i], p.Devices[k]
		if a.Self != b.Self {
			return a.Self
		}
		return a.Name < b.Name
	})
	for _, q := range queued {
		if q.N > 0 {
			p.Queue = append(p.Queue, q)
			p.Queued += q.N
		}
	}
	sort.Slice(p.Queue, func(i, k int) bool { return p.Queue[i].Repo < p.Queue[k].Repo })

	recs := append([]runs.Record(nil), in.Runs...)
	runs.SortNewest(recs)
	for _, r := range recs {
		if r.Conclusion == runs.Running {
			j := job(r, now)
			j.Age = "running " + ago(now, r.StartedAt)
			p.Jobs = append(p.Jobs, j)
		}
	}
	fails := runs.Merge(in.Failures)
	for _, r := range fails {
		if !r.Failed() {
			continue
		}
		if now.Sub(r.EndedAt) < 24*time.Hour {
			p.Failed++
		}
		if len(p.Failures) < MaxFailures {
			j := job(r, now)
			j.Age = ago(now, r.EndedAt) + " ago"
			j.At = r.EndedAt.UTC().Format(time.RFC3339)
			p.Failures = append(p.Failures, j)
		}
	}
	if h := in.HistoryFrom; !h.IsZero() && now.Sub(h) < 24*time.Hour {
		p.Kept = ago(now, h)
	}
	for _, e := range in.Errors {
		p.Errors = append(p.Errors, clean(e, 200))
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func job(r runs.Record, now time.Time) jobView {
	j := jobView{Repo: clean(r.Repo, 120), Job: clean(orDefault(r.Job, r.ID), 120), Device: clean(r.Device, 80),
		URL: forgeLink(r.RunURL), Conclusion: clean(r.Conclusion, 20), Step: clean(r.FailedStep, 120)}
	if !r.StartedAt.IsZero() {
		j.At = r.StartedAt.UTC().Format(time.RFC3339)
	}
	return j
}

// forgeLink is u when it is a plain http or https link to a forge page,
// else "": no credentials, no other scheme.
func forgeLink(u string) string {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" || p.User != nil {
		return ""
	}
	if s := p.String(); s == runs.RedactString(s) {
		return s
	}
	return ""
}

func suffix(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}
