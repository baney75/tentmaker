package darwin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/baney75/tentmaker/internal/core"
)

// Disk room Preflight wants on Tart's volume: one job's clone grows with its
// build, and an image that isn't pulled yet needs its own size on top.
const (
	diskPerVM       = 40 << 30
	diskImageMacOS  = 80 << 30
	diskImageLinux  = 10 << 30
	minMacOSRelease = 26
)

func (e *Executor) softnetPath() string { return filepath.Join(e.o.LibDir, "bin", "softnet") }
func (e *Executor) tartApp() string     { return filepath.Join(e.o.LibDir, "tart", "tart.app") }

// Preflight implements core.Executor. Every check that can't run counts as a
// problem (fail closed).
func (e *Executor) Preflight(ctx context.Context) []core.Problem {
	var ps []core.Problem
	add := func(check, what, fix string) { ps = append(ps, core.Problem{Check: check, What: what, Fix: fix}) }
	setup := "run sudo tentmaker setup"
	if e.o.HostArch != "arm64" {
		add("arch", "Tart needs Apple silicon; this Mac is "+e.o.HostArch, "use an Apple silicon Mac")
		return ps
	}
	if major, err := e.macOSMajor(ctx); err != nil {
		add("macOS", "can't read the macOS version: "+err.Error(), "check that sw_vers works")
	} else if major < minMacOSRelease {
		add("macOS", fmt.Sprintf("macOS %d is too old: Softnet (the VMs' network isolation) needs macOS %d or newer", major, minMacOSRelease), "update macOS")
	}
	if res, err := e.t.Run(ctx, e.tart(30*time.Second, "--version")); err != nil {
		add("tart", "tart is not installed at "+e.tartPath(), setup)
	} else if v := strings.TrimSpace(res.Stdout); v != TartVersion {
		add("tart", fmt.Sprintf("tart is %q; the pinned version is %s", trunc(v, 40), TartVersion), setup)
	}
	if _, err := e.t.Run(ctx, Cmd{Path: "/usr/bin/codesign", Args: []string{"--verify", "--deep", "--strict", e.tartApp()}, Timeout: 2 * time.Minute}); err != nil {
		add("tart codesign", "tart.app fails codesign verification", setup)
	}
	if st, err := os.Lstat(e.softnetPath()); err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSetuid == 0 {
		add("softnet", e.softnetPath()+" is missing or not setuid", setup)
	} else if res, err := e.t.Run(ctx, Cmd{Path: "/usr/bin/stat", Args: []string{"-f", "%u", e.softnetPath()}, Timeout: 30 * time.Second}); err != nil || strings.TrimSpace(res.Stdout) != "0" {
		add("softnet", e.softnetPath()+" is not owned by root", setup)
	}
	pulled, err := e.pulledImages(ctx)
	if err != nil {
		add("images", err.Error(), setup)
	}
	need := int64(diskPerVM)
	for plat, img := range e.o.Images {
		if img == "" {
			continue
		}
		if !strings.Contains(img, "@sha256:") {
			add("images", plat+" image "+img+" is not pinned by digest", "pin it as repo@sha256:<digest>")
		}
		if err == nil && !contains(pulled, img) {
			add("images", plat+" image is not pulled yet", setup)
			if strings.HasPrefix(plat, "macos/") {
				need += diskImageMacOS
			} else {
				need += diskImageLinux
			}
		}
	}
	if free, err := e.freeDisk(ctx); err != nil {
		add("disk", "can't read free disk space: "+err.Error(), "check that df works")
	} else if free < need {
		add("disk", fmt.Sprintf("%d GiB free where Tart keeps VMs; want %d GiB", free>>30, need>>30), "free disk space")
	}
	return ps
}

func (e *Executor) macOSMajor(ctx context.Context) (int, error) {
	res, err := e.t.Run(ctx, Cmd{Path: "/usr/bin/sw_vers", Args: []string{"-productVersion"}, Timeout: 30 * time.Second})
	if err != nil {
		return 0, err
	}
	v := strings.TrimSpace(res.Stdout)
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, fmt.Errorf("unrecognized version %q", trunc(v, 40))
	}
	return n, nil
}

