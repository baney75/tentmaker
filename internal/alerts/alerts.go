// Package alerts POSTs a small message to webhooks when a job ends: on a
// failure, on a recovery (a pass after a failure) or on a slow job.
//
// Sends never block dispatch. JobEnded decides which alerts fire and puts
// them on a bounded queue; a full queue drops the message and counts it.
// One sender works the queue with a timeout per POST. Each (alert, repo,
// job) has a cooldown so a flapping job sends one message, not fifty.
//
// A webhook URL is a secret (a Slack or Discord URL is its own key). It
// never goes in a log line or an error: both name only the host.
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/runs"
)

// Event kinds; they match config Alert.On.
const (
	OnFailure   = "failure"
	OnRecovered = "recovered"
	OnSlow      = "slow"
	onTest      = "test"
)

// Defaults for Options.
const (
	DefaultQueue    = 64
	DefaultTimeout  = 10 * time.Second
	DefaultCooldown = 15 * time.Minute
)

// Options configure a Notifier.
type Options struct {
	Alerts []config.Alert
	// Resolve returns the URL kept in the secret store under name.
	Resolve  func(name string) (string, error)
	Client   *http.Client // nil = a client with Timeout
	Timeout  time.Duration
	Cooldown time.Duration
	Queue    int
	Log      *slog.Logger
	Now      func() time.Time
}

type target struct {
	idx int
	a   config.Alert
	url string
	err string // why it can't send; "" = ready
}

type message struct {
	t   *target
	ev  string
	rec runs.Record
}

// Notifier sends alerts. Its zero value is not usable; call New.
type Notifier struct {
	o       Options
	targets []*target
	q       chan message

	mu    sync.Mutex
	last  map[string]string    // repo+job -> last conclusion, for recovered
	sent  map[string]time.Time // alert+repo+job -> last send, for cooldown
	drops atomic.Int64
	fails atomic.Int64
	oks   atomic.Int64
	// dropLog limits the "queue full" warning to one a minute.
	dropLog time.Time
}

// Counts is what a notifier did since it started.
type Counts struct {
	Sent    int64 `json:"sent"`
	Failed  int64 `json:"failed"`
	Dropped int64 `json:"dropped"`
}

// New resolves each alert's URL and returns a Notifier. Call Run to send.
func New(o Options) *Notifier {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Cooldown <= 0 {
		o.Cooldown = DefaultCooldown
	}
	if o.Queue <= 0 {
		o.Queue = DefaultQueue
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: o.Timeout}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	n := &Notifier{o: o, q: make(chan message, o.Queue), last: map[string]string{}, sent: map[string]time.Time{}}
	for i, a := range o.Alerts {
		t := &target{idx: i, a: a, url: a.URL}
		if a.URLSecret != "" {
			switch {
			case o.Resolve == nil:
				t.err = "no secret store"
			default:
				u, err := o.Resolve(a.URLSecret)
				if err != nil {
					t.err = fmt.Sprintf("secret %s: not readable", a.URLSecret)
				}
				t.url = strings.TrimSpace(u)
			}
		}
		if t.err == "" {
			if u, err := url.Parse(t.url); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				t.err = "the URL is not an http or https URL"
			}
		}
		n.targets = append(n.targets, t)
	}
	return n
}

// Enabled reports whether any alert is configured.
func (n *Notifier) Enabled() bool { return n != nil && len(n.targets) > 0 }

// Counts returns the send totals.
func (n *Notifier) Counts() Counts {
	return Counts{Sent: n.oks.Load(), Failed: n.fails.Load(), Dropped: n.drops.Load()}
}

// Run sends queued messages until ctx ends.
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-n.q:
			if err := n.send(ctx, m); err != nil {
				n.fails.Add(1)
				n.o.Log.Warn("alert not sent", "alert", m.t.idx, "host", host(m.t.url), "error", err)
			} else {
				n.oks.Add(1)
			}
		}
	}
}

func jobKey(r runs.Record) string {
	return strings.ToLower(r.Forge + "\x00" + r.Repo + "\x00" + r.Workflow + "\x00" + r.Job)
}

