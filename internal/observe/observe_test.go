package observe

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/dispatch"
	"github.com/baney75/tentmaker/internal/mesh"
	"github.com/baney75/tentmaker/internal/runs"
)

// sample is one parsed metric line.
type sample struct {
	name   string
	labels map[string]string
	value  float64
}

var (
	metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelName  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// parseText checks the Prometheus text format (version 0.0.4) strictly and
// returns its samples: a HELP and a TYPE line before each family's
// samples, each family in one block, valid names, escaped label values,
// and histograms with cumulative buckets that end in +Inf equal to
// _count.
func parseText(t *testing.T, body string) []sample {
	t.Helper()
	var out []sample
	types := map[string]string{}
	helped := map[string]bool{}
	done := map[string]bool{}
	cur := ""
	sc := bufio.NewScanner(strings.NewReader(body))
	n := 0
	for sc.Scan() {
		n++
		line := sc.Text()
		switch {
		case line == "":
			t.Fatalf("line %d: empty line", n)
		case strings.HasPrefix(line, "# HELP "):
			f := strings.SplitN(strings.TrimPrefix(line, "# HELP "), " ", 2)
			if len(f) != 2 || !metricName.MatchString(f[0]) || helped[f[0]] {
				t.Fatalf("line %d: bad or repeated HELP: %q", n, line)
			}
			if cur != "" {
				done[cur] = true
			}
			if done[f[0]] {
				t.Fatalf("line %d: family %s appears twice", n, f[0])
			}
			helped[f[0]], cur = true, f[0]
		case strings.HasPrefix(line, "# TYPE "):
			f := strings.Fields(strings.TrimPrefix(line, "# TYPE "))
			if len(f) != 2 || f[0] != cur || types[f[0]] != "" {
				t.Fatalf("line %d: TYPE not right after its HELP: %q", n, line)
			}
			switch f[1] {
			case "counter", "gauge", "histogram":
			default:
				t.Fatalf("line %d: unknown type %q", n, f[1])
			}
			types[f[0]] = f[1]
		case strings.HasPrefix(line, "#"):
			t.Fatalf("line %d: stray comment %q", n, line)
		default:
			s := parseSample(t, n, line)
			fam := s.name
			if types[cur] == "histogram" {
				for _, suf := range []string{"_bucket", "_sum", "_count"} {
					fam = strings.TrimSuffix(fam, suf)
					if fam != s.name {
						break
					}
				}
			}
			if fam != cur || types[cur] == "" {
				t.Fatalf("line %d: sample %s outside its family (in %s)", n, s.name, cur)
			}
			if types[cur] == "counter" && !strings.HasSuffix(s.name, "_total") {
				t.Fatalf("line %d: counter %s has no _total suffix", n, s.name)
			}
			out = append(out, s)
		}
	}
	if !strings.HasSuffix(body, "\n") {
		t.Fatal("body does not end in a newline")
	}
	for name, typ := range types {
		if typ == "histogram" {
			checkHistogram(t, name, out)
		}
	}
	return out
}

func parseSample(t *testing.T, n int, line string) sample {
	t.Helper()
	s := sample{labels: map[string]string{}}
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		t.Fatalf("line %d: no value: %q", n, line)
	}
	s.name = line[:i]
	if !metricName.MatchString(s.name) {
		t.Fatalf("line %d: bad metric name %q", n, s.name)
	}
	rest := line[i:]
	if rest[0] == '{' {
		rest = rest[1:]
		for {
			eq := strings.Index(rest, `="`)
			if eq < 0 {
				t.Fatalf("line %d: bad labels: %q", n, line)
			}
			k := rest[:eq]
			if !labelName.MatchString(k) || strings.HasPrefix(k, "__") {
				t.Fatalf("line %d: bad label name %q", n, k)
			}
			if _, dup := s.labels[k]; dup {
				t.Fatalf("line %d: label %s twice", n, k)
			}
			rest = rest[eq+2:]
			var v strings.Builder
			j := 0
			for ; j < len(rest) && rest[j] != '"'; j++ {
				if rest[j] == '\\' {
					j++
					if j == len(rest) {
						break
					}
					switch rest[j] {
					case '\\', '"':
						v.WriteByte(rest[j])
					case 'n':
						v.WriteByte('\n')
					default:
						t.Fatalf("line %d: bad escape in %q", n, line)
					}
					continue
				}
				v.WriteByte(rest[j])
			}
			if j >= len(rest) {
				t.Fatalf("line %d: unterminated label value: %q", n, line)
			}
			s.labels[k] = v.String()
			rest = rest[j+1:]
			if strings.HasPrefix(rest, ",") {
				rest = rest[1:]
				continue
			}
			if !strings.HasPrefix(rest, "}") {
				t.Fatalf("line %d: bad labels: %q", n, line)
			}
			rest = rest[1:]
			break
		}
	}
	if !strings.HasPrefix(rest, " ") || strings.Contains(rest[1:], " ") {
		t.Fatalf("line %d: want one value after one space: %q", n, line)
	}
	v, err := strconv.ParseFloat(strings.Replace(rest[1:], "+Inf", "Inf", 1), 64)
	if err != nil {
		t.Fatalf("line %d: bad value: %q", n, line)
	}
	s.value = v
	return s
}

