package cli

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/alerts"
	"github.com/baney75/tentmaker/internal/control"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/runs"
)

// fakeHistory is the run history the fake daemon serves.
type fakeHistory struct {
	mu     sync.Mutex
	recs   []runs.Record
	logs   map[string][]string // id -> successive answers; the last repeats
	polls  map[string]int
	reruns []control.RerunRequest
}

var hist = map[*fakeDaemon]*fakeHistory{}
var histMu sync.Mutex

func (d *fakeDaemon) hist() *fakeHistory {
	histMu.Lock()
	defer histMu.Unlock()
	h := hist[d]
	if h == nil {
		h = &fakeHistory{logs: map[string][]string{}, polls: map[string]int{}}
		hist[d] = h
	}
	return h
}

func (d *fakeDaemon) Runs(ctx context.Context, q runs.Query) (runs.Answer, error) {
	h := d.hist()
	h.mu.Lock()
	defer h.mu.Unlock()
	return runs.Answer{Records: runs.Apply(h.recs, q)}, nil
}

func (d *fakeDaemon) RunLog(ctx context.Context, id string, max int64) (runs.LogAnswer, error) {
	h := d.hist()
	h.mu.Lock()
	defer h.mu.Unlock()
	answers, ok := h.logs[id]
	if !ok {
		return runs.LogAnswer{}, core.ErrNotFound
	}
	i := min(h.polls[id], len(answers)-1)
	h.polls[id]++
	return runs.LogAnswer{ID: id, Device: "box", Running: i < len(answers)-1, Data: []byte(answers[i]), Size: int64(len(answers[i]))}, nil
}

func (d *fakeDaemon) Rerun(ctx context.Context, req control.RerunRequest) (control.RerunAnswer, error) {
	h := d.hist()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reruns = append(h.reruns, req)
	return control.RerunAnswer{Repo: req.Repo, RunID: req.RunID, Broker: "box"}, nil
}

func (d *fakeDaemon) AlertsTest(ctx context.Context) (any, error) {
	return []alerts.Result{{Alert: 0, On: "failure", Host: "hooks.slack.com", Kind: "slack", OK: true},
		{Alert: 1, On: "slow", Host: "ntfy.sh", Kind: "ntfy", Error: "post to ntfy.sh: 500 Internal Server Error"}}, nil
}

func histFixture(t *testing.T) (*fixture, *fakeHistory) {
	f := newFixture(t)
	f.setUp()
	f.serveControl()
	h := f.daemon.hist()
	now := time.Now()
	h.recs = []runs.Record{
		{ID: "tm-box-1", Repo: "baney75/site", Workflow: "ci", Job: "test", Device: "box", RunID: 41, Platform: "linux/x64",
			QueuedAt: now.Add(-10 * time.Minute), StartedAt: now.Add(-9*time.Minute - 30*time.Second), EndedAt: now.Add(-5 * time.Minute),
			Conclusion: runs.Failure, FailedStep: "go test", PeakMemoryBytes: 3 << 30},
		{ID: "tm-box-2", Repo: "baney75/site", Workflow: "ci", Job: "lint", Device: "box", RunID: 41, Platform: "linux/x64",
			StartedAt: now.Add(-9 * time.Minute), EndedAt: now.Add(-8 * time.Minute), Conclusion: runs.Success, PeakMemoryBytes: 512 << 20},
		{ID: "tm-mac-1", Kind: core.KindCheck, Repo: "baney75/app", Job: "build", Device: "mac", Platform: "macos/arm64",
			StartedAt: now.Add(-2 * time.Minute), Conclusion: runs.Running},
	}
	return f, h
}

func TestRunsCommand(t *testing.T) {
	f, _ := histFixture(t)
	if code := f.run("", "runs"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, f.stderr)
	}
	out := f.stdout.String()
	for _, want := range []string{"CONCLUSION", "failure (go test)", "ci / test", "30s", "4m30s", "3.0GiB", "512MiB", "running", "tm-mac-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("runs output misses %q:\n%s", want, out)
		}
	}
	if code := f.run("", "runs", "--failed", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	var v runsView
	if err := json.Unmarshal(f.stdout.Bytes(), &v); err != nil || len(v.Records) != 1 || v.Records[0].ID != "tm-box-1" || v.Service != "running" {
		t.Fatalf("runs --failed --json = %+v %v", v, err)
	}
	f.run("", "runs", "--repo", "baney75/app", "-n", "5", "--json")
	if err := json.Unmarshal(f.stdout.Bytes(), &v); err != nil || len(v.Records) != 1 || v.Records[0].ID != "tm-mac-1" {
		t.Fatalf("runs --repo = %+v %v", v, err)
	}
}

