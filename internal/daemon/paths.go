// Package daemon composes Tentmaker's packages into the long-running device
// service: executors, the budget, forges from the secret store, a local
// broker, the mesh node, the worker, the loopback control API and the
// auto-update loop. Reload rebuilds only what changed and never stops a
// running job; a component that can't be built becomes a doctor problem and
// the rest keeps running.
package daemon

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// Paths are the per-OS locations from docs/CLI.md "Files".
type Paths struct {
	Config  string `json:"config"`
	Secrets string `json:"secrets"`
	State   string `json:"state"`
	Cache   string `json:"cache"`
	Log     string `json:"log,omitempty"` // empty: stderr, which journald keeps (Linux)
}

// DefaultPaths returns the Files table for goos. getenv reads ProgramData
// on Windows.
func DefaultPaths(goos string, getenv func(string) string) Paths {
	switch goos {
	case "windows":
		pd := ""
		if getenv != nil {
			pd = getenv("ProgramData")
		}
		if pd == "" {
			pd = `C:\ProgramData`
		}
		base := pd + `\Tentmaker`
		return Paths{
			Config:  base + `\config.toml`,
			Secrets: base + `\secrets`,
			State:   base + `\state`,
			Cache:   base + `\cache`,
			Log:     base + `\logs\tentmaker.log`,
		}
	case "darwin":
		base := "/Library/Application Support/Tentmaker"
		return Paths{
			Config:  base + "/config.toml",
			Secrets: base + "/secrets",
			State:   base + "/state",
			Cache:   "/Library/Caches/Tentmaker",
			Log:     "/Library/Logs/Tentmaker/tentmaker.log",
		}
	default:
		return Paths{
			Config:  "/etc/tentmaker/config.toml",
			Secrets: "/etc/tentmaker/secrets",
			State:   "/var/lib/tentmaker",
			Cache:   "/var/cache/tentmaker",
		}
	}
}

// Resolve fills the state and cache directories from p when the config
// leaves them empty. The guards fail closed on an empty state dir, so every
// component gets a resolved config.
func Resolve(cfg config.Config, p Paths) config.Config {
	if cfg.StateDir == "" {
		cfg.StateDir = p.State
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = p.Cache
	}
	return cfg
}

var notDevice = regexp.MustCompile(`[^a-z0-9-]+`)

// SanitizeDevice lowercases name and keeps [a-z0-9-], so it fits runner
// names and labels. An empty result is "device".
func SanitizeDevice(name string) string {
	name = strings.ToLower(name)
	if i := strings.IndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	name = strings.Trim(notDevice.ReplaceAllString(name, "-"), "-")
	if len(name) > 40 {
		name = strings.Trim(name[:40], "-")
	}
	if name == "" {
		return "device"
	}
	return name
}

// DeviceName is the config's device, or the sanitized hostname.
func DeviceName(cfg config.Config, hostname func() (string, error)) string {
	if cfg.Device != "" {
		return cfg.Device
	}
	if hostname != nil {
		if h, err := hostname(); err == nil {
			return SanitizeDevice(h)
		}
	}
	return "device"
}

// HostPlatform is goos/goarch in core.Platform spelling.
func HostPlatform(goos, goarch string) core.Platform {
	os := goos
	if os == "darwin" {
		os = "macos"
	}
	arch := goarch
	if arch == "amd64" {
		arch = "x64"
	}
	return core.Platform{OS: os, Arch: arch}
}

// GoPlatform is p in Go's GOOS/GOARCH spelling.
func GoPlatform(p core.Platform) (goos, goarch string) {
	goos, goarch = p.OS, p.Arch
	if goos == "macos" {
		goos = "darwin"
	}
	if goarch == "x64" {
		goarch = "amd64"
	}
	return goos, goarch
}

// Channel is the update channel a version came from: dev builds carry
// "-dev." in their version.
func Channel(version string) string {
	if strings.Contains(version, "-dev") {
		return "dev"
	}
	return "stable"
}

func join(dir, name string) string { return filepath.Join(dir, name) }