func (e *Executor) pulledImages(ctx context.Context) ([]string, error) {
	res, err := e.t.Run(ctx, e.tart(time.Minute, "list", "--source", "oci", "--quiet"))
	if err != nil {
		return nil, fmt.Errorf("tart list: %v", err)
	}
	var out []string
	for _, l := range strings.Split(res.Stdout, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

// freeDisk reads the bytes available on Tart's volume from `df -k`.
func (e *Executor) freeDisk(ctx context.Context) (int64, error) {
	dir := e.o.TartHome
	for {
		if _, err := os.Stat(dir); err == nil || dir == filepath.Dir(dir) {
			break
		}
		dir = filepath.Dir(dir)
	}
	res, err := e.t.Run(ctx, Cmd{Path: "/bin/df", Args: []string{"-k", dir}, Timeout: 30 * time.Second})
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) < 2 {
		return 0, errors.New("unrecognized df output")
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, errors.New("unrecognized df output")
	}
	kb, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return 0, errors.New("unrecognized df output")
	}
	return kb << 10, nil
}

// Install sets up the host: Tart and Softnet (pinned and checksummed; Tart
// codesign-verified; Softnet setuid root, the one privileged piece, which
// drops privileges once the vmnet interface exists), the pinned images, and
// the state dirs. It deletes leftover job VMs and older pulls of the same
// image repositories. It runs as root and is safe to rerun.
func Install(ctx context.Context, o Options) error {
	e := New(o)
	t := e.t
	if e.o.HostArch != "arm64" {
		return errors.New("Tart needs Apple silicon")
	}
	major, err := e.macOSMajor(ctx)
	if err != nil {
		return err
	}
	if major < minMacOSRelease {
		return fmt.Errorf("macOS %d: Softnet needs macOS %d or newer", major, minMacOSRelease)
	}
	for plat, img := range e.o.Images {
		if img != "" && !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(img) {
			return fmt.Errorf("%s image %s is not pinned by digest", plat, img)
		}
	}
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{
		{e.o.LibDir, 0o755}, {filepath.Join(e.o.LibDir, "bin"), 0o755}, {filepath.Join(e.o.CacheDir, "dist"), 0o755},
		{e.base, 0o700}, {e.o.TartHome, 0o700}, {e.logDir(), 0o700}, {e.outRoot(), 0o700},
	} {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
	}

	// Tart.
	tartDir := filepath.Join(e.o.LibDir, "tart-"+TartVersion)
	if _, err := os.Stat(filepath.Join(tartDir, "tart.app", "Contents", "MacOS", "tart")); err != nil {
		tarball := filepath.Join(e.o.CacheDir, "dist", "tart-"+TartVersion+".tar.gz")
		if err := fetchVerified(ctx, t, TartURL, tarball, e.o.Pins.TartSHA256); err != nil {
			return fmt.Errorf("tart %s: %w", TartVersion, err)
		}
		part := tartDir + ".part"
		os.RemoveAll(part)
		if err := os.MkdirAll(part, 0o755); err != nil {
			return err
		}
		if _, err := t.Run(ctx, Cmd{Path: "/usr/bin/tar", Args: []string{"-xzf", tarball, "-C", part}, Timeout: 10 * time.Minute}); err != nil {
			return fmt.Errorf("extract tart: %v", err)
		}
		if _, err := t.Run(ctx, Cmd{Path: "/usr/bin/codesign", Args: []string{"--verify", "--deep", "--strict", filepath.Join(part, "tart.app")}, Timeout: 2 * time.Minute}); err != nil {
			os.RemoveAll(part)
			return fmt.Errorf("tart.app failed codesign verification: %v", err)
		}
		if err := rootOwnReadOnly(t, part); err != nil {
			return err
		}
		if err := os.Rename(part, tartDir); err != nil {
			return err
		}
	}
	if err := replaceSymlink(tartDir, filepath.Join(e.o.LibDir, "tart")); err != nil {
		return err
	}

	// Softnet: setuid root so Tart can start it for --net-softnet.
	snTar := filepath.Join(e.o.CacheDir, "dist", "softnet-"+SoftnetVersion+".tar.gz")
	if err := fetchVerified(ctx, t, SoftnetURL, snTar, e.o.Pins.SoftnetSHA256); err != nil {
		return fmt.Errorf("softnet %s: %w", SoftnetVersion, err)
	}
	work, err := os.MkdirTemp(filepath.Join(e.o.CacheDir, "dist"), "softnet-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if _, err := t.Run(ctx, Cmd{Path: "/usr/bin/tar", Args: []string{"-xzf", snTar, "-C", work}, Timeout: 5 * time.Minute}); err != nil {
		return fmt.Errorf("extract softnet: %v", err)
	}
	bin, err := os.ReadFile(filepath.Join(work, "softnet"))
	if err != nil {
		return fmt.Errorf("the softnet archive has no softnet binary: %v", err)
	}
	tmp := e.softnetPath() + ".tmp"
	os.Remove(tmp)
	if err := os.WriteFile(tmp, bin, 0o700); err != nil {
		return err
	}
	if err := t.Lchown(tmp, 0, 0); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755|os.ModeSetuid); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, e.softnetPath()); err != nil {
		return err
	}

	// No job VM survives an install.
	if vms, err := e.localVMs(ctx); err != nil {
		return err
	} else {
		for _, v := range vms {
			if strings.HasPrefix(v, e.o.Prefix) {
				if err := e.deleteVM(ctx, v); err != nil {
					return err
				}
			}
		}
	}

	// Pull the pinned images (each is large), then delete older pulls of
	// exactly the same repositories.
	for _, plat := range sortedKeys(e.o.Images) {
		img := e.o.Images[plat]
		if img == "" {
			continue
		}
		if _, err := t.Run(ctx, e.tart(6*time.Hour, "pull", img)); err != nil {
			return fmt.Errorf("pull %s: %v", img, err)
		}
	}
	pulled, err := e.pulledImages(ctx)
	if err != nil {
		return err
	}
	for _, old := range e.supersededImages(pulled) {
		if _, err := t.Run(ctx, e.tart(10*time.Minute, "delete", old)); err != nil {
			return fmt.Errorf("delete superseded image %s: %v", old, err)
		}
	}
	e.log.Info("installed", "executor", Name, "tart", TartVersion, "softnet", SoftnetVersion)
	return nil
}

