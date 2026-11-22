// Package dispatch moves work from forges to devices.
//
// A Broker (any device with forge credentials) polls forges, matches queued
// jobs to workers' requests, grants single-use runners and watches them. A
// Worker (any device with free slots) asks every broker it can reach for
// work and runs it with its executors. Both persist their state atomically
// and fail closed: when in doubt, take no new work and stop what can't be
// vouched for.
package dispatch

import (
	"crypto/rand"
	"encoding/hex"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baney75/tentmaker/internal/core"
)

// Timing the reference dispatcher settled on.
const (
	pollMin    = 5 * time.Second
	pollMax    = 30 * time.Second
	pollFactor = 1.5
	// pollFast is the poll interval of a forge whose unchanged polls are
	// free (core.CheapPoller), stretched by pollFastPerRepo for each
	// configured repo so a device with many repos stays well under
	// GitHub's secondary limit (900 requests a minute).
	pollFast        = 2 * time.Second
	pollFastPerRepo = 500 * time.Millisecond
	tickSlack       = 100 * time.Millisecond
	identifyTimeout = 30 * time.Second
	workerSilence   = 2 * time.Minute
	repoInfoTTL     = time.Hour
	repoInfoRetry   = time.Minute
	maxJobAge       = 24 * time.Hour
	monitorEvery    = 5 * time.Second
	startGrace      = 15 * time.Minute // a granted runner its worker never reports running
	forgeTimeout    = 30 * time.Second
	postGiveUp      = 24 * time.Hour

	workerTick     = time.Second
	workTimeout    = 15 * time.Second
	startTimeout   = 10 * time.Minute
	execTimeout    = 30 * time.Second
	stopTimeout    = 2 * time.Minute
	resetTimeout   = 10 * time.Minute
	resetEvery     = time.Minute
	bootBackoff    = time.Minute
	bootBackoffMax = time.Hour
	bootCloseAfter = 5
	unackedMaxAge  = 24 * time.Hour
	overrunGrace   = 10 * time.Minute
)

// CheckEvent marks a check offer: WorkOffer has no kind field, so a broker
// sets Job.Event to core.KindCheck on offers for check jobs.
const CheckEvent = core.KindCheck

// LabelTentmaker is the label every job Tentmaker serves must ask for.
const LabelTentmaker = "tentmaker"

// OSLabel is the runner label for an OS: linux, macOS, Windows.
func OSLabel(os string) string {
	switch strings.ToLower(os) {
	case "macos", "darwin", "osx":
		return "macOS"
	case "windows":
		return "Windows"
	case "linux":
		return "linux"
	}
	return os
}

// ArchLabel is the runner label for an architecture: X64, ARM64.
func ArchLabel(arch string) string {
	switch strings.ToLower(arch) {
	case "x64", "amd64", "x86_64":
		return "X64"
	case "arm64", "aarch64":
		return "ARM64"
	}
	return strings.ToUpper(arch)
}

// PlatformLabels is a worker's base labels plus the OS and arch labels of p,
// without duplicates (case-insensitive), in order.
func PlatformLabels(base []string, p core.Platform) []string {
	var out []string
	seen := map[string]bool{}
	add := func(l string) {
		k := strings.ToLower(l)
		if l == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, l)
	}
	for _, l := range base {
		add(l)
	}
	add(OSLabel(p.OS))
	add(ArchLabel(p.Arch))
	return out
}

// WorkerLabels is what every runner a device starts carries before the
// platform labels: self-hosted, tentmaker, tm-<device>, then extra.
func WorkerLabels(device string, extra []string) []string {
	return PlatformLabels(append([]string{"self-hosted", LabelTentmaker, "tm-" + deviceSlug(device)}, extra...), core.Platform{})
}

// subset reports whether every label in want is in have (case-insensitive).
func subset(want, have []string) bool {
	h := map[string]bool{}
	for _, l := range have {
		h[strings.ToLower(l)] = true
	}
	for _, l := range want {
		if !h[strings.ToLower(l)] {
			return false
		}
	}
	return true
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, want) {
			return true
		}
	}
	return false
}

// MatchJob finds the first of the worker's platforms whose labels cover the
// job's. A job must ask for "tentmaker"; GitHub gives a runner any job whose
// labels are a subset of the runner's, so that is the test.
func MatchJob(jobLabels, base []string, platforms []core.Platform) (core.Platform, []string, bool) {
	if !hasLabel(jobLabels, LabelTentmaker) {
		return core.Platform{}, nil, false
	}
	for _, p := range platforms {
		labels := PlatformLabels(base, p)
		if subset(jobLabels, labels) {
			return p, labels, true
		}
	}
	return core.Platform{}, nil, false
}

// matchCheckPlatform picks the worker platform for a check job: the same OS,
// and the same arch when the job names one, by its runner label so aliases
// match (x64, amd64 and x86_64 are one arch).
func matchCheckPlatform(want core.Platform, platforms []core.Platform) (core.Platform, bool) {
	for _, p := range platforms {
		if !strings.EqualFold(p.OS, want.OS) {
			continue
		}
		if want.Arch == "" || ArchLabel(p.Arch) == ArchLabel(want.Arch) {
			return p, true
		}
	}
	return core.Platform{}, false
}

// Denied reports whether repo matches a deny pattern (path.Match syntax,
// case-insensitive). A malformed pattern denies everything: fail closed.
func Denied(repo string, patterns []string) bool {
	r := strings.ToLower(repo)
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if p == r {
			return true
		}
		ok, err := path.Match(p, r)
		if err != nil || ok {
			return true
		}
	}
	return false
}

// deviceSlug turns a device name into something a runner name can carry.
func deviceSlug(device string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(device) {
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		if ok {
			b.WriteRune(c)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(b.String(), "-")
	if len(s) > 48 {
		s = strings.TrimRight(s[:48], "-")
	}
	if s == "" {
		s = "device"
	}
	return s
}

// RunnerName is "tm-<device>-<8 hex>", always valid per core.NamePattern.
func RunnerName(device string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; a clock-based
		// suffix still keeps the name unique enough to retry on conflict.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (8 * i))
		}
	}
	return "tm-" + deviceSlug(device) + "-" + hex.EncodeToString(b[:])
}

// CheckContext is the commit status context of a check job.
func CheckContext(job core.CheckJob) string {
	name := job.Name
	if name == "" {
		name = job.ID
	}
	return "tentmaker/" + name
}

func isCheckOffer(o core.WorkOffer) bool { return o.Job.Event == CheckEvent }

func ownerOf(repo string) string {
	if i := strings.Index(repo, "/"); i > 0 {
		return repo[:i]
	}
	return repo
}

// truncate cuts s to at most n bytes without splitting a UTF-8 character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
