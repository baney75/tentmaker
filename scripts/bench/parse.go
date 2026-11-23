package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sample is one run of one workload on one platform.
type Sample struct {
	Workload   string             `json:"workload"`
	Platform   string             `json:"platform"` // "github" or "tentmaker"
	Round      string             `json:"round"`    // "cold" or "warm"
	Rep        int                `json:"rep"`
	OK         bool               `json:"ok"`
	Conclusion string             `json:"conclusion"`
	StartS     float64            `json:"start_s"` // queued to running; -1 when not measured
	JobS       float64            `json:"job_s"`   // running to done
	Phases     map[string]float64 `json:"phases"`  // seconds; "setup" is job start to the first mark
	MBps       map[string]float64 `json:"mbps,omitempty"`
	// CacheHit says whether the job started with the cache an earlier run
	// of the workload left; nil when the job didn't record it.
	CacheHit *bool `json:"cache_hit,omitempty"`
	// StoreBound says whether a Tentmaker Docker job ran with its repo's
	// container store bound; nil when the job didn't record it.
	StoreBound *bool  `json:"store_bound,omitempty"`
	Go         string `json:"go,omitempty"` // the Go that compiled, as `go env GOVERSION` printed it
	URL        string `json:"url,omitempty"`
}

// Device describes the machine Tentmaker ran on, as bench.sh recorded it.
type Device struct {
	Workload     string  `json:"workload"`
	ImageOS      string  `json:"image_os"`
	ImageVersion string  `json:"image_version"`
	CPUs         int     `json:"cpus"`
	CPUModel     string  `json:"cpu_model"`
	MemoryGiB    float64 `json:"memory_gib"`
	Version      string  `json:"version"`
	JobURL       string  `json:"job_url,omitempty"`
	Budget       any     `json:"budget,omitempty"`
	// API is the daemon's API call counts at the end (tm/<workload>/api.json).
	API *DeviceAPI `json:"api,omitempty"`
}

// DeviceAPI is `tentmaker status --json`'s broker.api.github, plus when
// bench.sh read it.
type DeviceAPI struct {
	Since       time.Time `json:"since"`
	At          time.Time `json:"at"`
	Requests    int64     `json:"requests"`
	NotModified int64     `json:"not_modified"`
	Billed      int64     `json:"billed"`
	RateLimited int64     `json:"rate_limited"`
	RateLeft    int64     `json:"rate_left"`  // the shared limit left at the end
	RateLimit   int64     `json:"rate_limit"` // the hourly limit
}

// Hours is how long the counts ran.
func (a *DeviceAPI) Hours() float64 { return a.At.Sub(a.Since).Hours() }

// Run is everything one bench run produced.
type Run struct {
	URL     string   `json:"url"`
	SHA     string   `json:"sha"`
	Date    string   `json:"date"`
	Samples []Sample `json:"samples"`
	Devices []Device `json:"devices"`
	Missing []string `json:"missing,omitempty"` // what could not be read
}

// ghJob is the part of GitHub's job object the bench reads.
type ghJob struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	HTMLURL     string    `json:"html_url"`
}

// ghName matches the hosted jobs bench.yml names: "gh go-test cold 2 / go-test".
var ghName = regexp.MustCompile(`^gh ([a-z0-9-]+) (cold|warm) ([0-9]+) / `)

// phaseLine matches a mark that phase.sh report printed, anywhere in a line
// (GitHub prefixes each log line with a timestamp).
var phaseLine = regexp.MustCompile(`BENCH_PHASE (\S+) (begin|end) ([0-9]+(?:\.[0-9]+)?)(?: ([0-9]+))?`)

// metaLine matches a fact phase.sh meta recorded: "BENCH_META go go1.25.0".
var metaLine = regexp.MustCompile(`BENCH_META ([a-z_]+) (\S+)`)

// parseMeta reads the BENCH_META lines in a job log. The last value of a key
// counts.
func parseMeta(log string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(log, "\n") {
		if m := metaLine.FindStringSubmatch(line); m != nil {
			out[m[1]] = m[2]
		}
	}
	return out
}

// readLog fills the parts of s that come from its job log: phases, rates,
// the cache hit, the container store bind and the Go version. A passing job always ends with
// phase.sh report, so a log with no marks means the job didn't finish
// its work (or its log is gone): the sample doesn't count as passing.
// It returns why, or "".
func readLog(s *Sample, log string, jobStart float64) string {
	marks := parsePhases(log)
	s.Phases, s.MBps = phaseTimes(marks, jobStart)
	meta := parseMeta(log)
	s.CacheHit = boolMeta(meta["cache_hit"])
	s.StoreBound = boolMeta(meta["store_bound"])
	s.Go = meta["go"]
	if s.OK && len(marks) == 0 {
		s.OK = false
		return "no phase report in its log; not counted as passing"
	}
	return ""
}