func checkHistogram(t *testing.T, name string, all []sample) {
	t.Helper()
	var buckets []sample
	count := -1.0
	for _, s := range all {
		switch s.name {
		case name + "_bucket":
			buckets = append(buckets, s)
		case name + "_count":
			count = s.value
		}
	}
	if len(buckets) == 0 || count < 0 {
		t.Fatalf("%s: no buckets or no count", name)
	}
	prev, prevLE := -1.0, -1.0
	for _, b := range buckets {
		le, err := strconv.ParseFloat(strings.Replace(b.labels["le"], "+Inf", "Inf", 1), 64)
		if err != nil || le <= prevLE || b.value < prev {
			t.Fatalf("%s: buckets not cumulative and ascending at le=%q", name, b.labels["le"])
		}
		prev, prevLE = b.value, le
	}
	if last := buckets[len(buckets)-1]; last.labels["le"] != "+Inf" || last.value != count {
		t.Fatalf("%s: last bucket %v, want le=+Inf equal to count %v", name, last, count)
	}
}

// value finds the one sample with name and every given label.
func value(t *testing.T, all []sample, name string, labels ...string) float64 {
	t.Helper()
	found, v := 0, 0.0
next:
	for _, s := range all {
		if s.name != name {
			continue
		}
		for i := 0; i+1 < len(labels); i += 2 {
			if s.labels[labels[i]] != labels[i+1] {
				continue next
			}
		}
		found++
		v = s.value
	}
	if found != 1 {
		t.Fatalf("%s%v: %d samples, want 1", name, labels, found)
	}
	return v
}

