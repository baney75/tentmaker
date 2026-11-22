package dispatch

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/baney75/tentmaker/internal/core"
)

func TestWorkerAndPlatformLabels(t *testing.T) {
	base := WorkerLabels("Alpha Box.local", []string{"gpu", "Tentmaker"})
	if got := strings.Join(base, ","); got != "self-hosted,tentmaker,tm-alpha-box-local,gpu" {
		t.Fatalf("worker labels = %s", got)
	}
	cases := map[core.Platform]string{
		linuxX64:                     "linux,X64",
		linuxARM:                     "linux,ARM64",
		macosARM:                     "macOS,ARM64",
		{OS: "windows", Arch: "x64"}: "Windows,X64",
	}
	for p, want := range cases {
		got := PlatformLabels(base, p)
		if tail := strings.Join(got[len(base):], ","); tail != want {
			t.Errorf("%s labels end %s, want %s", p, tail, want)
		}
	}
}

func TestMatchJob(t *testing.T) {
	base := WorkerLabels("alpha", nil)
	both := []core.Platform{linuxX64, macosARM}
	cases := []struct {
		labels []string
		ok     bool
		plat   core.Platform
	}{
		{[]string{"self-hosted", "tentmaker", "linux"}, true, linuxX64},
		{[]string{"SELF-HOSTED", "Tentmaker", "LINUX", "x64"}, true, linuxX64},
		{[]string{"self-hosted", "tentmaker", "macOS"}, true, macosARM},
		{[]string{"tentmaker", "ARM64"}, true, macosARM},
		{[]string{"self-hosted", "tentmaker"}, true, linuxX64},
		{[]string{"self-hosted", "linux"}, false, core.Platform{}},            // not ours
		{[]string{"self-hosted", "tentmaker", "gpu"}, false, core.Platform{}}, // a label we lack
		{[]string{"self-hosted", "tentmaker", "linux", "ARM64"}, false, core.Platform{}},
		{[]string{"tentmaker", "tm-alpha"}, true, linuxX64},
		{[]string{"tentmaker", "tm-beta"}, false, core.Platform{}},
	}
	for _, c := range cases {
		p, labels, ok := MatchJob(c.labels, base, both)
		if ok != c.ok || p != c.plat {
			t.Errorf("%v: got %v %v, want %v %v", c.labels, p, ok, c.plat, c.ok)
		}
		if ok && !subset(c.labels, labels) {
			t.Errorf("%v: grant labels %v do not cover the job", c.labels, labels)
		}
	}
}

func TestRunnerNameAlwaysValid(t *testing.T) {
	for _, d := range []string{"alpha", "", "Ünïcode Host", "--x--", strings.Repeat("long-name", 20), "a"} {
		n := RunnerName(d)
		if !core.NamePattern.MatchString(n) || !strings.HasPrefix(n, "tm-") {
			t.Errorf("RunnerName(%q) = %q", d, n)
		}
	}
	if RunnerName("alpha") == RunnerName("alpha") {
		t.Errorf("two names collided")
	}
}

func TestDenied(t *testing.T) {
	pats := []string{"evil/*", "Baney75/Secret"}
	for repo, want := range map[string]bool{
		"evil/thing":     true,
		"EVIL/x":         true,
		"baney75/secret": true,
		"baney75/app":    false,
		"notevil/x":      false,
	} {
		if got := Denied(repo, pats); got != want {
			t.Errorf("Denied(%s) = %v", repo, got)
		}
	}
	if !Denied("baney75/app", []string{"["}) {
		t.Errorf("a malformed pattern must deny (fail closed)")
	}
}

func TestCheckContextAndTruncate(t *testing.T) {
	if got := CheckContext(core.CheckJob{ID: "build", Name: "Build and test"}); got != "tentmaker/Build and test" {
		t.Fatal(got)
	}
	if got := CheckContext(core.CheckJob{ID: "build"}); got != "tentmaker/build" {
		t.Fatal(got)
	}
	s := truncate(strings.Repeat("é", 100), 141)
	if !utf8.ValidString(s) || len(s) > 141 {
		t.Fatalf("truncate broke UTF-8: %q", s)
	}
}
