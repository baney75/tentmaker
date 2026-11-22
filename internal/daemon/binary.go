package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/update"
)

// ReleaseBase is where release assets download from.
const ReleaseBase = "https://github.com/baney75/tentmaker/releases/download"

// BinaryDownloadTimeout bounds fetching a guest binary.
const BinaryDownloadTimeout = 10 * time.Minute

// BinaryOptions configures NewBinary.
type BinaryOptions struct {
	Version    string
	CacheDir   string
	Executable func() (string, error) // default os.Executable
	GOOS       string                 // default runtime.GOOS
	GOARCH     string                 // default runtime.GOARCH
	// Download fetches rel's archive for goos/goarch into dir and returns
	// the verified binary. Default: internal/update for that platform.
	Download    func(ctx context.Context, goos, goarch string, rel update.Release, dir string) (string, error)
	ReleaseBase string // default ReleaseBase
	Log         *slog.Logger
}

// NewBinary returns Binary(platform) from docs/CLI.md: this executable when
// the platform matches the host, otherwise
// <cache>/bin/tentmaker_<os>_<arch>-<version>, downloaded and verified with
// internal/update for the exact running version (dev builds: the dev
// release). The version is in the name so an update fetches a new copy
// instead of serving the old one to check jobs forever (review r1 partB
// first-F2, sol-F3); copies for other versions are deleted. A hand build
// has no release, so it uses a binary put at <cache>/bin/tentmaker_<os>_<arch>.
func NewBinary(o BinaryOptions) func(core.Platform) (string, error) {
	if o.Executable == nil {
		o.Executable = os.Executable
	}
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOARCH == "" {
		o.GOARCH = runtime.GOARCH
	}
	if o.ReleaseBase == "" {
		o.ReleaseBase = ReleaseBase
	}
	if o.Download == nil {
		o.Download = func(ctx context.Context, goos, goarch string, rel update.Release, dir string) (string, error) {
			return update.New(update.Options{GOOS: goos, GOARCH: goarch}).Download(ctx, rel, dir)
		}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	var mu sync.Mutex
	return func(p core.Platform) (string, error) {
		goos, goarch := GoPlatform(p)
		if goos == o.GOOS && goarch == o.GOARCH {
			exe, err := o.Executable()
			if err != nil {
				return "", err
			}
			if r, err := filepath.EvalSymlinks(exe); err == nil {
				exe = r
			}
			return exe, nil
		}
		ext := ""
		if goos == "windows" {
			ext = ".exe"
		}
		base := "tentmaker_" + goos + "_" + goarch
		mu.Lock()
		defer mu.Unlock()
		tag, err := releaseTag(o.Version)
		if err != nil {
			hand := filepath.Join(o.CacheDir, "bin", base+ext)
			if st, serr := os.Stat(hand); serr == nil && st.Mode().IsRegular() {
				return hand, nil
			}
			return "", fmt.Errorf("no %s/%s tentmaker for check jobs: %w; build one with GOOS=%s GOARCH=%s and put it at %s", goos, goarch, err, goos, goarch, hand)
		}
		dst := filepath.Join(o.CacheDir, "bin", base+"-"+fileSafe(o.Version)+ext)
		if st, err := os.Stat(dst); err == nil && st.Mode().IsRegular() {
			return dst, nil
		}
		rel := update.Release{Tag: tag, Version: o.Version, Assets: map[string]string{}}
		for _, a := range []string{update.ArchiveName(goos, goarch), update.SumsName} {
			rel.Assets[a] = o.ReleaseBase + "/" + tag + "/" + a
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		tmp, err := os.MkdirTemp(filepath.Dir(dst), ".dl-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmp)
		ctx, cancel := context.WithTimeout(context.Background(), BinaryDownloadTimeout)
		defer cancel()
		o.Log.Info("downloading guest binary", "platform", p.String(), "release", tag)
		got, err := o.Download(ctx, goos, goarch, rel, tmp)
		if err != nil {
			return "", fmt.Errorf("download tentmaker for %s: %w", p, err)
		}
		if err := copyFile(got, dst, 0o755); err != nil {
			return "", err
		}
		pruneGuestBinaries(filepath.Dir(dst), base+"-", filepath.Base(dst), o.Log)
		return dst, nil
	}
}

// fileSafe keeps a version usable in a file name.
func fileSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '+' {
			return r
		}
		return '_'
	}, s)
}

// pruneGuestBinaries deletes cached copies of one platform's binary for
// other versions. A copy a running job still uses stays readable on Unix;
// a failed delete only costs disk.
func pruneGuestBinaries(dir, prefix, keep string, log *slog.Logger) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if n := e.Name(); strings.HasPrefix(n, prefix) && n != keep && e.Type().IsRegular() {
			if err := os.Remove(filepath.Join(dir, n)); err != nil {
				log.Debug("old guest binary not deleted", "file", n, "error", err)
			}
		}
	}
}

// releaseTag is the release a version came from: "dev" for dev builds,
// "vX.Y.Z" for stable ones. A hand build has none.
func releaseTag(version string) (string, error) {
	if strings.Contains(version, "-dev.") {
		return update.DevTag, nil
	}
	v, err := update.ParseVersion(version)
	if err != nil || len(v.Pre) > 0 {
		return "", fmt.Errorf("version %q is a hand build with no release", version)
	}
	return "v" + v.String(), nil
}

// copyFile copies src to dst atomically with mode.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-"+filepath.Base(dst)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
