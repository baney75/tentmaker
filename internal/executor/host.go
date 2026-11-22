// Package executor picks the sandboxes this machine can run.
package executor

import (
	"log/slog"
	"runtime"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/executor/darwin"
	"github.com/baney75/tentmaker/internal/executor/linux"
	"github.com/baney75/tentmaker/internal/executor/windows"
)

// ForHost returns the executors for this OS and CPU, built from cfg's state
// and cache directories (empty picks each executor's OS default). binary
// finds the tentmaker build for a guest platform, for check jobs. It does not
// touch the host; run each executor's Preflight before use. A machine with
// no usable sandbox gets a Problem instead.
func ForHost(cfg config.Config, binary func(core.Platform) (string, error), log *slog.Logger) ([]core.Executor, []core.Problem) {
	return forHost(runtime.GOOS, runtime.GOARCH, cfg, binary, log)
}

func forHost(goos, goarch string, cfg config.Config, binary func(core.Platform) (string, error), log *slog.Logger) ([]core.Executor, []core.Problem) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var e core.Executor
	switch goos {
	case "linux":
		e = linux.New(linux.Options{StateDir: cfg.StateDir, CacheDir: cfg.CacheDir, Log: log, Binary: binary, Arch: ghArch(goarch), AllowLAN: cfg.Guards.AllowLAN, CacheMax: repoCacheMax(cfg)})
	case "darwin":
		e = darwin.New(darwin.Options{StateDir: cfg.StateDir, CacheDir: cfg.CacheDir, Log: log, Binary: binary, HostArch: goarch})
	case "windows":
		e = windows.New(windows.Options{StateDir: cfg.StateDir, CacheDir: cfg.CacheDir, Log: log, Binary: binary, Arch: ghArch(goarch), AllowLAN: cfg.Guards.AllowLAN})
	default:
		return nil, []core.Problem{{
			Check: "executor",
			What:  "Tentmaker has no sandbox for " + goos,
			Fix:   "run Tentmaker on Linux, macOS or Windows",
		}}
	}
	// An Apple silicon Mac keeps its executor even with no guest image
	// installed (no disk room yet): its Preflight says what to free.
	if len(e.Platforms()) == 0 && !(goos == "darwin" && goarch == "arm64") {
		fix := "use an x64 or arm64 machine"
		if goos == "darwin" {
			fix = "use a Mac with Apple silicon; Tart needs it"
		}
		return nil, []core.Problem{{
			Check: "executor",
			What:  e.Name() + " can't run on " + goos + "/" + goarch,
			Fix:   fix,
		}}
	}
	return []core.Executor{e}, nil
}

// ghArch is GitHub's spelling of a Go GOARCH.
func ghArch(goarch string) string {
	if goarch == "amd64" {
		return "x64"
	}
	return goarch
}

// repoCacheMax maps cache_max to linux.Options.CacheMax: 0 in the config
// turns the repo caches off.
func repoCacheMax(cfg config.Config) int64 {
	if cfg.CacheMax <= 0 {
		return -1
	}
	return int64(cfg.CacheMax)
}
