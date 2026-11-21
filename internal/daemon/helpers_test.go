package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	fakeexec "github.com/baney75/tentmaker/internal/executor/fake"
	fakeforge "github.com/baney75/tentmaker/internal/forge/fake"
	"github.com/baney75/tentmaker/internal/guards"
	"github.com/baney75/tentmaker/internal/service"
	"github.com/baney75/tentmaker/internal/update"
)

func TestDefaultPaths(t *testing.T) {
	l := DefaultPaths("linux", nil)
	if l.Config != "/etc/tentmaker/config.toml" || l.State != "/var/lib/tentmaker" || l.Cache != "/var/cache/tentmaker" || l.Log != "" || l.Secrets != "/etc/tentmaker/secrets" {
		t.Errorf("linux %+v", l)
	}
	m := DefaultPaths("darwin", nil)
	if m.State != "/Library/Application Support/Tentmaker/state" || m.Cache != "/Library/Caches/Tentmaker" || m.Log != "/Library/Logs/Tentmaker/tentmaker.log" {
		t.Errorf("darwin %+v", m)
	}
	w := DefaultPaths("windows", func(k string) string { return `D:\PD` })
	if w.Config != `D:\PD\Tentmaker\config.toml` || w.Log != `D:\PD\Tentmaker\logs\tentmaker.log` || w.State != `D:\PD\Tentmaker\state` {
		t.Errorf("windows %+v", w)
	}
	cfg := Resolve(config.Config{}, l)
	if cfg.StateDir != l.State || cfg.CacheDir != l.Cache {
		t.Errorf("resolve %+v", cfg)
	}
	cfg = Resolve(config.Config{StateDir: "/s", CacheDir: "/c"}, l)
	if cfg.StateDir != "/s" || cfg.CacheDir != "/c" {
		t.Errorf("resolve kept %+v", cfg)
	}
}

func TestSanitizeDevice(t *testing.T) {
	for in, want := range map[string]string{
		"Donovans-MacBook-Pro.local": "donovans-macbook-pro",
		"GIGA_PC":                    "giga-pc",
		"--":                         "device",
		"box":                        "box",
		strings.Repeat("a", 60):      strings.Repeat("a", 40),
	} {
		if got := SanitizeDevice(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
		if !core.NamePattern.MatchString("tm-" + SanitizeDevice(in) + "-abcdef12") {
			t.Errorf("%q makes an invalid runner name", in)
		}
	}
}

func TestSecretName(t *testing.T) {
	for _, c := range []struct{ kind, url, want string }{
		{"github", "", "github-token"},
		{"github", "https://api.github.com", "github-token"},
		{"github", "https://ghe.corp.example/api/v3", "github-ghe.corp.example-token"},
		{"forgejo", "", "forgejo-token"},
		{"forgejo", "https://codeberg.org", "forgejo-codeberg.org-token"},
		{"forgejo", "http://git.lan:3000", "forgejo-git.lan-3000-token"},
		{"gitlab", "https://gitlab.com", "gitlab-token"},
		{"", "", "github-token"},
	} {
		if got := SecretName(c.kind, c.url); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.kind, c.url, got, c.want)
		}
	}
}

func TestWhy(t *testing.T) {
	d := core.Decision{Profile: "boost"}
	if got := Why(d, &core.PowerState{Known: true, OnAC: true}, 14*time.Minute, true); got != "boost: plugged in, idle 14m" {
		t.Errorf("got %q", got)
	}
	d = core.Decision{Profile: "off", Reasons: []string{"on battery at 31%, below 40%"}}
	if got := Why(d, &core.PowerState{Known: true, HasBattery: true, BatteryPercent: 31}, 0, false); got != "off: on battery at 31%, below 40%" {
		t.Errorf("got %q", got)
	}
	if got := Why(core.Decision{}, nil, 0, false); got != "everyday" {
		t.Errorf("got %q", got)
	}
	for d, want := range map[time.Duration]string{30 * time.Second: "30s", 3*time.Hour + 12*time.Minute: "3h12m", 50 * time.Hour: "2d2h", 2 * time.Hour: "2h"} {
		if got := ShortDuration(d); got != want {
			t.Errorf("%v: %q", d, got)
		}
	}
}

func TestRotatingWriter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logs", "tm.log")
	w, err := OpenRotating(p, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 39) + "\n"
	for i := 0; i < 20; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	for _, name := range []string{p, p + ".1", p + ".2"} {
		st, err := os.Stat(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st.Size() > 100 {
			t.Errorf("%s is %d bytes", name, st.Size())
		}
	}
	if _, err := os.Stat(p + ".3"); !os.IsNotExist(err) {
		t.Error("kept more than 2 old files")
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("write after close")
	}
}

func TestNewLogger(t *testing.T) {
	var buf strings.Builder
	log, c, err := NewLogger("", &buf)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello")
	c.Close()
	if !strings.Contains(buf.String(), "hello") {
		t.Error("stderr logger wrote nothing")
	}
	p := filepath.Join(t.TempDir(), "a", "tm.log")
	log, c, err = NewLogger(p, &buf)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("to file")
	c.Close()
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "to file") {
		t.Error("file logger wrote nothing")
	}
}