func ptr[T any](v T) *T { return &v }

// boolMeta reads a 1 or 0 fact; anything else is not recorded.
func boolMeta(v string) *bool {
	switch v {
	case "1":
		return ptr(true)
	case "0":
		return ptr(false)
	}
	return nil
}

type mark struct {
	begin, end float64
	bytes      int64
	hasB, hasE bool
}

// parsePhases reads the BENCH_PHASE marks in a job log. The first begin and
// the last end of a name count.
func parsePhases(log string) map[string]*mark {
	out := map[string]*mark{}
	sc := bufio.NewScanner(strings.NewReader(log))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		m := phaseLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		t, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			continue
		}
		p := out[m[1]]
		if p == nil {
			p = &mark{}
			out[m[1]] = p
		}
		if m[2] == "begin" {
			if !p.hasB {
				p.begin, p.hasB = t, true
			}
			continue
		}
		p.end, p.hasE = t, true
		if m[4] != "" {
			p.bytes, _ = strconv.ParseInt(m[4], 10, 64)
		}
	}
	return out
}

// phaseTimes turns marks into seconds per phase, adds "setup" (jobStart to
// the first begin) and MB/s (decimal megabytes) for phases that moved bytes.
func phaseTimes(marks map[string]*mark, jobStart float64) (map[string]float64, map[string]float64) {
	ph := map[string]float64{}
	var mbps map[string]float64
	first := 0.0
	for name, m := range marks {
		if m.hasB && (first == 0 || m.begin < first) {
			first = m.begin
		}
		if !m.hasB || !m.hasE || m.end < m.begin {
			continue
		}
		d := m.end - m.begin
		ph[name] = round1(d)
		if m.bytes > 0 && d > 0 {
			if mbps == nil {
				mbps = map[string]float64{}
			}
			mbps[name] = round1(float64(m.bytes) / d / 1e6)
		}
	}
	if first > 0 && jobStart > 0 && first >= jobStart {
		ph["setup"] = round1(first - jobStart)
	}
	return ph, mbps
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// parseGitHub reads the hosted side: jobs.json (the run's jobs) and
// logs/<id>.txt for each bench job.
// It also returns the links of the other jobs by name, so the Tentmaker
// samples can link to the device job that ran them ("tentmaker <workload>").
func parseGitHub(dir string) ([]Sample, []string, map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "jobs.json"))
	if err != nil {
		return nil, nil, nil, err
	}
	var jobs []ghJob
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, nil, nil, fmt.Errorf("jobs.json: %w", err)
	}
	var out []Sample
	var missing []string
	urls := map[string]string{}
	for _, j := range jobs {
		urls[j.Name] = j.HTMLURL
		m := ghName.FindStringSubmatch(j.Name)
		if m == nil {
			continue
		}
		rep, _ := strconv.Atoi(m[3])
		s := Sample{Workload: m[1], Platform: "github", Round: m[2], Rep: rep, Conclusion: j.Conclusion, URL: j.HTMLURL, StartS: -1}
		if j.Status != "completed" {
			s.Conclusion = j.Status
		}
		s.OK = s.Conclusion == "success"
		if !j.StartedAt.IsZero() && !j.CreatedAt.IsZero() && !j.StartedAt.Before(j.CreatedAt) {
			s.StartS = round1(j.StartedAt.Sub(j.CreatedAt).Seconds())
		}
		// A job time needs a start and an end, in order. Without them the
		// sample can't count as passing: a zero would look fast.
		if !j.StartedAt.IsZero() && !j.CompletedAt.IsZero() && j.CompletedAt.After(j.StartedAt) {
			s.JobS = round1(j.CompletedAt.Sub(j.StartedAt).Seconds())
		} else if s.OK {
			s.OK = false
			missing = append(missing, j.Name+": no ordered start and end time; not counted as passing")
		}
		log, err := os.ReadFile(filepath.Join(dir, "logs", strconv.FormatInt(j.ID, 10)+".txt"))
		if err != nil {
			missing = append(missing, "log of "+j.Name)
		}
		if why := readLog(&s, string(log), epoch(j.StartedAt)); why != "" {
			missing = append(missing, j.Name+": "+why)
		}
		out = append(out, s)
	}
	return out, missing, urls, nil
}

// tmMeta is what bench.sh wrote next to each Tentmaker sample.
type tmMeta struct {
	Workload  string `json:"workload"`
	Rep       int    `json:"rep"`
	Round     string `json:"round"`
	SHA       string `json:"sha"`
	Context   string `json:"context"`
	PushEpoch int64  `json:"push_epoch"`
	URL       string `json:"url"`
}