func TestMetricsFormatAndValues(t *testing.T) {
	body := string(Metrics(fixtureMetrics()))
	all := parseText(t, body)

	for _, s := range all {
		if s.labels["device"] != "studio" {
			t.Fatalf("%s has device=%q, want studio", s.name, s.labels["device"])
		}
	}
	want := []struct {
		name   string
		labels []string
		v      float64
	}{
		{"tentmaker_info", []string{"version", "0.4.0", "pool", "home"}, 1},
		{"tentmaker_paused", nil, 0},
		{"tentmaker_budget_profile", []string{"profile", "boost"}, 1},
		{"tentmaker_budget_cpus", nil, 8},
		{"tentmaker_budget_memory_bytes", nil, 16 << 30},
		{"tentmaker_budget_admit", nil, 1},
		{"tentmaker_guard_active", []string{"guard", "battery"}, 0},
		{"tentmaker_guard_active", []string{"guard", "memory"}, 0},
		{"tentmaker_slots", []string{"state", "used"}, 2},
		{"tentmaker_slots", []string{"state", "free"}, 2},
		{"tentmaker_jobs_running", nil, 2},
		{"tentmaker_executor_ready", []string{"executor", "linux-podman"}, 1},
		{"tentmaker_jobs_queued", []string{"repo", "baney75/tentmaker", "mode", "runner"}, 3},
		{"tentmaker_jobs_queued", []string{"repo", "baney75/notes", "mode", "checks"}, 1},
		{"tentmaker_broker_device_runners", []string{"worker", "laptop"}, 1},
		{"tentmaker_forge_api_requests_total", []string{"forge", "github"}, 5120},
		{"tentmaker_forge_api_billed_total", []string{"forge", "github"}, 820},
		{"tentmaker_forge_api_rate_remaining", []string{"forge", "github"}, 4180},
		{"tentmaker_forge_api_rate_limit", []string{"forge", "github"}, 5000},
		{"tentmaker_forge_api_rate_reset_timestamp_seconds", []string{"forge", "github"}, float64(fixtureNow.Unix() + 37*60)},
		// Counted since the daemon started, on studio, with a final result:
		// tm-studio-1 to 4. Not tm-studio-5 (before the start), 6 (waiting
		// for the forge) or the laptop's jobs.
		{"tentmaker_jobs_result_pending", nil, 1},
		{"tentmaker_jobs_finished_total", []string{"result", "success"}, 2},
		{"tentmaker_jobs_finished_total", []string{"result", "failure"}, 1},
		{"tentmaker_jobs_finished_total", []string{"result", "cancelled"}, 1},
		{"tentmaker_jobs_finished_total", []string{"result", "unknown"}, 0},
		{"tentmaker_jobs_finished_total", []string{"result", "error"}, 0},
		// Waits of 2, 3, 12 and 300 seconds.
		{"tentmaker_job_start_latency_seconds_bucket", []string{"le", "1"}, 0},
		{"tentmaker_job_start_latency_seconds_bucket", []string{"le", "2"}, 1},
		{"tentmaker_job_start_latency_seconds_bucket", []string{"le", "5"}, 2},
		{"tentmaker_job_start_latency_seconds_bucket", []string{"le", "15"}, 3},
		{"tentmaker_job_start_latency_seconds_bucket", []string{"le", "120"}, 3},
		{"tentmaker_job_start_latency_seconds_bucket", []string{"le", "300"}, 4},
		{"tentmaker_job_start_latency_seconds_sum", nil, 317},
		{"tentmaker_job_start_latency_seconds_count", nil, 4},
		{"tentmaker_cache_jobs_total", []string{"cache", "repo", "result", "hit"}, 2},
		{"tentmaker_cache_jobs_total", []string{"cache", "repo", "result", "miss"}, 1},
		{"tentmaker_cache_jobs_total", []string{"cache", "container_store", "result", "hit"}, 1},
		{"tentmaker_cache_jobs_total", []string{"cache", "container_store", "result", "miss"}, 1},
	}
	for _, w := range want {
		if got := value(t, all, w.name, w.labels...); got != w.v {
			t.Errorf("%s%v = %v, want %v", w.name, w.labels, got, w.v)
		}
	}
	// Every known guard is reported, so a dashboard sees 0 before 1.
	for _, g := range KnownGuards {
		value(t, all, "tentmaker_guard_active", "guard", g)
	}
	if os.Getenv("TENTMAKER_METRICS_OUT") != "" {
		_ = os.WriteFile(os.Getenv("TENTMAKER_METRICS_OUT"), []byte(body), 0o644)
	}
}

func TestMetricsGuardsAndHeldDevice(t *testing.T) {
	all := parseText(t, string(Metrics(MetricsInput{Now: fixtureNow, Self: laptop()})))
	for g, want := range map[string]float64{"battery": 1, "memory": 1, "disk": 0, "power": 0} {
		if got := value(t, all, "tentmaker_guard_active", "guard", g); got != want {
			t.Errorf("guard %s = %v, want %v", g, got, want)
		}
	}
	if v := value(t, all, "tentmaker_budget_profile", "profile", "off"); v != 1 {
		t.Errorf("profile off = %v", v)
	}
	if v := value(t, all, "tentmaker_budget_admit"); v != 0 {
		t.Errorf("admit = %v, want 0", v)
	}
	// No broker: no queue or API families.
	for _, s := range all {
		if strings.HasPrefix(s.name, "tentmaker_forge_api") || s.name == "tentmaker_jobs_queued" {
			t.Errorf("worker-only device reports %s", s.name)
		}
	}
	// With no history every counter still starts at 0.
	if v := value(t, all, "tentmaker_job_start_latency_seconds_count"); v != 0 {
		t.Errorf("latency count = %v", v)
	}
}