// JobEnded queues the alerts rec fires. It never blocks.
func (n *Notifier) JobEnded(rec runs.Record) {
	if !n.Enabled() || !rec.Ended() {
		return
	}
	now := n.o.Now()
	k := jobKey(rec)
	n.mu.Lock()
	prev := n.last[k]
	// Cancelled and unknown ends say nothing about the code; keep the last
	// real outcome so a pass after them still counts as a recovery.
	if rec.Conclusion == runs.Success || rec.Failed() {
		n.last[k] = rec.Conclusion
	}
	var out []message
	for _, t := range n.targets {
		ev := fires(t.a, rec, prev, now)
		if ev == "" {
			continue
		}
		ck := fmt.Sprintf("%d\x00%s", t.idx, k)
		if at, ok := n.sent[ck]; ok && now.Sub(at) < n.o.Cooldown {
			continue
		}
		n.sent[ck] = now
		out = append(out, message{t: t, ev: ev, rec: rec})
	}
	n.mu.Unlock()
	for _, m := range out {
		n.enqueue(m)
	}
}

func (n *Notifier) enqueue(m message) {
	select {
	case n.q <- m:
	default:
		n.drops.Add(1)
		n.mu.Lock()
		now := n.o.Now()
		warn := now.Sub(n.dropLog) >= time.Minute
		if warn {
			n.dropLog = now
		}
		n.mu.Unlock()
		if warn {
			n.o.Log.Warn("alert queue full; dropped an alert", "dropped_total", n.drops.Load())
		}
	}
}

// fires returns the event an alert sends for rec, or "".
func fires(a config.Alert, rec runs.Record, prev string, now time.Time) string {
	if !matchRepo(a.Repos, rec.Repo) {
		return ""
	}
	d := rec.Duration(now)
	if a.MinDuration > 0 && d < time.Duration(a.MinDuration) {
		return ""
	}
	switch a.On {
	case OnFailure:
		if rec.Failed() {
			return OnFailure
		}
	case OnRecovered:
		if rec.Conclusion == runs.Success && (prev == runs.Failure || prev == runs.Error) {
			return OnRecovered
		}
	case OnSlow:
		if a.MinDuration > 0 {
			return OnSlow
		}
	}
	return ""
}

// matchRepo: an empty list matches every repo; entries may use path globs
// ("baney75/*"). Names compare without case.
func matchRepo(pats []string, repo string) bool {
	if len(pats) == 0 {
		return true
	}
	repo = strings.ToLower(repo)
	for _, p := range pats {
		p = strings.ToLower(p)
		if p == repo {
			return true
		}
		if ok, err := path.Match(p, repo); err == nil && ok {
			return true
		}
	}
	return false
}

// Result is one alert's test send.
type Result struct {
	Alert int    `json:"alert"`
	On    string `json:"on"`
	Host  string `json:"host"`
	Kind  string `json:"kind"` // slack, discord, ntfy or generic
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Test sends a test message to every alert now, off the queue and with no
// cooldown, and says how each went.
func (n *Notifier) Test(ctx context.Context, device string) []Result {
	now := n.o.Now()
	rec := runs.Record{ID: "tentmaker-test", Forge: "github", Repo: "tentmaker/alerts-test", Workflow: "ci",
		Job: "test", Device: device, StartedAt: now.Add(-90 * time.Second), EndedAt: now, Conclusion: runs.Failure,
		FailedStep: "this is a test alert"}
	out := make([]Result, 0, len(n.targets))
	for _, t := range n.targets {
		r := Result{Alert: t.idx, On: t.a.On, Host: host(t.url), Kind: Kind(t.url)}
		if err := n.send(ctx, message{t: t, ev: onTest, rec: rec}); err != nil {
			r.Error = err.Error()
		} else {
			r.OK = true
		}
		out = append(out, r)
	}
	return out
}

func (n *Notifier) send(ctx context.Context, m message) error {
	if m.t.err != "" {
		return errors.New(m.t.err)
	}
	ctype, body, hdr := Format(m.t.url, m.ev, m.rec, n.o.Now())
	ctx, cancel := context.WithTimeout(ctx, n.o.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.t.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("bad webhook URL")
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("User-Agent", "tentmaker-alerts")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := n.o.Client.Do(req)
	if err != nil {
		// A *url.Error quotes the URL; keep only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("post to %s: %v", host(m.t.url), err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("post to %s: %s", host(m.t.url), resp.Status)
	}
	return nil
}

func host(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(bad url)"
	}
	return u.Hostname()
}

// Kind is the body format a webhook URL gets: "slack", "discord", "ntfy"
// or "generic", by its host.
func Kind(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "generic"
	}
	h := strings.ToLower(u.Hostname())
	switch {
	case h == "hooks.slack.com":
		return "slack"
	case (h == "discord.com" || h == "discordapp.com" || strings.HasSuffix(h, ".discord.com")) && strings.HasPrefix(u.Path, "/api/webhooks/"):
		return "discord"
	case h == "ntfy.sh" || strings.HasPrefix(h, "ntfy."):
		return "ntfy"
	}
	return "generic"
}