// supersededImages are pulls of a pinned image's repository (as
// repo@sha256:<64 hex> or repo:<tag>, never an image whose name merely
// contains it) that are not the pinned image itself.
func (e *Executor) supersededImages(pulled []string) []string {
	var out []string
	for _, img := range e.o.Images {
		if img == "" {
			continue
		}
		repo, _, _ := strings.Cut(img, "@")
		re := regexp.MustCompile(`^` + regexp.QuoteMeta(repo) + `(@sha256:[0-9a-f]{64}|:[A-Za-z0-9._-]+)$`)
		for _, p := range pulled {
			if p != img && re.MatchString(p) && !e.pinned(p) {
				out = append(out, p)
			}
		}
	}
	return out
}

func (e *Executor) pinned(img string) bool {
	for _, v := range e.o.Images {
		if v == img {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Uninstall deletes every job VM and proves none is left before removing
// Tart and Softnet; if Tart can't list or delete a VM it stops with Tart
// still installed, so a rerun has what it needs. With purge it also deletes
// the pulled images of the pinned repositories, outputs, logs and downloads.
func Uninstall(ctx context.Context, o Options, purge bool) error {
	e := New(o)
	t := e.t
	if _, err := os.Stat(e.tartPath()); err == nil {
		vms, err := e.localVMs(ctx)
		if err != nil {
			return fmt.Errorf("uninstall stopped, Tart kept: %v", err)
		}
		for _, v := range vms {
			if strings.HasPrefix(v, e.o.Prefix) {
				if err := e.deleteVM(ctx, v); err != nil {
					return fmt.Errorf("uninstall stopped, Tart kept: %v", err)
				}
			}
		}
		if purge {
			pulled, err := e.pulledImages(ctx)
			if err != nil {
				return fmt.Errorf("uninstall stopped, Tart kept: %v", err)
			}
			for _, p := range pulled {
				if e.pinned(p) || contains(e.supersededImages([]string{p}), p) {
					if _, err := t.Run(ctx, e.tart(10*time.Minute, "delete", p)); err != nil {
						return fmt.Errorf("uninstall stopped, Tart kept: delete image %s: %v", p, err)
					}
				}
			}
		}
	}
	entries, _ := os.ReadDir(e.o.LibDir)
	for _, ent := range entries {
		if n := ent.Name(); n == "tart" || strings.HasPrefix(n, "tart-") {
			if err := os.RemoveAll(filepath.Join(e.o.LibDir, n)); err != nil {
				return err
			}
		}
	}
	if err := os.Remove(e.softnetPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	os.Remove(filepath.Join(e.o.LibDir, "bin")) // only if empty
	if purge {
		if err := os.RemoveAll(e.base); err != nil {
			return err
		}
		if entries, err := os.ReadDir(filepath.Join(e.o.CacheDir, "dist")); err == nil {
			for _, ent := range entries {
				if n := ent.Name(); strings.HasPrefix(n, "tart-") || strings.HasPrefix(n, "softnet-") {
					os.RemoveAll(filepath.Join(e.o.CacheDir, "dist", n))
				}
			}
		}
	}
	return nil
}

func rootOwnReadOnly(t Tools, top string) error {
	return filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := t.Lchown(p, 0, 0); err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return os.Chmod(p, info.Mode().Perm()&^0o022)
	})
}

func replaceSymlink(target, link string) error {
	if cur, err := os.Readlink(link); err == nil && cur == target {
		return nil
	}
	tmp := link + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}
