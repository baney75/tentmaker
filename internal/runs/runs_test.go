package runs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func rec(id, repo, concl string, startMin int, durS int) Record {
	r := Record{ID: id, Repo: repo, Job: "test", Device: "box", Conclusion: concl, Platform: "linux/x64",
		QueuedAt: t0.Add(time.Duration(startMin)*time.Minute - 10*time.Second), StartedAt: t0.Add(time.Duration(startMin) * time.Minute)}
	if concl != Running {
		r.EndedAt = r.StartedAt.Add(time.Duration(durS) * time.Second)
	}
	return r
}

func TestStoreBoundsAndLogs(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir, 3)
	for i := range 5 {
		if err := s.Add(rec(fmt.Sprintf("tm-box-%03d", i), "o/r", Success, i, 60), []byte(fmt.Sprintf("log %d\n", i))); err != nil {
			t.Fatal(err)
		}
	}
	if s.Len() != 3 {
		t.Fatalf("len %d, want 3", s.Len())
	}
	got := s.List(Query{})
	if got[0].ID != "tm-box-004" || got[2].ID != "tm-box-002" {
		t.Fatalf("order: %v %v", got[0].ID, got[2].ID)
	}
	// The dropped records' logs are gone, the kept ones stay.
	if _, err := os.Stat(s.LogPath("tm-box-000")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("log of a dropped record kept: %v", err)
	}
	if b, err := s.Log("tm-box-004"); err != nil || string(b) != "log 4\n" {
		t.Fatalf("log: %q %v", b, err)
	}
	if st, err := os.Stat(s.LogPath("tm-box-004")); err != nil || (st.Mode().Perm() != 0o600 && os.Getenv("GOOS") != "windows") {
		t.Fatalf("log mode: %v %v", st, err)
	}
	// A fresh store reads the same history back.
	s2 := Open(dir, 3)
	if s2.Len() != 3 || s2.List(Query{})[0].ID != "tm-box-004" {
		t.Fatalf("reload: %d", s2.Len())
	}
	// A reload with a smaller max keeps the newest.
	if s3 := Open(dir, 2); s3.Len() != 2 || s3.List(Query{})[1].ID != "tm-box-003" {
		t.Fatalf("smaller max: %+v", s3.List(Query{}))
	}
	// Writes are atomic: no temp files are left behind.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

func TestStoreLogTailIsBoundedAndRedacted(t *testing.T) {
	s := Open(t.TempDir(), 10)
	var big bytes.Buffer
	for big.Len() < LogTailBytes*2 {
		big.WriteString("some build output line that goes on for a while\n")
	}
	big.WriteString("token ghs_" + strings.Repeat("a", 36) + " end\n")
	if err := s.Add(rec("tm-box-big", "o/r", Failure, 0, 5), big.Bytes()); err != nil {
		t.Fatal(err)
	}
	b, err := s.Log("tm-box-big")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > LogTailBytes || len(b) < LogTailBytes-200 {
		t.Fatalf("tail is %d bytes", len(b))
	}
	if bytes.Contains(b, []byte("ghs_")) || !bytes.Contains(b, []byte("token *** end")) {
		t.Fatalf("not redacted: %q", b[len(b)-40:])
	}
	if !bytes.HasPrefix(b, []byte("some build")) {
		t.Fatalf("tail does not start on a whole line: %q", b[:20])
	}
	if r, _ := s.Get("tm-box-big"); !r.HasLog {
		t.Fatal("HasLog not set")
	}
}

func TestStoreRejectsBadIDAndReplaces(t *testing.T) {
	s := Open(t.TempDir(), 10)
	if err := s.Add(Record{ID: "../etc/passwd"}, []byte("x")); err == nil {
		t.Fatal("a path id was accepted")
	}
	if _, err := s.Log("../x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id log: %v", err)
	}
	r := rec("tm-box-1", "o/r", Failure, 0, 5)
	s.Add(r, nil)
	r.Conclusion = Success
	s.Add(r, nil)
	if s.Len() != 1 || s.List(Query{})[0].Conclusion != Success {
		t.Fatalf("replace: %+v", s.List(Query{}))
	}
}

