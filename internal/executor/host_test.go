package executor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

func TestForHost(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir(), CacheDir: t.TempDir()}
	bin := func(core.Platform) (string, error) { return "/nonexistent", nil }
	for _, c := range []struct {
		goos, goarch, name string
		platforms          []core.Platform
	}{
		{"linux", "amd64", "linux-podman", []core.Platform{{OS: "linux", Arch: "x64"}}},
		{"linux", "arm64", "linux-podman", []core.Platform{{OS: "linux", Arch: "arm64"}}},
		{"darwin", "arm64", "macos-tart", []core.Platform{{OS: "macos", Arch: "arm64"}, {OS: "linux", Arch: "arm64"}}},
		{"windows", "amd64", "windows-job", []core.Platform{{OS: "windows", Arch: "x64"}}},
		{"windows", "arm64", "windows-job", []core.Platform{{OS: "windows", Arch: "arm64"}}},
	} {
		ex, probs := forHost(c.goos, c.goarch, cfg, bin, nil)
		if len(probs) != 0 || len(ex) != 1 {
			t.Fatalf("%s/%s: %d executors, problems %v", c.goos, c.goarch, len(ex), probs)
		}
		if ex[0].Name() != c.name || !slices.Equal(ex[0].Platforms(), c.platforms) {
			t.Fatalf("%s/%s: %s %v", c.goos, c.goarch, ex[0].Name(), ex[0].Platforms())
		}
	}
	for _, c := range []struct{ goos, goarch, want string }{
		{"freebsd", "amd64", "no sandbox for freebsd"},
		{"darwin", "amd64", "Apple silicon"},
		{"linux", "386", "can't run on linux/386"},
		{"windows", "386", "can't run on windows/386"},
	} {
		ex, probs := forHost(c.goos, c.goarch, cfg, bin, nil)
		if len(ex) != 0 || len(probs) != 1 || !strings.Contains(probs[0].What+" "+probs[0].Fix, c.want) {
			t.Fatalf("%s/%s: %d executors, problems %v", c.goos, c.goarch, len(ex), probs)
		}
	}
	// An Apple silicon Mac whose setup found no room for any image keeps
	// its executor, so doctor can say what to free.
	mac := config.Config{StateDir: t.TempDir(), CacheDir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(mac.StateDir, "macos-tart"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mac.StateDir, "macos-tart", "guests.json"), []byte(`{"platforms":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if ex, probs := forHost("darwin", "arm64", mac, bin, nil); len(ex) != 1 || len(probs) != 0 || len(ex[0].Platforms()) != 0 {
		t.Fatalf("mac without images: %d executors, problems %v", len(ex), probs)
	}
	// The real entry point runs on this machine without touching it.
	ForHost(cfg, bin, nil)
}