type ghStatus struct {
	Context   string    `json:"context"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

// parseTentmakerSample reads one sample folder: meta.json, statuses.json
// (the commit's statuses, newest first, as GitHub lists them) and log.txt
// (the check log Tentmaker kept). The second result says why a sample
// that passed doesn't count as passing, or is "".
func parseTentmakerSample(dir string) (Sample, string, error) {
	var meta tmMeta
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return Sample{}, "", err
	}
	s := Sample{Workload: meta.Workload, Platform: "tentmaker", Round: meta.Round, Rep: meta.Rep, URL: meta.URL, Conclusion: "none", StartS: -1}
	var sts []ghStatus
	if err := readJSON(filepath.Join(dir, "statuses.json"), &sts); err != nil {
		return s, "", err
	}
	var pending, final *ghStatus
	for i := range sts {
		st := &sts[i]
		if st.Context != meta.Context {
			continue
		}
		switch st.State {
		case "pending":
			if pending == nil || st.CreatedAt.Before(pending.CreatedAt) {
				pending = st
			}
		default:
			if final == nil || st.CreatedAt.After(final.CreatedAt) {
				final = st
			}
		}
	}
	if final != nil {
		s.Conclusion = final.State
	}
	s.OK = s.Conclusion == "success"
	start := 0.0
	var why string
	if pending != nil {
		start = epoch(pending.CreatedAt)
		if meta.PushEpoch > 0 && pending.CreatedAt.Unix() >= meta.PushEpoch {
			s.StartS = round1(start - float64(meta.PushEpoch))
		}
	}
	// The job time is the pending status to the final one. Without both,
	// in order, the sample can't count as passing: a zero would look fast.
	if pending != nil && final != nil && final.CreatedAt.After(pending.CreatedAt) {
		s.JobS = round1(final.CreatedAt.Sub(pending.CreatedAt).Seconds())
	} else if s.OK {
		s.OK = false
		why = "no pending status before its final one; not counted as passing"
	}
	log, _ := os.ReadFile(filepath.Join(dir, "log.txt"))
	if w := readLog(&s, string(log), start); why == "" {
		why = w
	}
	return s, why, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}

// LoadRun reads a raw bench folder:
//
//	run.json                        {url, sha, date}
//	gh/jobs.json, gh/logs/<id>.txt  the hosted side
//	tm/<workload>/device.json       the Tentmaker device
//	tm/<workload>/<rep>-<round>/    one Tentmaker sample
func LoadRun(dir string) (Run, error) {
	var r Run
	if err := readJSON(filepath.Join(dir, "run.json"), &r); err != nil {
		return r, err
	}
	gh, missing, urls, err := parseGitHub(filepath.Join(dir, "gh"))
	if err != nil {
		r.Missing = append(r.Missing, "GitHub-hosted jobs: "+err.Error())
	}
	r.Samples = append(r.Samples, gh...)
	r.Missing = append(r.Missing, missing...)
	wls, _ := filepath.Glob(filepath.Join(dir, "tm", "*"))
	sort.Strings(wls)
	for _, wd := range wls {
		var d Device
		jobURL := urls["tentmaker "+filepath.Base(wd)]
		if err := readJSON(filepath.Join(wd, "device.json"), &d); err == nil {
			if d.JobURL == "" {
				d.JobURL = jobURL
			}
			var a DeviceAPI
			if err := readJSON(filepath.Join(wd, "api.json"), &a); err == nil && !a.Since.IsZero() {
				d.API = &a
			}
			r.Devices = append(r.Devices, d)
		} else {
			r.Missing = append(r.Missing, "device of "+filepath.Base(wd))
		}
		sds, _ := filepath.Glob(filepath.Join(wd, "*-*"))
		sort.Strings(sds)
		for _, sd := range sds {
			if st, err := os.Stat(sd); err != nil || !st.IsDir() {
				continue
			}
			s, why, err := parseTentmakerSample(sd)
			if err != nil {
				r.Missing = append(r.Missing, fmt.Sprintf("Tentmaker sample %s/%s: %v", filepath.Base(wd), filepath.Base(sd), err))
				continue
			}
			if why != "" {
				r.Missing = append(r.Missing, fmt.Sprintf("Tentmaker sample %s/%s: %s", filepath.Base(wd), filepath.Base(sd), why))
			}
			if s.URL == "" {
				s.URL = jobURL
			}
			r.Samples = append(r.Samples, s)
		}
	}
	sort.SliceStable(r.Samples, func(i, j int) bool {
		a, b := r.Samples[i], r.Samples[j]
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if a.Round != b.Round {
			return a.Round < b.Round
		}
		return a.Rep < b.Rep
	})
	return r, nil
}