func TestStoreCorruptFileMovedAside(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, historyFile), []byte("{not json"), 0o644)
	s := Open(dir, 10)
	if s.Len() != 0 {
		t.Fatal("corrupt file read as records")
	}
	if err := s.Add(rec("tm-box-1", "o/r", Success, 0, 5), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, historyFile+".bad")); err != nil {
		t.Fatalf("corrupt file not kept aside: %v", err)
	}
}

func TestStoreConcurrentAdds(t *testing.T) {
	s := Open(t.TempDir(), 50)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Add(rec(fmt.Sprintf("tm-box-%03d", i), "o/r", Success, i, 1), []byte("x\n")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := Open(s.Dir(), 50).Len(); n != 40 {
		t.Fatalf("reload has %d records, want 40", n)
	}
}

func TestRedact(t *testing.T) {
	jit := strings.Repeat("eyJydW5uZXIiOnsiYWdlbnRJZCI6MX19", 8)
	// Fake credentials are put together here so secret scanners don't flag
	// the test file.
	pw := "not" + "-a-" + "password"
	jwt := strings.Join([]string{"ey" + "JhbGciOiJub25lIn0", "ey" + "JzdWIiOiJ0ZXN0In0", "ZmFrZS1zaWduYXR1cmU"}, ".")
	cases := map[string]string{
		"push with ghp_" + strings.Repeat("A1", 18) + " ok":          "push with *** ok",
		"GITHUB_TOKEN=ghs_" + strings.Repeat("z", 36):                "GITHUB_TOKEN=***",
		"pat github_pat_11ABCDEFG0" + strings.Repeat("x", 60):        "pat ***",
		"gitlab glpat-" + strings.Repeat("q", 20):                    "gitlab ***",
		"ACTIONS_RUNNER_INPUT_JITCONFIG=abc.def":                     "ACTIONS_RUNNER_INPUT_JITCONFIG=***",
		"config " + jit + " done":                                    "config *** done",
		"Authorization: Bearer abc123":                               "Authorization: Bearer ***",
		"authorization: token abc123":                                "authorization: token ***",
		"git clone https://x-access-token:" + pw + "@github.com/o/r": "git clone https://x-access-token:***@github.com/o/r",
		"curl https://user:" + pw + "@example.com/x":                 "curl https://user:***@example.com/x",
		"rt " + jwt:                 "rt ***",
		"key AKIAABCDEFGHIJKLMNOP!": "key ***!",
		"post https://hooks.slack.com/services/T000/B000/XXXX":     "post https://hooks.slack.com/services/***",
		"plain sha 0123456789abcdef0123456789abcdef01234567 stays": "plain sha 0123456789abcdef0123456789abcdef01234567 stays",
		"Processing step: DisplayName='Run go test'":               "Processing step: DisplayName='Run go test'",
	}
	for in, want := range cases {
		if got := RedactString(in); got != want {
			t.Errorf("Redact(%q)\n got %q\nwant %q", in, got, want)
		}
	}
	if got := string(Redact([]byte("the secret is hunter22"), []byte("hunter22"))); got != "the secret is ***" {
		t.Errorf("extra: %q", got)
	}
}

func TestParseRunnerLog(t *testing.T) {
	log := `[2026-11-01 12:00:01Z INFO StepsRunner] Processing step: DisplayName='Set up job'
[2026-11-01 12:00:02Z INFO StepsRunner] Step result: Succeeded
[2026-11-01 12:00:03Z INFO StepsRunner] Processing step: DisplayName='Run go test ./...'
[2026-11-01 12:00:09Z INFO StepsRunner] Step result: Failed
[2026-11-01 12:00:10Z INFO StepsRunner] Processing step: DisplayName='Post Run actions/checkout@v4'
[2026-11-01 12:00:11Z INFO StepsRunner] Step result: Succeeded
[2026-11-01 12:00:12Z INFO JobRunner] Job result after all job steps finish: Failed
2026-11-01 12:00:13Z: Job test completed with result: Failed`
	c, step := ParseRunnerLog(log)
	if c != Failure || step != "Run go test ./..." {
		t.Fatalf("got %q %q", c, step)
	}
	if c, step := ParseRunnerLog("Job build completed with result: Succeeded"); c != Success || step != "" {
		t.Fatalf("success: %q %q", c, step)
	}
	if c, _ := ParseRunnerLog("Job x completed with result: Canceled"); c != Cancelled {
		t.Fatalf("cancel: %q", c)
	}
	if c, _ := ParseRunnerLog("nothing here"); c != "" {
		t.Fatalf("empty: %q", c)
	}
	if s := CheckStep("failed at step go vet, 12s"); s != "go vet" {
		t.Fatalf("check step %q", s)
	}
	if s := CheckStep("timed out at step build, 3600s"); s != "build" {
		t.Fatalf("check step %q", s)
	}
}

func TestMergePeers(t *testing.T) {
	a := []Record{rec("tm-a-1", "o/r", Success, 1, 60), rec("tm-a-2", "o/r", Running, 5, 0)}
	a[0].Device, a[1].Device = "a", "a"
	b := []Record{rec("tm-b-1", "o/r", Failure, 3, 60)}
	b[0].Device = "b"
	// The same running job, seen again after it ended, keeps the ended copy.
	again := a[1]
	again.Conclusion, again.EndedAt = Success, again.StartedAt.Add(time.Minute)
	got := Merge(a, b, []Record{again, a[0]})
	if len(got) != 3 {
		t.Fatalf("merged %d, want 3: %+v", len(got), got)
	}
	if got[0].ID != "tm-a-2" || got[0].Conclusion != Success || got[1].ID != "tm-b-1" || got[2].ID != "tm-a-1" {
		t.Fatalf("order: %s %s %s", got[0].ID, got[1].ID, got[2].ID)
	}
	// Running jobs sort first.
	got = Merge(a, b)
	if got[0].Conclusion != Running {
		t.Fatalf("running not first: %+v", got[0])
	}
	// The same id on two devices is two jobs.
	c := a[0]
	c.Device = "c"
	if n := len(Merge(a[:1], []Record{c})); n != 2 {
		t.Fatalf("two devices merged into %d", n)
	}
}

func TestQuery(t *testing.T) {
	r := rec("tm-box-1", "Owner/Repo", Failure, 0, 5)
	r.SHA = "0123456789abcdef0123456789abcdef01234567"
	for _, q := range []Query{{Repo: "owner/repo"}, {SHA: "0123456"}, {SHA: r.SHA}, {Failed: true}, {ID: "tm-box-1"}} {
		if !q.Match(r) {
			t.Errorf("%+v should match", q)
		}
	}
	for _, q := range []Query{{Repo: "o/x"}, {SHA: "012345"}, {SHA: "1123456"}, {ID: "tm-box-2"}, {Since: t0.Add(time.Hour)}} {
		if q.Match(r) {
			t.Errorf("%+v should not match", q)
		}
	}
	ok := rec("tm-box-2", "o/r", Success, 0, 5)
	if (Query{Failed: true}).Match(ok) {
		t.Error("success matched failed")
	}
}

func TestStatsMath(t *testing.T) {
	var recs []Record
	// o/a: 10 jobs, 2 failed, durations 10..100 s, queue waits 1..10 s.
	for i := 1; i <= 10; i++ {
		r := rec(fmt.Sprintf("tm-a-%d", i), "o/a", Success, i, i*10)
		r.QueuedAt = r.StartedAt.Add(-time.Duration(i) * time.Second)
		if i <= 2 {
			r.Conclusion = Failure
		}
		recs = append(recs, r)
	}
	// o/b: one macOS job of 61 s (bills 2 minutes).
	m := rec("tm-b-1", "o/b", Error, 20, 61)
	m.Platform = "macos/arm64"
	recs = append(recs, m)
	// Out of the window, and still running: both left out.
	recs = append(recs, rec("tm-old", "o/a", Failure, -600, 10), rec("tm-run", "o/a", Running, 30, 0))

	rep := Stats(recs, t0, t0.Add(2*time.Hour))
	if len(rep.Repos) != 2 || rep.Repos[0].Repo != "o/a" {
		t.Fatalf("repos %+v", rep.Repos)
	}
	a := rep.Repos[0]
	if a.Jobs != 10 || a.Failed != 2 || a.FailureRate != 0.2 {
		t.Fatalf("a counts %+v", a)
	}
	if a.DurationP50 != 50 || a.DurationP95 != 100 || a.QueueP50S != 5 || a.QueueP95S != 10 {
		t.Fatalf("a percentiles %+v", a)
	}
	// 10+20+...+100 s = 550 s = 9.2 min; billed 1+1+1+1+1+1+2+2+2+2 = 14.
	if a.Minutes != 9.2 || a.BilledMinutes != 14 || a.EstimatedSavingUSD != 0.08 {
		t.Fatalf("a minutes %+v", a)
	}
	b := rep.Repos[1]
	if b.Jobs != 1 || b.FailureRate != 1 || b.BilledMinutes != 2 || b.EstimatedSavingUSD != 0.12 {
		t.Fatalf("b %+v", b)
	}
	if rep.Total.Jobs != 11 || rep.Total.Failed != 3 || rep.Total.BilledMinutes != 16 || rep.Total.EstimatedSavingUSD != 0.21 {
		t.Fatalf("total %+v", rep.Total)
	}
	if !strings.Contains(rep.Estimate, "estimate") {
		t.Fatal("not labeled an estimate")
	}
}

func TestPercentile(t *testing.T) {
	if Percentile(nil, 50) != 0 || Percentile([]float64{7}, 95) != 7 {
		t.Fatal("edge cases")
	}
	xs := []float64{5, 1, 4, 2, 3}
	if Percentile(xs, 50) != 3 || Percentile(xs, 95) != 5 || Percentile(xs, 0) != 1 {
		t.Fatal("nearest rank")
	}
}

func TestParseSince(t *testing.T) {
	for s, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "24h": 24 * time.Hour, "90m": 90 * time.Minute, "1.5d": 36 * time.Hour} {
		if d, ok := ParseSince(s); !ok || d != want {
			t.Errorf("%s: %v %v", s, d, ok)
		}
	}
	for _, s := range []string{"", "x", "-1d", "0h", "2000000h", "100000d", "3651d"} {
		if _, ok := ParseSince(s); ok {
			t.Errorf("%q accepted", s)
		}
	}
}