func TestBinary(t *testing.T) {
	cache := t.TempDir()
	exe := filepath.Join(cache, "self")
	os.WriteFile(exe, []byte("me"), 0o755)
	var downloads []string
	bin := NewBinary(BinaryOptions{
		Version: "1.4.0", CacheDir: cache, GOOS: "darwin", GOARCH: "arm64",
		Executable: func() (string, error) { return exe, nil },
		Download: func(ctx context.Context, goos, goarch string, rel update.Release, dir string) (string, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("download without a deadline")
			}
			downloads = append(downloads, goos+"/"+goarch+"@"+rel.Tag+" "+rel.Assets[update.ArchiveName(goos, goarch)])
			p := filepath.Join(dir, "tentmaker")
			return p, os.WriteFile(p, []byte("linux build"), 0o755)
		},
	})
	got, err := bin(core.Platform{OS: "macos", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(exe); got != want {
		t.Errorf("host platform gave %s", got)
	}
	for i := 0; i < 2; i++ {
		got, err = bin(core.Platform{OS: "linux", Arch: "arm64"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if got != filepath.Join(cache, "bin", "tentmaker_linux_arm64-1.4.0") {
		t.Errorf("guest binary at %s", got)
	}
	if len(downloads) != 1 || downloads[0] != "linux/arm64@v1.4.0 "+ReleaseBase+"/v1.4.0/tentmaker_linux_arm64.tar.gz" {
		t.Errorf("downloads %v", downloads)
	}
	dev := NewBinary(BinaryOptions{Version: "1.5.0-dev.202611210400.abc1234", CacheDir: t.TempDir(), GOOS: "darwin", GOARCH: "arm64",
		Download: func(ctx context.Context, goos, goarch string, rel update.Release, dir string) (string, error) {
			if rel.Tag != "dev" {
				t.Errorf("dev build fetched %s", rel.Tag)
			}
			return "", errors.New("offline")
		}})
	if _, err := dev(core.Platform{OS: "linux", Arch: "arm64"}); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Errorf("dev error %v", err)
	}
	handCache := t.TempDir()
	hand := NewBinary(BinaryOptions{Version: "0.0.0-dev", CacheDir: handCache, GOOS: "darwin", GOARCH: "arm64"})
	if _, err := hand(core.Platform{OS: "linux", Arch: "arm64"}); err == nil || !strings.Contains(err.Error(), "hand build") {
		t.Errorf("hand build error %v", err)
	}
	// A hand build uses the binary put where the error says.
	handBin := filepath.Join(handCache, "bin", "tentmaker_linux_arm64")
	os.MkdirAll(filepath.Dir(handBin), 0o755)
	os.WriteFile(handBin, []byte("hand"), 0o755)
	if got, err := hand(core.Platform{OS: "linux", Arch: "arm64"}); err != nil || got != handBin {
		t.Errorf("hand build with a binary in place: %s %v", got, err)
	}
}

// After an update the guest binary is fetched again for the new version,
// not served from the old version's cache, and the old copy goes (review
// r1 partB first-F2, sol-F3).
func TestBinaryCacheFollowsTheVersion(t *testing.T) {
	cache := t.TempDir()
	var tags []string
	newBin := func(version string) func(core.Platform) (string, error) {
		return NewBinary(BinaryOptions{Version: version, CacheDir: cache, GOOS: "darwin", GOARCH: "arm64",
			Download: func(ctx context.Context, goos, goarch string, rel update.Release, dir string) (string, error) {
				tags = append(tags, rel.Tag)
				p := filepath.Join(dir, "tentmaker")
				return p, os.WriteFile(p, []byte("build "+rel.Version), 0o755)
			}})
	}
	linux := core.Platform{OS: "linux", Arch: "arm64"}
	old, err := newBin("1.4.0")(linux)
	if err != nil {
		t.Fatal(err)
	}
	got, err := newBin("1.5.0")(linux)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(got); string(b) != "build 1.5.0" {
		t.Fatalf("after the update check jobs get %q", b)
	}
	if !slices.Equal(tags, []string{"v1.4.0", "v1.5.0"}) {
		t.Fatalf("downloads %v", tags)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the old version's copy is still there: %v", err)
	}
}

func TestBuildForges(t *testing.T) {
	cfg := config.Default(16<<30, 8)
	cfg.Repos = []config.Repo{{Name: "a/b"}, {Name: "g/h", Forge: "gitlab", Mode: "checks"}, {Name: "c/d", Forge: "forgejo"}}
	cfg.Forges["forgejo"] = config.Forge{Kind: "forgejo", URL: "https://codeberg.org"}
	store := &memStore{m: map[string][]byte{"github-token": []byte("t1"), "forgejo-codeberg.org-token": []byte("t2")}}
	var asked []string
	f := BuildForges(cfg, store, func(name string, fc config.Forge, token func(context.Context) (string, error), _ *slog.Logger) (core.Forge, core.StatusForge, error) {
		tok, _ := token(context.Background())
		asked = append(asked, name+"="+tok)
		ff := fakeforge.New(name, nil)
		return ff, ff, nil
	}, nil)
	if strings.Join(f.Tokens, ",") != "forgejo,github" || len(f.Problems) != 0 {
		t.Errorf("tokens %v problems %v", f.Tokens, f.Problems)
	}
	if strings.Join(asked, ",") != "forgejo=t2,github=t1" {
		t.Errorf("asked %v", asked)
	}
	// The real factory refuses a forgejo forge without a URL.
	if _, _, err := DefaultForge("forgejo", config.Forge{Kind: "forgejo"}, nil, nil); err == nil {
		t.Error("forgejo without url accepted")
	}
	if fg, sf, err := DefaultForge("gitlab", config.Forge{Kind: "gitlab", URL: "https://git.lab"}, nil, nil); err != nil || fg != nil || sf == nil {
		t.Errorf("gitlab: %v %v %v", fg, sf, err)
	}
}

func TestDoctorFindings(t *testing.T) {
	cfg := Resolve(config.Default(16<<30, 8), Paths{State: t.TempDir(), Cache: t.TempDir()})
	cfg.Repos = []config.Repo{{Name: "o/pub"}, {Name: "o/priv"}}
	ff := fakeforge.New("github", nil)
	ff.SetRepo(core.RepoInfo{Repo: "o/pub", Public: true, ForkPolicy: "off"})
	ff.SetRepo(core.RepoInfo{Repo: "o/priv", Problems: []string{"token lacks administration: write"}})
	ex := fakeexec.New("fake", 0, linuxX64)
	rep := Doctor(context.Background(), DoctorInput{
		Version: "1.0.0", ConfigPath: "/etc/tentmaker/config.toml", ConfigExists: true, Config: cfg,
		Executors: []core.Executor{ex},
		Forges:    Forges{Forges: map[string]core.Forge{"github": ff}, Tokens: []string{"github"}},
		Tailscale: &fakeTS{},
		Service:   func(context.Context) (service.State, error) { return service.State{Installed: true}, nil },
		Probe:     guards.Fake{Disk: 1 << 30},
		Update: func(ctx context.Context, ch, cur string) (update.Release, bool, error) {
			return update.Release{Version: "1.1.0"}, true, nil
		},
	})
	if rep.OK {
		t.Error("report OK with problems")
	}
	text := fmt.Sprint(rep.Checks)
	for _, want := range []string{"o/pub is public and allow_public is off", "token lacks administration", "not running", "tailscale is not up", "below min_free_disk", "1.1.0 is available", "fake is ready"} {
		if !strings.Contains(text, want) {
			t.Errorf("doctor lacks %q:\n%s", want, text)
		}
	}
	for _, f := range rep.Checks {
		if f.Status != OK && f.Fix == "" {
			t.Errorf("finding without a fix: %+v", f)
		}
	}
	if len(rep.ProblemList()) != rep.Problems+rep.Warnings {
		t.Error("problem list count")
	}
	// An executor's notes show as warnings, not problems.
	noted := Doctor(context.Background(), DoctorInput{
		Version: "1.0.0", ConfigPath: "/etc/tentmaker/config.toml", ConfigExists: true, Config: cfg,
		Executors: []core.Executor{notedExec{ex}},
	})
	var note *Finding
	for i, f := range noted.Checks {
		if f.Check == "macos guests" {
			note = &noted.Checks[i]
		}
	}
	if note == nil || note.Status != Warn || !strings.HasPrefix(note.What, "macOS guests are off") || note.Fix == "" {
		t.Errorf("executor note: %+v", note)
	}
	rep = Doctor(context.Background(), DoctorInput{ConfigPath: "/x", Config: cfg})
	if rep.OK || !strings.Contains(fmt.Sprint(rep.Checks), "not set up yet") {
		t.Errorf("not set up: %+v", rep)
	}
}

func TestChannel(t *testing.T) {
	if Channel("1.2.3") != "stable" || Channel("1.3.0-dev.202611210400.abc1234") != "dev" || Channel("0.0.0-dev") != "dev" {
		t.Error("channel")
	}
	if HostPlatform("darwin", "amd64") != (core.Platform{OS: "macos", Arch: "x64"}) {
		t.Error("host platform")
	}
	if goos, goarch := GoPlatform(core.Platform{OS: "macos", Arch: "x64"}); goos != "darwin" || goarch != "amd64" {
		t.Error("go platform")
	}
}

// notedExec is an executor with a doctor note.
type notedExec struct{ core.Executor }

func (notedExec) Notes(context.Context) []core.Problem {
	return []core.Problem{{Check: "macos guests", What: "macOS guests are off: the macOS image needs 130 GiB, 60 GiB free; run sudo tentmaker setup --macos-guests when you have room", Fix: "sudo tentmaker setup --macos-guests"}}
}
