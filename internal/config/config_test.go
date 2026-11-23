package config

import (
	"math"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	good := map[string]Size{
		"4GiB": 4 * GiB, "4gib": 4 * GiB, "4G": 4 * GiB, "4GB": 4 * GiB, "4 GiB": 4 * GiB,
		"512MiB": 512 * MiB, "512M": 512 * MiB, "1.5GiB": Size(1.5 * float64(GiB)), "0": 0, "2TiB": 2 << 40,
	}
	for in, want := range good {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	// Fuzz findings: sizes past int64 used to wrap to a negative or
	// arch-dependent value.
	for _, in := range []string{"", "4", "abc", "-1GiB", "4XB", "4GiBs", "99999999999999999999T", "8193PiB", "1e9T", "1048577TiB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) accepted", in)
		}
	}
}

func TestSizeRoundTrip(t *testing.T) {
	// 1023 bytes and 1.5 KiB used to save as "0KiB" and "1KiB".
	for _, s := range []Size{0, 4 * GiB, 512 * MiB, 3 * KiB, 1023, 1536, MaxSize} {
		got, err := ParseSize(s.String())
		if err != nil || got != s {
			t.Errorf("round trip %v: %v %v", s, got, err)
		}
	}
}

func TestDefaultValidates(t *testing.T) {
	for _, mem := range []int64{4 * int64(GiB), 8 * int64(GiB), 16 * int64(GiB), 64 * int64(GiB), 256 * int64(GiB)} {
		c := Default(mem, 8)
		if err := c.Validate(); err != nil {
			t.Errorf("Default(%d GiB) invalid: %v", mem/int64(GiB), err)
		}
	}
}

func TestChecksOnlyForges(t *testing.T) {
	c := Default(16*int64(GiB), 8)
	c.Forges["codeberg"] = Forge{Kind: "forgejo", URL: "https://codeberg.org"}
	c.Repos = []Repo{{Forge: "codeberg", Name: "a/b", Mode: "runner"}}
	if err := c.Validate(); err == nil {
		t.Fatal("forgejo repo in runner mode accepted")
	}
	c.Repos[0].Mode = "checks"
	if err := c.Validate(); err != nil {
		t.Fatalf("forgejo repo in checks mode refused: %v", err)
	}
	c.Repos = []Repo{{Forge: "github", Name: "a/b"}}
	if err := c.Validate(); err != nil {
		t.Fatalf("github repo refused: %v", err)
	}
}

func TestRepoBranches(t *testing.T) {
	for _, b := range []string{"main", "release/1.2", "e2e", "feat_x-y"} {
		if !ValidBranch(b) {
			t.Errorf("%q should be a branch name", b)
		}
	}
	for _, b := range []string{"", "-x", "a..b", "a b", "a:b", "x.lock", "/x", "x/", "a~1", "a\\b", "a@{1}"} {
		if ValidBranch(b) {
			t.Errorf("%q should not be a branch name", b)
		}
	}
	c := Default(int64(16*GiB), 8)
	c.Repos = []Repo{{Name: "o/r", Mode: "checks", Branches: []string{"e2e"}}}
	if p := c.Validate(); len(p) != 0 {
		t.Errorf("checks repo with a branch: %v", p)
	}
	c.Repos = []Repo{{Name: "o/r", Branches: []string{"e2e"}}}
	if p := c.Validate(); len(p) != 1 || !strings.Contains(p[0], "checks") {
		t.Errorf("runner repo with a branch: %v", p)
	}
}

func TestValidateCPUs(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		c := Default(16*int64(GiB), 8)
		c.Job.CPUs = bad
		c.Budget.Boost.CPUs = bad
		p := c.Validate()
		if !strings.Contains(strings.Join(p, "\n"), "job.cpus must be a finite number") ||
			!strings.Contains(strings.Join(p, "\n"), "budget.boost.cpus must be a finite number") {
			t.Errorf("cpus %v accepted: %q", bad, p)
		}
	}
}

func TestValidateViewersAndMetricsListen(t *testing.T) {
	c := Default(16*int64(GiB), 8)
	c.Mesh.Viewers = []string{"tag:monitoring", "grafana-box", "nKx7abc.ts.net"}
	c.MetricsListen = "127.0.0.1:9437"
	if p := c.Validate(); len(p) != 0 {
		t.Fatalf("good viewers and listener refused: %q", p)
	}
	c.MetricsListen = "[::1]:9437"
	if p := c.Validate(); len(p) != 0 {
		t.Fatalf("::1 refused: %q", p)
	}
	for _, bad := range []string{"0.0.0.0:9437", "100.64.0.1:9437", "localhost:9437", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:70000"} {
		c.MetricsListen = bad
		if p := c.Validate(); !strings.Contains(strings.Join(p, "\n"), "metrics_listen") {
			t.Errorf("metrics_listen %q accepted", bad)
		}
	}
	c.MetricsListen = ""
	for _, bad := range []string{"tag:", "*", "tag:a b", "bob@example.com", ""} {
		c.Mesh.Viewers = []string{bad}
		if p := c.Validate(); !strings.Contains(strings.Join(p, "\n"), "mesh.viewers") {
			t.Errorf("viewer %q accepted", bad)
		}
	}
}

func TestValidateLendPublic(t *testing.T) {
	c := Default(16*int64(GiB), 8)
	c.Repos = []Repo{{Name: "a/pub", AllowPublic: true, LendPublic: true}}
	if p := c.Validate(); len(p) != 0 {
		t.Fatalf("lend_public with allow_public refused: %q", p)
	}
	for _, r := range []Repo{{Name: "a/pub", LendPublic: true}, {Name: "a/pub", AllowPublic: true, LendPublic: true, Mode: "checks"}} {
		c.Repos = []Repo{r}
		if p := c.Validate(); !strings.Contains(strings.Join(p, "\n"), "lend_public") {
			t.Errorf("%+v accepted", r)
		}
	}
}
