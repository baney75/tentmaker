package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/runs"
)

const (
	slackURL   = "https://hooks.slack.com/services/T000/B000/sekritsekrit"
	discordURL = "https://discord.com/api/webhooks/123/sekritsekrit"
	ntfyURL    = "https://ntfy.sh/tm-sekritsekrit"
	otherURL   = "https://ci.example.com/hook?key=sekritsekrit"
)

type posted struct {
	url, ctype string
	hdr        http.Header
	body       []byte
}

// fakeTransport records each request and answers with status or err;
// block, when set, holds every request until it is closed.
type fakeTransport struct {
	mu     sync.Mutex
	got    []posted
	status int
	err    error
	block  chan struct{}
}

func (f *fakeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.got = append(f.got, posted{url: r.URL.String(), ctype: r.Header.Get("Content-Type"), hdr: r.Header.Clone(), body: b})
	st, err := f.status, f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if st == 0 {
		st = 200
	}
	return &http.Response{StatusCode: st, Status: http.StatusText(st), Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func (f *fakeTransport) posts() []posted {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]posted(nil), f.got...)
}

var t0 = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

func rec(concl string, d time.Duration) runs.Record {
	return runs.Record{ID: "tm-alpha-1", Forge: "github", Repo: "baney75/site", Workflow: "ci", Job: "test",
		Device: "alpha", Ref: "main", SHA: "0123456789abcdef", RunURL: "https://github.com/baney75/site/actions/runs/9",
		QueuedAt: t0.Add(-d - 30*time.Second), StartedAt: t0.Add(-d), EndedAt: t0, Conclusion: concl, FailedStep: "go test"}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newN(ft *fakeTransport, c *clock, as ...config.Alert) *Notifier {
	return New(Options{Alerts: as, Client: &http.Client{Transport: ft}, Now: c.now, Timeout: time.Second,
		Resolve: func(name string) (string, error) {
			if name == "slack-ci" {
				return slackURL + "\n", nil
			}
			return "", errors.New("secret not found")
		}})
}

// drain sends what is queued now.
func drain(n *Notifier) {
	for {
		select {
		case m := <-n.q:
			if err := n.send(context.Background(), m); err != nil {
				n.fails.Add(1)
			} else {
				n.oks.Add(1)
			}
		default:
			return
		}
	}
}

func TestFormats(t *testing.T) {
	r := rec(runs.Failure, 75*time.Second)
	r.FailedStep = "deploy with ghp_abcdefghijklmnopqrstuvwxyz0123456789"

	ct, b, h := Format(slackURL, OnFailure, r, t0)
	var s map[string]string
	if ct != "application/json" || json.Unmarshal(b, &s) != nil || !strings.HasPrefix(s["text"], "baney75/site ci / test failed") || h != nil {
		t.Fatalf("slack %s %s %v", ct, b, h)
	}
	if strings.Contains(string(b), "ghp_") || !strings.Contains(s["text"], "1m15s") || !strings.Contains(s["text"], "tentmaker logs tm-alpha-1") {
		t.Fatalf("slack text %q", s["text"])
	}

	_, b, _ = Format(discordURL, OnRecovered, rec(runs.Success, time.Minute), t0)
	var d map[string]any
	if json.Unmarshal(b, &d) != nil || !strings.Contains(d["content"].(string), "passed again") || d["allowed_mentions"] == nil {
		t.Fatalf("discord %s", b)
	}

	ct, b, h = Format(ntfyURL, OnFailure, r, t0)
	if !strings.HasPrefix(ct, "text/plain") || h["Title"] != "baney75/site ci / test failed" || h["Priority"] != "high" ||
		h["Click"] != r.RunURL || h["Tags"] != "x" || strings.Contains(string(b), "test failed") {
		t.Fatalf("ntfy %s %q %v", ct, b, h)
	}

	_, b, _ = Format(otherURL, OnSlow, r, t0)
	var g Generic
	if err := json.Unmarshal(b, &g); err != nil || g.Event != OnSlow || g.Repo != "baney75/site" || g.DurationS != 75 ||
		g.QueueS != 30 || g.ID != "tm-alpha-1" || strings.Contains(string(b), "ghp_") {
		t.Fatalf("generic %s %v", b, err)
	}

	for raw, want := range map[string]string{slackURL: "slack", discordURL: "discord", ntfyURL: "ntfy",
		"https://ntfy.example.org/x": "ntfy", "https://discord.com/other": "generic", otherURL: "generic", "::": "generic"} {
		if got := Kind(raw); got != want {
			t.Errorf("Kind(%q) = %s, want %s", raw, got, want)
		}
	}
}

func TestFiresAndCooldown(t *testing.T) {
	ft := &fakeTransport{}
	c := &clock{t: t0}
	n := newN(ft, c,
		config.Alert{URLSecret: "slack-ci", On: "failure", Repos: []string{"baney75/*"}},
		config.Alert{URL: ntfyURL, On: "recovered"},
		config.Alert{URL: otherURL, On: "slow", MinDuration: config.Duration(10 * time.Minute)},
		config.Alert{URL: otherURL, On: "failure", Repos: []string{"someone/else"}},
	)
	n.JobEnded(rec(runs.Failure, time.Minute))
	n.JobEnded(rec(runs.Failure, time.Minute)) // inside the cooldown
	drain(n)
	p := ft.posts()
	if len(p) != 1 || p[0].url != slackURL {
		t.Fatalf("posts = %+v", p)
	}

	n.JobEnded(rec(runs.Cancelled, time.Minute)) // not a real outcome
	n.JobEnded(rec(runs.Success, 11*time.Minute))
	drain(n)
	p = ft.posts()
	if len(p) != 3 || p[1].url != ntfyURL || p[2].url != otherURL {
		t.Fatalf("posts = %+v", p)
	}
	n.JobEnded(rec(runs.Success, time.Minute)) // no recovery twice
	drain(n)
	if len(ft.posts()) != 3 {
		t.Fatalf("recovered twice: %+v", ft.posts())
	}

	c.add(DefaultCooldown)
	n.JobEnded(rec(runs.Error, time.Minute))
	drain(n)
	if p := ft.posts(); len(p) != 4 || p[3].url != slackURL {
		t.Fatalf("after the cooldown: %+v", p)
	}
	n.JobEnded(rec(runs.Running, time.Minute))
	if len(n.q) != 0 {
		t.Fatalf("a running job fired")
	}
	if c := n.Counts(); c.Sent != 4 || c.Dropped != 0 {
		t.Fatalf("counts %+v", c)
	}
}

// A full queue drops and counts; JobEnded returns at once while the sender
// is stuck.
func TestQueueFullDrops(t *testing.T) {
	ft := &fakeTransport{block: make(chan struct{})}
	c := &clock{t: t0}
	n := New(Options{Alerts: []config.Alert{{URL: otherURL, On: "failure"}}, Client: &http.Client{Transport: ft},
		Now: c.now, Queue: 2, Cooldown: time.Nanosecond, Timeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.Run(ctx); close(done) }()

	start := time.Now()
	for i := 0; i < 10; i++ {
		c.add(time.Second)
		n.JobEnded(rec(runs.Failure, time.Minute))
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("JobEnded blocked for %s", el)
	}
	// One is in flight, two are queued; the rest dropped.
	if d := n.Counts().Dropped; d < 7 || d > 8 {
		t.Fatalf("dropped = %d", d)
	}
	close(ft.block)
	cancel()
	<-done
}

// Errors and test results name the host, never the URL or its key.
func TestErrorsHideURL(t *testing.T) {
	ft := &fakeTransport{err: errors.New("connection refused")}
	c := &clock{t: t0}
	n := newN(ft, c, config.Alert{URL: otherURL, On: "failure"}, config.Alert{URLSecret: "missing", On: "failure"},
		config.Alert{URLSecret: "slack-ci", On: "failure"})
	res := n.Test(context.Background(), "alpha")
	if len(res) != 3 || res[0].OK || res[0].Host != "ci.example.com" || !strings.Contains(res[0].Error, "connection refused") {
		t.Fatalf("results %+v", res)
	}
	if res[1].OK || !strings.Contains(res[1].Error, "not readable") || res[2].Kind != "slack" {
		t.Fatalf("results %+v", res)
	}
	ft.mu.Lock()
	ft.err, ft.status = nil, 404
	ft.mu.Unlock()
	res = append(res, n.Test(context.Background(), "alpha")...)
	if res[3].OK || !strings.Contains(res[3].Error, "Not Found") {
		t.Fatalf("results %+v", res)
	}
	for _, r := range res {
		if strings.Contains(r.Error, "sekrit") || strings.Contains(r.Host, "sekrit") {
			t.Fatalf("a result shows the URL: %+v", r)
		}
	}
	ft.mu.Lock()
	ft.status = 200
	ft.mu.Unlock()
	if r := n.Test(context.Background(), "alpha"); !r[0].OK || !r[2].OK {
		t.Fatalf("results %+v", r)
	}
	p := ft.posts()
	if !strings.Contains(string(p[len(p)-1].body), "test alert") {
		t.Fatalf("test body %s", p[len(p)-1].body)
	}
}

// A post that hangs is cut by the timeout.
func TestSendTimeout(t *testing.T) {
	ft := &fakeTransport{block: make(chan struct{})}
	defer close(ft.block)
	n := New(Options{Alerts: []config.Alert{{URL: otherURL, On: "failure"}}, Client: &http.Client{Transport: ft},
		Timeout: 20 * time.Millisecond})
	r := n.Test(context.Background(), "alpha")
	if r[0].OK || !strings.Contains(r[0].Error, "deadline") || strings.Contains(r[0].Error, "sekrit") {
		t.Fatalf("result %+v", r)
	}
}