// Without the service, runs reads this device's own history file.
func TestRunsWithoutService(t *testing.T) {
	f := newFixture(t)
	f.setUp()
	st := runs.Open(f.env.Paths.State+"/runs", 0)
	if err := st.Add(runs.Record{ID: "tm-box-9", Repo: "o/r", Job: "test", Device: "box", StartedAt: time.Now().Add(-time.Minute),
		EndedAt: time.Now(), Conclusion: runs.Success}, []byte("all good\n")); err != nil {
		t.Fatal(err)
	}
	if code := f.run("", "runs", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	m := f.jsonObject(nil)
	if m["service"] != "not running" || len(m["records"].([]any)) != 1 {
		t.Fatalf("runs = %v", m)
	}
	if code := f.run("", "logs", "tm-box-9"); code != ExitOK || f.stdout.String() != "all good\n" {
		t.Fatalf("logs exit %d: %q %s", code, f.stdout, f.stderr)
	}
}

func TestJobLogsAndFollow(t *testing.T) {
	f, h := histFixture(t)
	h.logs["tm-box-1"] = []string{"one\ntwo\nthree\n"}
	if code := f.run("", "logs", "tm-box-1"); code != ExitOK || f.stdout.String() != "one\ntwo\nthree\n" {
		t.Fatalf("logs = %d %q %s", code, f.stdout, f.stderr)
	}
	if code := f.run("", "logs", "tm-box-1", "-n", "1"); code != ExitOK || f.stdout.String() != "three\n" {
		t.Fatalf("logs -n 1 = %q", f.stdout)
	}
	if code := f.run("", "logs", "tm-box-1", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	if m := f.jsonObject(nil); len(m["lines"].([]any)) != 3 || m["running"] != false {
		t.Fatalf("logs --json = %v", m)
	}
	if code := f.run("", "logs", "tm-nobody-1"); code != ExitFailed || !strings.Contains(f.stderr.String(), "no device has job") {
		t.Fatalf("missing job = %d %s", code, f.stderr)
	}

	// --follow prints only what is new at each poll, then stops when the
	// job ends.
	h.logs["tm-mac-1"] = []string{"a\n", "a\nb\n", "a\nb\nc\n", "a\nb\nc\nd\n"}
	if code := f.run("", "logs", "tm-mac-1", "--follow"); code != ExitOK {
		t.Fatalf("follow exit %d: %s", code, f.stderr)
	}
	if got := f.stdout.String(); got != "a\nb\nc\nd\n(job tm-mac-1 has ended)\n" {
		t.Fatalf("follow printed %q", got)
	}
}

func TestNewBytes(t *testing.T) {
	cases := []struct{ prev, next, want string }{
		{"", "abc", "abc"},
		{"abc\n", "abc\ndef\n", "def\n"},
		{"abc\n", "abc\n", ""},
		{"xyz\n", "abc\n", "[... more output than one poll could show ...]\nabc\n"},
		{"bc\n", "bc\nde\n", "de\n"},
	}
	for _, c := range cases {
		if got := string(newBytes([]byte(c.prev), []byte(c.next))); got != c.want {
			t.Errorf("newBytes(%q, %q) = %q, want %q", c.prev, c.next, got, c.want)
		}
	}
}

func TestRerunCommand(t *testing.T) {
	f, h := histFixture(t)
	if code := f.run("", "rerun", "tm-box-1"); code != ExitOK || !strings.Contains(f.stdout.String(), "run 41 in baney75/site") {
		t.Fatalf("rerun by id = %d %q %s", code, f.stdout, f.stderr)
	}
	if code := f.run("", "rerun", "https://github.com/baney75/other/actions/runs/77/job/1", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	if m := f.jsonObject(nil); m["broker"] != "box" || m["run_id"] != float64(77) {
		t.Fatalf("rerun --json = %v", m)
	}
	if code := f.run("", "rerun", "tm-mac-1"); code != ExitFailed || !strings.Contains(f.stderr.String(), "checks-mode") {
		t.Fatalf("rerun of a check job = %d %s", code, f.stderr)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.reruns) != 2 || h.reruns[0] != (control.RerunRequest{Repo: "baney75/site", RunID: 41}) || h.reruns[1].Repo != "baney75/other" {
		t.Fatalf("reruns = %+v", h.reruns)
	}
}

func TestStatsCommand(t *testing.T) {
	f, _ := histFixture(t)
	if code := f.run("", "stats", "--since", "24h"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	out := f.stdout.String()
	for _, want := range []string{"baney75/site", "50%", "EST. SAVED", "estimate"} {
		if !strings.Contains(out, want) {
			t.Errorf("stats misses %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "baney75/app") {
		t.Errorf("a running job counted:\n%s", out)
	}
	f.run("", "stats", "--json")
	m := f.jsonObject(nil)
	if m["total"].(map[string]any)["jobs"] != float64(2) || !strings.Contains(m["estimate"].(string), "estimate") {
		t.Fatalf("stats --json = %v", m)
	}
}

func TestAlertsTestCommand(t *testing.T) {
	f, _ := histFixture(t)
	code := f.run("", "alerts", "test")
	if code != ExitFailed || !strings.Contains(f.stdout.String(), "hooks.slack.com: sent") ||
		!strings.Contains(f.stdout.String(), "ntfy.sh: failed") || !strings.Contains(f.stderr.String(), "1 alert(s)") {
		t.Fatalf("alerts test = %d %q %q", code, f.stdout, f.stderr)
	}
	if code := f.run("", "alerts", "test", "--json"); code != ExitOK {
		t.Fatal(f.stderr.String())
	}
	if m := f.jsonObject(nil); len(m["results"].([]any)) != 2 {
		t.Fatalf("alerts --json = %v", m)
	}
}

// wait_for_run waits for the run to settle, then names the failed job, its
// step and the end of its log.
func TestMCPWaitForRun(t *testing.T) {
	f, h := histFixture(t)
	h.mu.Lock()
	for i := range h.recs {
		h.recs[i].SHA = "abc1234def"
	}
	h.recs[0].Conclusion = runs.Running // tm-box-1 is still going
	h.recs[0].EndedAt = time.Time{}
	h.recs = h.recs[:2]
	h.mu.Unlock()
	var lines []string
	for i := range 100 {
		lines = append(lines, "line "+strconv.Itoa(i))
	}
	h.logs["tm-box-1"] = []string{strings.Join(lines, "\n") + "\n"}

	var mu sync.Mutex
	clock := time.Now()
	sleeps := 0
	f.env.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	f.env.Sleep = func(d time.Duration) {
		mu.Lock()
		clock = clock.Add(d)
		sleeps++
		n := sleeps
		mu.Unlock()
		if n == 2 {
			h.mu.Lock()
			h.recs[0].Conclusion, h.recs[0].EndedAt = runs.Failure, clock
			h.mu.Unlock()
		}
	}
	a := &app{env: f.env, ctx: context.Background(), json: true}
	got, err := (controller{a}).WaitForRun(context.Background(), "baney75/site", "abc1234", "", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	v := got.(waitView)
	if !v.Done || v.TimedOut || v.Conclusion != runs.Failure || len(v.Jobs) != 2 {
		t.Fatalf("wait = %+v", v)
	}
	j := v.Jobs[0]
	if j.ID != "tm-box-1" {
		j = v.Jobs[1]
	}
	if j.FailedStep != "go test" || len(j.LogTail) != 80 || j.LogTail[79] != "line 99" || j.LogTail[0] != "line 20" {
		t.Fatalf("failed job = %+v", j)
	}
	if !strings.Contains(v.Note, "tm-box-1") {
		t.Fatalf("note = %q", v.Note)
	}

	// A run that never settles times out and says so.
	h.mu.Lock()
	h.recs[0].Conclusion, h.recs[0].EndedAt = runs.Running, time.Time{}
	h.mu.Unlock()
	sleeps = 100
	got, err = (controller{a}).WaitForRun(context.Background(), "", "", "https://github.com/baney75/site/actions/runs/41", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if v := got.(waitView); v.Done || !v.TimedOut || v.Conclusion != runs.Running || !strings.Contains(v.Note, "timeout") {
		t.Fatalf("timed out wait = %+v", v)
	}

	// Nothing for the commit: a note says so.
	got, _ = (controller{a}).WaitForRun(context.Background(), "baney75/site", "fff0000", "", 30*time.Second)
	if v := got.(waitView); !v.TimedOut || len(v.Jobs) != 0 || !strings.Contains(v.Note, "no Tentmaker job") {
		t.Fatalf("empty wait = %+v", v)
	}
	if _, err := (controller{a}).WaitForRun(context.Background(), "", "", "https://example.com/nope", time.Minute); err == nil {
		t.Fatal("a bad run URL was accepted")
	}
}

func TestMCPLogsAndRerun(t *testing.T) {
	f, h := histFixture(t)
	h.logs["tm-box-1"] = []string{"a\nb\nc\n"}
	a := &app{env: f.env, ctx: context.Background(), json: true}
	got, err := (controller{a}).Logs(context.Background(), "tm-box-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if v := got.(jobLogView); strings.Join(v.Lines, ",") != "b,c" {
		t.Fatalf("logs = %+v", v)
	}
	f.isRoot = false
	if _, err := (controller{a}).Rerun(context.Background(), "tm-box-1"); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("rerun as a user = %v", err)
	}
	f.isRoot = true
	if _, err := (controller{a}).Rerun(context.Background(), "tm-box-1"); err != nil {
		t.Fatal(err)
	}
	if len(h.reruns) != 1 || h.reruns[0].RunID != 41 {
		t.Fatalf("reruns = %+v", h.reruns)
	}
}
