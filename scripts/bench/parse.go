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
	URL        string             `json:"url,omitempty"`
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
}

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
		if !j.CompletedAt.IsZero() && !j.StartedAt.IsZero() {
			s.JobS = round1(j.CompletedAt.Sub(j.StartedAt).Seconds())
		}
		log, err := os.ReadFile(filepath.Join(dir, "logs", strconv.FormatInt(j.ID, 10)+".txt"))
		if err != nil {
			missing = append(missing, "log of "+j.Name)
		}
		s.Phases, s.MBps = phaseTimes(parsePhases(string(log)), epoch(j.StartedAt))
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
// (the check log Tentmaker kept).
func parseTentmakerSample(dir string) (Sample, error) {
	var meta tmMeta
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return Sample{}, err
	}
	s := Sample{Workload: meta.Workload, Platform: "tentmaker", Round: meta.Round, Rep: meta.Rep, URL: meta.URL, Conclusion: "none", StartS: -1}
	var sts []ghStatus
	if err := readJSON(filepath.Join(dir, "statuses.json"), &sts); err != nil {
		return s, err
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
	if pending != nil {
		start = epoch(pending.CreatedAt)
		if meta.PushEpoch > 0 && pending.CreatedAt.Unix() >= meta.PushEpoch {
			s.StartS = round1(start - float64(meta.PushEpoch))
		}
		if final != nil && !final.CreatedAt.Before(pending.CreatedAt) {
			s.JobS = round1(final.CreatedAt.Sub(pending.CreatedAt).Seconds())
		}
	}
	log, _ := os.ReadFile(filepath.Join(dir, "log.txt"))
	s.Phases, s.MBps = phaseTimes(parsePhases(string(log)), start)
	return s, nil
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
			s, err := parseTentmakerSample(sd)
			if err != nil {
				r.Missing = append(r.Missing, fmt.Sprintf("Tentmaker sample %s/%s: %v", filepath.Base(wd), filepath.Base(sd), err))
				continue
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
