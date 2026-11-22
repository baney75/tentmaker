package dispatch

import (
	"testing"

	"github.com/baney75/tentmaker/internal/checks"
	"github.com/baney75/tentmaker/internal/core"
)

// A hosted image label names only the OS, so ubuntu-latest runs on any
// linux worker. Before, plan pinned it to x64 and the match compared arch
// names without aliases, so it never matched an arm64 worker (every Apple
// silicon Mac's linux VM), or even an "amd64" one, with no status and no log
// (review r1 partA second-F4).
func TestHostedLabelsMatchAnyArch(t *testing.T) {
	src := "on: push\njobs:\n" +
		"  any:\n    runs-on: ubuntu-latest\n    steps:\n      - run: make\n" +
		"  arm:\n    runs-on: ubuntu-24.04-arm\n    steps:\n      - run: make\n" +
		"  x64:\n    runs-on: [self-hosted, linux, x64]\n    steps:\n      - run: make\n" +
		"  win:\n    runs-on: windows-latest\n    steps:\n      - run: make\n"
	jobs, _, err := checks.Plan(".github/workflows/ci.yml", []byte(src), "github")
	if err != nil || len(jobs) != 4 {
		t.Fatalf("plan: %d jobs, %v", len(jobs), err)
	}
	byName := map[string]core.Platform{}
	for _, j := range jobs {
		byName[j.Name] = j.Platform
	}
	amd64 := core.Platform{OS: "linux", Arch: "amd64"}
	arm64 := core.Platform{OS: "linux", Arch: "arm64"}
	x64 := core.Platform{OS: "linux", Arch: "x64"}
	winARM := core.Platform{OS: "windows", Arch: "arm64"}
	for _, c := range []struct {
		job    string
		worker core.Platform
		want   bool
	}{
		{"any", amd64, true},
		{"any", arm64, true},
		{"any", x64, true},
		{"arm", arm64, true},
		{"arm", amd64, false},
		{"arm", x64, false},
		{"x64", amd64, true},
		{"x64", x64, true},
		{"x64", arm64, false},
		{"win", winARM, true},
		{"any", winARM, false},
	} {
		_, ok := matchCheckPlatform(byName[c.job], []core.Platform{c.worker})
		if ok != c.want {
			t.Errorf("%s (%+v) on %s: match = %v, want %v", c.job, byName[c.job], c.worker, ok, c.want)
		}
	}
}