// fakeClock drives Wait without sleeping.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return ctx.Err()
}

func TestWaitSuccess(t *testing.T) {
	clk := &fakeClock{now: t0}
	looks := 0
	look := func(ctx context.Context) (Snapshot, error) {
		looks++
		switch {
		case looks == 1:
			return Snapshot{}, nil // nothing yet
		case looks == 2:
			return Snapshot{}, errors.New("peer down") // retried
		case looks <= 4:
			return Snapshot{Records: []Record{rec("tm-1", "o/r", Running, 0, 0)}, Queued: []string{"lint"}}, nil
		case looks == 5:
			// The first job ended; a dependent one is still queued.
			return Snapshot{Records: []Record{rec("tm-1", "o/r", Success, 0, 5)}, Queued: []string{"deploy"}}, nil
		}
		r2 := rec("tm-2", "o/r", Failure, 1, 5)
		r2.FailedStep = "go test"
		return Snapshot{Records: []Record{rec("tm-1", "o/r", Success, 0, 5), r2}}, nil
	}
	res, err := Wait(context.Background(), look, WaitOptions{Timeout: 10 * time.Minute, Poll: 5 * time.Second, Settle: 10 * time.Second, Now: clk.Now, Sleep: clk.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Done || res.TimedOut || res.Conclusion != Failure || len(res.Records) != 2 {
		t.Fatalf("result %+v", res)
	}
	// Ended at look 6; it had to stay quiet for 10 s (two more polls).
	if looks != 8 {
		t.Fatalf("looks %d, want 8", looks)
	}
}

func TestWaitTimeout(t *testing.T) {
	clk := &fakeClock{now: t0}
	look := func(ctx context.Context) (Snapshot, error) {
		return Snapshot{Records: []Record{rec("tm-1", "o/r", Running, 0, 0)}}, nil
	}
	res, err := Wait(context.Background(), look, WaitOptions{Timeout: time.Minute, Poll: 7 * time.Second, Now: clk.Now, Sleep: clk.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	if res.Done || !res.TimedOut || res.Conclusion != Running || res.Waited != 60 {
		t.Fatalf("result %+v", res)
	}
	// No look ever works: the error comes back.
	_, err = Wait(context.Background(), func(ctx context.Context) (Snapshot, error) { return Snapshot{}, errors.New("no daemon") },
		WaitOptions{Timeout: 10 * time.Second, Now: clk.Now, Sleep: clk.Sleep})
	if err == nil || err.Error() != "no daemon" {
		t.Fatalf("err %v", err)
	}
	// A cancelled context ends the wait.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Wait(ctx, look, WaitOptions{Timeout: time.Hour, Now: clk.Now, Sleep: clk.Sleep}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestParseRunURL(t *testing.T) {
	for in, want := range map[string]struct {
		repo string
		id   int64
		ok   bool
	}{
		"https://github.com/baney75/site/actions/runs/123":           {"baney75/site", 123, true},
		"https://github.com/baney75/site/actions/runs/123/job/456":   {"baney75/site", 123, true},
		"https://ghe.example.com/o/r/actions/runs/9/attempts/2?pr=1": {"o/r", 9, true},
		" https://github.com/baney75/site/actions/runs/123/ ":        {"baney75/site", 123, true},
		"https://github.com/baney75/site/pull/7":                     {"", 0, false},
		"https://github.com/actions/runs/1":                          {"", 0, false},
		"https://github.com/a/b/c/actions/runs/1":                    {"", 0, false},
		"https://github.com/baney75/site/actions/runs/x":             {"", 0, false},
		"tm-alpha-1": {"", 0, false},
		"ftp://github.com/baney75/site/actions/runs/1": {"", 0, false},
	} {
		repo, id, ok := ParseRunURL(in)
		if repo != want.repo || id != want.id || ok != want.ok {
			t.Errorf("ParseRunURL(%q) = %q %d %v", in, repo, id, ok)
		}
	}
}

func TestQueryValuesRoundTrip(t *testing.T) {
	q := Query{ID: "tm-a-1", Repo: "o/r", SHA: "abcdef1", RunID: 9, Failed: true, Since: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Limit: 20}
	got, err := ParseQuery(q.Values())
	if err != nil || got != q {
		t.Fatalf("round trip = %+v %v", got, err)
	}
	if got, _ := ParseQuery(nil); got.Limit != MaxLimit {
		t.Fatalf("default limit = %d", got.Limit)
	}
	for _, bad := range []string{"run_id=x", "since=yesterday", "limit=-1"} {
		v, _ := url.ParseQuery(bad)
		if _, err := ParseQuery(v); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}

// A peer's job: the peer's own record (diag, unknown) takes the conclusion
// of the copy its broker made from the forge, whatever the list order, and
// keeps the peer's own fields. Until then wait_for_run keeps waiting.
func TestMergeForgeCopy(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	dev := Record{ID: "tm-mac-1", Kind: "runner", Device: "mac", Executor: "darwin", Slot: 2, EndedAt: now,
		Conclusion: Unknown, LogSource: LogRunnerDiag}
	if settled(Snapshot{Records: []Record{dev}}, now.Add(time.Minute)) {
		t.Fatal("an unknown runner job settled before the forge grace")
	}
	if !settled(Snapshot{Records: []Record{dev}}, now.Add(ForgeGrace)) {
		t.Fatal("an unknown runner job never settled")
	}
	cp := ForgeReport{ID: "tm-mac-1", Device: "mac", Conclusion: "failure", FailedStep: "go test", Log: []byte("x")}.Record()
	cp.HasLog = true
	for _, lists := range [][][]Record{{{dev}, {cp}}, {{cp}, {dev}}} {
		got := Merge(lists...)
		if len(got) != 1 || got[0].Conclusion != Failure || got[0].FailedStep != "go test" || got[0].Executor != "darwin" ||
			got[0].Slot != 2 || got[0].LogSource != LogForge {
			t.Fatalf("merged = %+v", got)
		}
	}
}
