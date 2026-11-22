// Package config is Tentmaker's on-disk configuration: one TOML file per
// device. People edit it by hand or with `tentmaker config set`; agents use
// the CLI with --json. Every field has a safe default, so an empty file works.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Size is a byte count written like "4GiB", "512MiB", "8G" or "0".
type Size int64

const (
	KiB Size = 1 << 10
	MiB Size = 1 << 20
	GiB Size = 1 << 30

	// MaxSize is the largest size ParseSize accepts (1 EiB). Sizes are
	// added together (a job plus memory kept free), so the cap keeps those
	// sums far from overflowing.
	MaxSize Size = 1 << 60
)

var sizeRe = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*([KMGT]?)(I?B?)\s*$`)

// ParseSize reads "4GiB", "4G", "4GB" (all binary), "512MiB" or "0".
func ParseSize(s string) (Size, error) {
	m := sizeRe.FindStringSubmatch(strings.ToUpper(s))
	if m == nil {
		return 0, fmt.Errorf("size %q: use a number with KiB, MiB, GiB or TiB, like 8GiB", s)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	mult := map[string]float64{"": 1, "K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40}[m[2]]
	if m[2] == "" && m[1] != "0" && m[3] == "" {
		return 0, fmt.Errorf("size %q has no unit: write %sGiB or %sMiB", s, m[1], m[1])
	}
	// Compare as floats: converting an out-of-range float to an integer
	// gives a different garbage value on each CPU.
	if v := n * mult; !(v <= float64(MaxSize)) {
		return 0, fmt.Errorf("size %q is larger than 1 EiB", s)
	}
	return Size(n * mult), nil
}

func (s Size) String() string {
	switch {
	case s == 0:
		return "0"
	case s%GiB == 0:
		return fmt.Sprintf("%dGiB", s/GiB)
	case s%MiB == 0:
		return fmt.Sprintf("%dMiB", s/MiB)
	case s%KiB == 0:
		return fmt.Sprintf("%dKiB", s/KiB)
	default:
		// Exact bytes, so Save and Load keep "1.5KiB" or "100B" as is.
		return fmt.Sprintf("%dB", int64(s))
	}
}

func (s Size) MarshalText() ([]byte, error) { return []byte(s.String()), nil }
func (s *Size) UnmarshalText(b []byte) error {
	v, err := ParseSize(string(b))
	*s = v
	return err
}

// Duration is a time.Duration written like "90s", "3h" or "10m".
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	*d = Duration(v)
	return err
}
func (d Duration) D() time.Duration { return time.Duration(d) }

// Profile is an amount of the machine CI may use.
type Profile struct {
	Memory Size    `toml:"memory" json:"memory"`
	CPUs   float64 `toml:"cpus" json:"cpus"`
}

// Window overrides the profile on some days and hours, in local time.
// From may be later than To ("22:00" to "07:00" spans midnight).
type Window struct {
	Name    string   `toml:"name" json:"name"`
	Days    []string `toml:"days" json:"days"` // mon..sun; empty = every day
	From    string   `toml:"from" json:"from"` // "HH:MM"
	To      string   `toml:"to" json:"to"`     // "HH:MM"
	Profile string   `toml:"profile" json:"profile"`
	// Profile is "everyday", "boost" or "off".
}

// Budget is how much of this machine CI may use, and when.
type Budget struct {
	// Everyday applies whenever Tentmaker runs and nothing better applies.
	Everyday Profile `toml:"everyday" json:"everyday"`
	// Boost applies when the machine is plugged in (and, if BoostNeedsIdle,
	// the user has been away for IdleAfter). Desktops count as plugged in.
	Boost          Profile  `toml:"boost" json:"boost"`
	BoostNeedsIdle bool     `toml:"boost_needs_idle" json:"boost_needs_idle"`
	IdleAfter      Duration `toml:"idle_after" json:"idle_after"`
	// On battery: take no new jobs below PauseBelow percent, stop running
	// jobs below StopBelow. OnBattery picks the profile while on battery
	// and above PauseBelow ("everyday" or "off").
	OnBattery  string   `toml:"on_battery" json:"on_battery"`
	PauseBelow int      `toml:"pause_below_percent" json:"pause_below_percent"`
	StopBelow  int      `toml:"stop_below_percent" json:"stop_below_percent"`
	Schedule   []Window `toml:"schedule" json:"schedule"`
}

// Job sizes each job. Slots = how many jobs fit in the current profile.
type Job struct {
	Memory      Size     `toml:"memory" json:"memory"`
	CPUs        float64  `toml:"cpus" json:"cpus"`
	MaxDuration Duration `toml:"max_duration" json:"max_duration"`
	IdleTTL     Duration `toml:"idle_ttl" json:"idle_ttl"`
	MaxSlots    int      `toml:"max_slots" json:"max_slots"` // 0 = no extra cap
}

// Guards keep CI out of the way of what the machine is for.
type Guards struct {
	// PauseWhenRunning lists process names (case-insensitive), such as games
	// or a video call, that drop boost to the reserved everyday budget.
	PauseWhenRunning []string `toml:"pause_when_running" json:"pause_when_running"`
	MinFreeDisk      Size     `toml:"min_free_disk" json:"min_free_disk"`
	StopBelowDisk    Size     `toml:"stop_below_disk" json:"stop_below_disk"`
	// MinFreeMemory: take no new job unless this much memory stays free for
	// the user after the job's own memory.
	MinFreeMemory Size `toml:"min_free_memory" json:"min_free_memory"`
	// MaxLoadPerCPU drops boost to everyday while load average / CPUs
	// exceeds it (0 = off).
	MaxLoadPerCPU float64 `toml:"max_load_per_cpu" json:"max_load_per_cpu"`
	// AllowLAN lets CI jobs reach private (RFC 1918) addresses. The tailnet,
	// link-local, this host and the mesh port stay blocked either way. Linux
	// and Windows only; rerun setup after changing it.
	AllowLAN bool `toml:"allow_lan" json:"allow_lan"`
}

// Repo is one repository whose jobs this device serves.
type Repo struct {
	Forge string `toml:"forge" json:"forge"` // "github" (default), "forgejo"
	Name  string `toml:"name" json:"name"`   // "owner/name"
	// Public repos run only with AllowPublic, after the forge requires
	// approval for outside contributors.
	AllowPublic bool `toml:"allow_public" json:"allow_public"`
	// Mode is "runner" (the forge's own runner; GitHub and Forgejo) or
	// "checks" (Tentmaker runs workflow jobs itself and posts commit
	// statuses; any forge, and GitHub repos whose hosted Actions don't run).
	Mode string `toml:"mode" json:"mode"`
	// Workflows to run in checks mode (empty = every workflow under
	// .github/workflows or .forgejo/workflows triggered by push or pull_request).
	Workflows []string `toml:"workflows" json:"workflows"`
	// Authors whose PR heads checks mode runs (empty = the repo owner only).
	Authors []string `toml:"authors" json:"authors"`
	// Branches whose heads checks mode runs besides the default branch, such
	// as a release branch. Only people who can push to the repo move them.
	Branches []string `toml:"branches" json:"branches"`
}

// Forge is how to reach one forge.
type Forge struct {
	Kind string `toml:"kind" json:"kind"` // "github", "forgejo", "gitlab"
	URL  string `toml:"url" json:"url"`   // API base; empty = https://api.github.com
	// Token comes from the secret store, never this file.
}

// Mesh is how devices on one tailnet form a pool.
type Mesh struct {
	Enabled bool   `toml:"enabled" json:"enabled"`
	Pool    string `toml:"pool" json:"pool"` // devices in the same pool share work
	Port    int    `toml:"port" json:"port"` // tailnet-only peer API port
	// Members limits the pool to these tailnet login names (empty = devices
	// owned by the same tailnet user as this one).
	Members []string `toml:"members" json:"members"`
}

// Update is how Tentmaker updates itself.
type Update struct {
	Channel string `toml:"channel" json:"channel"` // "stable" or "dev"
	Auto    bool   `toml:"auto" json:"auto"`       // apply between jobs
}

// Config is the whole file.
type Config struct {
	Device    string           `toml:"device" json:"device"` // empty = hostname
	Labels    []string         `toml:"labels" json:"labels"` // extra runner labels
	Budget    Budget           `toml:"budget" json:"budget"`
	Job       Job              `toml:"job" json:"job"`
	Guards    Guards           `toml:"guards" json:"guards"`
	Repos     []Repo           `toml:"repos" json:"repos"`
	Forges    map[string]Forge `toml:"forges" json:"forges"`
	DenyRepos []string         `toml:"deny_repo_patterns" json:"deny_repo_patterns"`
	Mesh      Mesh             `toml:"mesh" json:"mesh"`
	Update    Update           `toml:"update" json:"update"`
	StateDir  string           `toml:"state_dir" json:"state_dir"`
	CacheDir  string           `toml:"cache_dir" json:"cache_dir"`
	CacheMax  Size             `toml:"cache_max" json:"cache_max"`
}

// Default returns the config a fresh install starts with. Memory defaults
// scale with the machine: everyday is a quarter of RAM (at least 2 GiB, at
// most 8 GiB), boost is half (at most 32 GiB).
func Default(totalMemory int64, cpus int) Config {
	q := clampSize(Size(totalMemory/4), 2*GiB, 8*GiB)
	h := clampSize(Size(totalMemory/2), q, 32*GiB)
	everydayCPU := maxf(1, float64(cpus)/4)
	boostCPU := maxf(everydayCPU, float64(cpus)/2)
	return Config{
		Budget: Budget{
			Everyday:   Profile{Memory: roundGiB(q), CPUs: everydayCPU},
			Boost:      Profile{Memory: roundGiB(h), CPUs: boostCPU},
			IdleAfter:  Duration(10 * time.Minute),
			OnBattery:  "everyday",
			PauseBelow: 40,
			StopBelow:  15,
		},
		Job: Job{
			Memory:      2 * GiB,
			CPUs:        2,
			MaxDuration: Duration(3 * time.Hour),
			IdleTTL:     Duration(90 * time.Second),
		},
		Guards: Guards{
			MinFreeDisk:   20 * GiB,
			StopBelowDisk: 5 * GiB,
			MinFreeMemory: 2 * GiB,
			MaxLoadPerCPU: 0,
		},
		Forges:    map[string]Forge{"github": {Kind: "github", URL: "https://api.github.com"}},
		DenyRepos: []string{},
		Mesh:      Mesh{Enabled: true, Pool: "default", Port: 7433},
		Update:    Update{Channel: "stable", Auto: true},
		CacheMax:  10 * GiB,
	}
}

// Validate returns every problem with c, in plain language.
func (c Config) Validate() []string {
	var p []string
	b := c.Budget
	if b.Everyday.Memory < 0 || b.Boost.Memory < 0 {
		p = append(p, "budget memory can't be negative")
	}
	if b.Boost.Memory != 0 && b.Boost.Memory < b.Everyday.Memory {
		p = append(p, "budget.boost.memory is smaller than budget.everyday.memory; boost should be the larger one")
	}
	if c.Job.Memory <= 0 || !(c.Job.CPUs > 0) {
		p = append(p, "job.memory and job.cpus must be above zero")
	}
	for _, x := range []struct {
		name string
		v    float64
	}{{"budget.everyday.cpus", b.Everyday.CPUs}, {"budget.boost.cpus", b.Boost.CPUs}, {"job.cpus", c.Job.CPUs}} {
		if math.IsNaN(x.v) || math.IsInf(x.v, 0) || x.v < 0 {
			p = append(p, x.name+" must be a finite number of CPUs, 0 or more")
		}
	}
	if c.Job.MaxSlots < 0 {
		p = append(p, "job.max_slots can't be negative")
	}
	if b.Everyday.Memory > 0 && b.Everyday.Memory < c.Job.Memory {
		p = append(p, fmt.Sprintf("budget.everyday.memory (%s) is below one job (%s), so no job fits outside boost", b.Everyday.Memory, c.Job.Memory))
	}
	if b.StopBelow > b.PauseBelow {
		p = append(p, "budget.stop_below_percent must not be above budget.pause_below_percent")
	}
	switch b.OnBattery {
	case "", "everyday", "off":
	default:
		p = append(p, `budget.on_battery must be "everyday" or "off"`)
	}
	for i, w := range b.Schedule {
		if _, err := ParseClock(w.From); err != nil {
			p = append(p, fmt.Sprintf("budget.schedule[%d].from: %v", i, err))
		}
		if _, err := ParseClock(w.To); err != nil {
			p = append(p, fmt.Sprintf("budget.schedule[%d].to: %v", i, err))
		}
		switch w.Profile {
		case "everyday", "boost", "off":
		default:
			p = append(p, fmt.Sprintf(`budget.schedule[%d].profile must be "everyday", "boost" or "off"`, i))
		}
		for _, d := range w.Days {
			if _, ok := Weekdays[strings.ToLower(d)]; !ok {
				p = append(p, fmt.Sprintf("budget.schedule[%d]: unknown day %q (use mon..sun)", i, d))
			}
		}
	}
	for _, r := range c.Repos {
		if r.Name == "" || strings.HasPrefix(r.Name, "/") || strings.HasSuffix(r.Name, "/") || !strings.Contains(r.Name, "/") {
			p = append(p, fmt.Sprintf("repo %q must be owner/name (GitLab groups: group/sub/name)", r.Name))
		}
		switch r.Mode {
		case "", "runner", "checks":
		default:
			p = append(p, fmt.Sprintf(`repo %q: mode must be "runner" or "checks"`, r.Name))
		}
		kind := r.Forge
		if f, ok := c.Forges[r.Forge]; ok && f.Kind != "" {
			kind = f.Kind
		}
		if (kind == "gitlab" || kind == "forgejo" || kind == "gitea") && r.Mode != "checks" {
			p = append(p, fmt.Sprintf(`repo %q: %s repos run in mode = "checks" in this version`, r.Name, kind))
		}
		for _, b := range r.Branches {
			if !ValidBranch(b) {
				p = append(p, fmt.Sprintf("repo %q: %q is not a branch name", r.Name, b))
			}
		}
		if len(r.Branches) > 0 && r.Mode != "checks" {
			p = append(p, fmt.Sprintf(`repo %q: branches apply only to mode = "checks"`, r.Name))
		}
	}
	switch c.Update.Channel {
	case "stable", "dev":
	default:
		p = append(p, `update.channel must be "stable" or "dev"`)
	}
	if c.Mesh.Port < 0 || c.Mesh.Port > 65535 {
		p = append(p, "mesh.port is out of range")
	}
	return p
}

// Weekdays maps day names to time.Weekday.
var Weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseClock reads "HH:MM" into minutes after midnight.
func ParseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a 24-hour time like 09:00", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// Path is where the config lives on this OS.
func Path() string {
	if p := os.Getenv("TENTMAKER_CONFIG"); p != "" {
		return p
	}
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("ProgramData"), "Tentmaker", "config.toml")
	case "darwin":
		return "/Library/Application Support/Tentmaker/config.toml"
	default:
		return "/etc/tentmaker/config.toml"
	}
}

// Load reads path over the defaults. A missing file returns the defaults.
func Load(path string, totalMemory int64, cpus int) (Config, error) {
	c := Default(totalMemory, cpus)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	md, err := toml.Decode(string(b), &c)
	if err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		return c, fmt.Errorf("%s: unknown setting %s", path, u[0])
	}
	return c, nil
}

// Save writes c to path atomically (temp file, fsync, rename), mode 0644.
func Save(path string, c Config) error {
	var buf bytes.Buffer
	buf.WriteString("# Tentmaker device config. Docs: https://github.com/baney75/tentmaker/blob/main/docs/config.md\n")
	if err := toml.NewEncoder(&buf).Encode(c); err != nil {
		return err
	}
	return WriteAtomic(path, buf.Bytes(), 0o644)
}

// WriteAtomic writes data to path so readers see the old or the new file,
// never a partial one.
func WriteAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteAtomicIn is WriteAtomic for a file inside root: the temp file and the
// rename stay in root, so a symlink swapped into the path can't redirect
// the write. name must be a plain file name in root's top folder.
func WriteAtomicIn(root *os.Root, name string, data []byte, mode os.FileMode) error {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := ".tmp-" + name + "-" + hex.EncodeToString(rnd[:])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, name)
}

func clampSize(v, lo, hi Size) Size {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func roundGiB(s Size) Size {
	if s < GiB {
		return s
	}
	return (s / GiB) * GiB
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// ValidBranch reports whether b is a plain git branch name: no spaces,
// control characters, "..", or the characters git refuses in refs.
func ValidBranch(b string) bool {
	if b == "" || len(b) > 255 || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") ||
		strings.Contains(b, "..") || strings.Contains(b, "//") || strings.Contains(b, "@{") {
		return false
	}
	for _, r := range b {
		if r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	return true
}