// Generic is the JSON body any other webhook gets.
type Generic struct {
	Event      string    `json:"event"`
	Repo       string    `json:"repo"`
	Workflow   string    `json:"workflow,omitempty"`
	Job        string    `json:"job"`
	Conclusion string    `json:"conclusion"`
	FailedStep string    `json:"failed_step,omitempty"`
	Device     string    `json:"device"`
	RunURL     string    `json:"run_url,omitempty"`
	Ref        string    `json:"ref,omitempty"`
	SHA        string    `json:"sha,omitempty"`
	ID         string    `json:"id"`
	DurationS  float64   `json:"duration_s"`
	QueueS     float64   `json:"queue_wait_s"`
	PeakMemory int64     `json:"peak_memory_bytes,omitempty"`
	EndedAt    time.Time `json:"ended_at"`
	Text       string    `json:"text"`
}

// Title is the one-line summary every format leads with.
func Title(ev string, r runs.Record) string {
	what := map[string]string{OnFailure: "failed", OnRecovered: "passed again", OnSlow: "was slow", onTest: "test alert"}[ev]
	if what == "" {
		what = ev
	}
	name := r.Job
	if r.Workflow != "" {
		name = r.Workflow + " / " + r.Job
	}
	return fmt.Sprintf("%s %s %s", r.Repo, name, what)
}

// Text is the plain message body: the title, then the facts.
func Text(ev string, r runs.Record, now time.Time) string {
	var b strings.Builder
	b.WriteString(Title(ev, r))
	fmt.Fprintf(&b, "\n%s on %s in %s", r.Conclusion, r.Device, r.Duration(now).Round(time.Second))
	if r.FailedStep != "" {
		fmt.Fprintf(&b, ", step %q", r.FailedStep)
	}
	if r.Ref != "" || r.SHA != "" {
		sha := r.SHA
		if len(sha) > 7 {
			sha = sha[:7]
		}
		fmt.Fprintf(&b, "\n%s %s", r.Ref, sha)
	}
	if r.RunURL != "" {
		b.WriteString("\n" + r.RunURL)
	}
	fmt.Fprintf(&b, "\ntentmaker logs %s", r.ID)
	return b.String()
}

// Format builds the request body for a webhook URL: its content type,
// body and extra headers.
func Format(raw, ev string, r runs.Record, now time.Time) (string, []byte, map[string]string) {
	text := runs.RedactString(Text(ev, r, now))
	switch Kind(raw) {
	case "slack":
		b, _ := json.Marshal(map[string]string{"text": text})
		return "application/json", b, nil
	case "discord":
		if len(text) > 1900 {
			text = text[:1900]
		}
		b, _ := json.Marshal(map[string]any{"content": text, "allowed_mentions": map[string]any{"parse": []string{}}})
		return "application/json", b, nil
	case "ntfy":
		h := map[string]string{"Title": runs.RedactString(Title(ev, r)), "Tags": map[string]string{
			OnFailure: "x", OnRecovered: "white_check_mark", OnSlow: "hourglass", onTest: "test_tube"}[ev]}
		if ev == OnFailure {
			h["Priority"] = "high"
		}
		if strings.HasPrefix(r.RunURL, "https://") {
			h["Click"] = r.RunURL
		}
		// The title is in a header; drop it from the body.
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
		return "text/plain; charset=utf-8", []byte(text), h
	}
	red := runs.RedactString
	g := Generic{Event: ev, Repo: r.Repo, Workflow: red(r.Workflow), Job: red(r.Job), Conclusion: r.Conclusion,
		FailedStep: red(r.FailedStep), Device: r.Device, RunURL: r.RunURL, Ref: r.Ref, SHA: r.SHA, ID: r.ID,
		DurationS: r.Duration(now).Seconds(), QueueS: r.QueueWait().Seconds(), PeakMemory: r.PeakMemoryBytes,
		EndedAt: r.EndedAt, Text: text}
	b, _ := json.Marshal(g)
	return "application/json", b, nil
}