func TestMetricsUnknownRateLeftOmitted(t *testing.T) {
	s := studio()
	s.Broker.API = map[string]core.APIStats{"gitlab": {Requests: 3, RateLeft: -1}}
	all := parseText(t, string(Metrics(MetricsInput{Now: fixtureNow, Self: s})))
	for _, x := range all {
		if x.name == "tentmaker_forge_api_rate_remaining" || x.name == "tentmaker_forge_api_rate_limit" {
			t.Fatalf("%s reported with no reading", x.name)
		}
	}
	if v := value(t, all, "tentmaker_forge_api_requests_total", "forge", "gitlab"); v != 3 {
		t.Fatalf("requests = %v", v)
	}
}

func TestMetricsEscapesLabels(t *testing.T) {
	s := studio()
	s.Broker.Repos = []dispatch.RepoStatus{{Forge: "github", Name: "a\"b\\c\nd", Mode: "runner", Queued: 1}}
	all := parseText(t, string(Metrics(MetricsInput{Now: fixtureNow, Self: s})))
	// clean turns the newline to a space; the rest round-trips.
	if v := value(t, all, "tentmaker_jobs_queued", "repo", `a"b\c d`); v != 1 {
		t.Fatalf("queued = %v", v)
	}
}

func renderPage(t *testing.T, in PageInput) string {
	t.Helper()
	b, err := Page(in)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var attrURL = regexp.MustCompile(`(?i)\s(?:src|href|action|srcset|poster|data)\s*=\s*"([^"]*)"`)

func TestPageSelfContained(t *testing.T) {
	page := renderPage(t, fixturePage())
	if os.Getenv("TENTMAKER_PAGE_OUT") != "" {
		if err := os.WriteFile(os.Getenv("TENTMAKER_PAGE_OUT"), []byte(page), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range attrURL.FindAllStringSubmatch(page, -1) {
		u := html.UnescapeString(m[1])
		switch {
		case u == "metrics", strings.HasPrefix(u, "data:image/svg+xml"),
			strings.HasPrefix(u, "https://github.com/baney75/"), strings.HasPrefix(u, "https://gitlab.com/baney75/"):
		default:
			t.Errorf("page loads or links outside itself: %q", u)
		}
	}
	for _, bad := range []string{"<link rel=\"stylesheet\"", "@import", "url(http", "<iframe", "fonts.g"} {
		if strings.Contains(page, bad) {
			t.Errorf("page has %q", bad)
		}
	}
	// The CSP lets exactly the inline style and script run.
	for _, tag := range []string{"style", "script"} {
		re := regexp.MustCompile(`(?s)<` + tag + `>(.*?)</` + tag + `>`)
		ms := re.FindAllStringSubmatch(page, -1)
		if len(ms) != 1 {
			t.Fatalf("%d <%s> elements, want 1", len(ms), tag)
		}
		sum := sha256.Sum256([]byte(ms[0][1]))
		h := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if !strings.Contains(pageCSP, tag+"-src "+h) {
			t.Errorf("CSP does not allow the page's %s (%s)", tag, h)
		}
	}
	for _, want := range []string{
		"prefers-color-scheme: dark", "max-width: 640px", `name="viewport"`, `data-every="15"`,
		`id="live"`, // what the script swaps
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestPageContent(t *testing.T) {
	page := html.UnescapeString(renderPage(t, fixturePage()))
	for _, want := range []string{
		// Devices, this one first, with budget and why.
		"studio", "boost", "8 CPUs, 16 GiB", "2 of 4 slots used", "boost: plugged in, idle 42m",
		"laptop", "on battery at 31%", "Held back by: battery, memory",
		"nas", "Unreachable: no answer in 2s",
		// Running jobs with forge links.
		"https://github.com/baney75/tentmaker/actions/runs/19007", "running 3m",
		"https://gitlab.com/baney75/notes/-/pipelines/77",
		// Queue, deduplicated, and quota.
		"3 waiting", "4180 of 5000", "resets in 37m",
		// Failures: lint on studio, e2e on laptop, the old one.
		"golangci-lint", "playwright test", "https://github.com/baney75/tentmaker/actions/runs/19003",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if i, k := strings.Index(page, "<h3>studio</h3>"), strings.Index(page, "<h3>laptop</h3>"); i < 0 || k < i {
		t.Errorf("this device is not listed first")
	}
	// A cancelled job is not a failure.
	if strings.Contains(page, "test (windows)") {
		t.Errorf("cancelled job listed as a failure")
	}
}

func TestPageEmptyPool(t *testing.T) {
	page := renderPage(t, PageInput{Now: fixtureNow})
	for _, want := range []string{"No devices answered.", "Nothing running."} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func TestForgeLink(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/o/r/actions/runs/1":                       "https://github.com/o/r/actions/runs/1",
		" http://forge.lan/o/r/-/jobs/2 ":                             "http://forge.lan/o/r/-/jobs/2",
		"javascript:alert(1)":                                         "",
		"data:text/html,hi":                                           "",
		"https://user:pw@github.com/o/r":                              "",
		"https://user@github.com/o/r":                                 "",
		"//github.com/o/r":                                            "",
		"/actions/runs/1":                                             "",
		"https://github.com/o/r?token=ghp_" + strings.Repeat("a", 36): "",
		"": "",
	} {
		if got := forgeLink(in); got != want {
			t.Errorf("forgeLink(%q) = %q, want %q", in, got, want)
		}
	}
}

// Secrets that reach free text (an error, a job name, a reason) must not
// leave the device in either view.
func TestNoSecretsInEitherOutput(t *testing.T) {
	jit := strings.Repeat("eyJydW5uZXIiOnsiaWQiOjQyfX0+/", 6)
	secrets := []string{
		"ghp_" + strings.Repeat("Z9", 18),
		"github_pat_" + strings.Repeat("Q", 30),
		"glpat-" + strings.Repeat("x", 24),
		jit,
		"s3cr3tBearerValue",
		"hunter2pass",
		"jitConfigValue77",
	}
	leak := strings.Join([]string{
		secrets[0], secrets[1], secrets[2], jit,
		"Authorization: Bearer " + secrets[4],
		"https://bot:" + secrets[5] + "@git.example.com/o/r",
		"ACTIONS_RUNNER_INPUT_JITCONFIG=" + secrets[6],
	}, " ")

	s := studio()
	s.Version, s.Pool, s.Why, s.Paused, s.PauseReason = leak, leak, leak, true, leak
	s.Decision.Profile = leak
	s.Decision.Guards = []string{leak}
	s.Problems = []core.Problem{{Check: leak, What: leak, Fix: leak}}
	s.Worker.Executors[0].Name = leak
	s.Worker.Running[0].Repo, s.Worker.Running[0].Job = leak, leak
	s.Broker.Repos[0].Name, s.Broker.Repos[0].Forge, s.Broker.Repos[0].Mode = leak, leak, leak
	s.Broker.Devices[0].Name = leak
	s.Broker.API[leak] = s.Broker.API["github"]
	rs := fixtureRuns()
	for i := range rs {
		rs[i].Repo, rs[i].Job, rs[i].FailedStep, rs[i].Note, rs[i].Device = leak, leak, leak, leak, leak
		rs[i].RunURL = "https://x-access-token:" + secrets[0] + "@github.com/o/r/actions/runs/1"
	}
	// Some runs keep this device's name, so they count in the metrics.
	rs[0].Device, rs[2].Device = "studio", "studio"
	rs[0].Conclusion = leak

	tally := NewTally(time.Minute)
	for _, r := range rs {
		if r.Device == "studio" {
			tally.Ended(r, true, fixtureNow)
		}
	}
	metrics := string(Metrics(MetricsInput{Now: fixtureNow, Self: s, Jobs: tally.Counts(fixtureNow)}))
	parseText(t, metrics)
	page := renderPage(t, PageInput{
		Now: fixtureNow, Pool: leak, Runs: rs, Failures: rs, Errors: []string{leak},
		Devices: []mesh.DeviceStatus{
			{Device: "studio", Self: true, Broker: true, Version: leak, Status: rawStatus(s)},
			{Device: leak, Version: leak, Error: leak},
		},
	})
	for name, out := range map[string]string{
		"metrics": metrics, "page": page, "page text": html.UnescapeString(page),
	} {
		for _, sec := range secrets {
			for _, part := range []string{sec, sec[:len(sec)/2], sec[len(sec)/2:]} {
				if len(part) >= 8 && strings.Contains(out, part) {
					t.Errorf("%s leaks %q", name, part)
				}
			}
		}
		if !strings.Contains(out, runs.Mask) {
			t.Errorf("%s shows no mask: the leak never reached it, so the test proves nothing", name)
		}
	}
}

type fakeSource struct {
	m   MetricsInput
	p   PageInput
	err error
}

func (f fakeSource) MetricsInput(context.Context) (MetricsInput, error) { return f.m, f.err }
func (f fakeSource) PageInput(context.Context) (PageInput, error)       { return f.p, f.err }

func TestHandler(t *testing.T) {
	h := Handler(fakeSource{m: fixtureMetrics(), p: fixturePage()})
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	w := get("/metrics")
	if w.Code != 200 || w.Header().Get("Content-Type") != MetricsContentType {
		t.Fatalf("metrics: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	parseText(t, w.Body.String())
	for _, p := range []string{"/", "/status"} {
		w := get(p)
		hd := w.Header()
		if w.Code != 200 || !strings.HasPrefix(hd.Get("Content-Type"), "text/html") ||
			hd.Get("Content-Security-Policy") != pageCSP || hd.Get("X-Frame-Options") != "DENY" ||
			hd.Get("Cache-Control") != "no-store" || hd.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: %d %v", p, w.Code, hd)
		}
	}
	if w := get("/other"); w.Code != 404 {
		t.Fatalf("/other: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics: %d", w.Code)
	}

	failing := Handler(fakeSource{err: errors.New("token ghp_" + strings.Repeat("a", 36) + " refused")})
	for _, p := range []string{"/metrics", "/"} {
		w := httptest.NewRecorder()
		failing.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "ghp_") {
			t.Fatalf("%s with a failing source: %d %q", p, w.Code, w.Body.String())
		}
	}
}

func TestFormatFloat(t *testing.T) {
	for in, want := range map[float64]string{1: "1", 0.5: "0.5", 17179869184: "17179869184", 1e21: "1e+21"} {
		if got := formatFloat(in); got != want {
			t.Errorf("formatFloat(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestMetricsBuildCache(t *testing.T) {
	// A device that serves no build cache reports none of its families.
	for _, s := range parseText(t, string(Metrics(MetricsInput{Now: fixtureNow, Self: laptop()}))) {
		if strings.HasPrefix(s.name, "tentmaker_build_cache") {
			t.Fatalf("no build cache, but %s", s.name)
		}
	}
	in := MetricsInput{Now: fixtureNow, Self: laptop(), BuildCache: []BuildCacheCounts{
		{Tool: "bazel", Hits: 7, Misses: 2, Stored: 3, NotStored: 1, BytesServed: 1000, BytesStored: 500},
		{Tool: "turbo"},
	}}
	all := parseText(t, string(Metrics(in)))
	for _, c := range []struct {
		name, k1, v1, k2, v2 string
		want                 float64
	}{
		{"tentmaker_build_cache_requests_total", "tool", "bazel", "result", "hit", 7},
		{"tentmaker_build_cache_requests_total", "tool", "bazel", "result", "miss", 2},
		{"tentmaker_build_cache_requests_total", "tool", "bazel", "result", "stored", 3},
		{"tentmaker_build_cache_requests_total", "tool", "bazel", "result", "not_stored", 1},
		{"tentmaker_build_cache_requests_total", "tool", "turbo", "result", "hit", 0},
		{"tentmaker_build_cache_bytes_total", "tool", "bazel", "direction", "served", 1000},
		{"tentmaker_build_cache_bytes_total", "tool", "bazel", "direction", "stored", 500},
	} {
		if got := value(t, all, c.name, c.k1, c.v1, c.k2, c.v2); got != c.want {
			t.Errorf("%s{%s=%s,%s=%s} = %v, want %v", c.name, c.k1, c.v1, c.k2, c.v2, got, c.want)
		}
	}
}
