package windows

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
)

// Limits on the runner zip, which is checked against a pinned hash before
// it is opened; these only bound a mistake.
const (
	maxZipFiles = 20000
	maxZipBytes = 2 << 30
)

// Preflight implements core.Executor.
func (e *Executor) Preflight(ctx context.Context) []core.Problem {
	var p []core.Problem
	add := func(check, what, fix string) { p = append(p, core.Problem{Check: check, What: what, Fix: fix}) }
	install := "run `tentmaker install` as an administrator"
	if _, ok := e.pin(); !ok {
		add("windows-arch", "no actions/runner build for "+e.arch, "use an x64 or arm64 Windows machine")
		return p
	}
	if ok, err := e.api.IsAdmin(); err != nil || !ok {
		add("windows-admin", "the daemon is not running as SYSTEM or an administrator",
			"run the Tentmaker service as LocalSystem")
		return p
	}
	sid, err := e.api.UserSID(User)
	if err != nil {
		add("windows-user", "local user "+User+" is missing", install)
		return p
	}
	if groups, err := e.api.LocalGroups(User); err != nil {
		add("windows-user-groups", "can't read "+User+"'s groups: "+err.Error(), install)
	} else {
		for _, g := range groups {
			if g.SID != SIDUsers {
				add("windows-user-groups", User+" is in the "+g.Name+" group", "remove it, or "+install)
			}
		}
	}
	if have, err := e.api.AccountRights(sid); err != nil {
		add("windows-user-rights", "can't read "+User+"'s logon rights: "+err.Error(), install)
	} else {
		for _, r := range append(slices.Clone(GrantRights), DenyRights...) {
			if !slices.Contains(have, r) {
				add("windows-user-rights", User+" lacks "+r, install)
			}
		}
	}
	if pw, err := e.password(); err != nil {
		add("windows-password", err.Error(), install)
	} else {
		clear(pw)
	}
	for _, f := range []struct{ path, what string }{
		{filepath.Join(e.template(), "run.cmd"), "the actions/runner template"},
		{e.script(), "the job script"},
		{e.guestBinary(), "the tentmaker binary for check jobs"},
	} {
		if st, err := os.Stat(f.path); err != nil || !st.Mode().IsRegular() {
			add("windows-files", f.what+" is missing at "+f.path, install)
		}
	}
	if st, err := os.Stat(e.jobsDir()); err != nil || !st.IsDir() {
		add("windows-files", "the jobs directory is missing at "+e.jobsDir(), install)
	}
	if free, err := e.api.FreeDisk(e.o.StateDir); err != nil {
		add("windows-disk", "can't read free disk space: "+err.Error(), "check "+e.o.StateDir)
	} else if free < MinFreeDisk {
		add("windows-disk", fmt.Sprintf("%d GiB free under %s; jobs need %d GiB", free>>30, e.o.StateDir, MinFreeDisk>>30),
			"free some disk space")
	}
	return p
}

// newPassword is 32 random bytes, base64, behind a prefix that meets any
// complexity policy (upper, lower, digit, symbol).
func newPassword() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	out := make([]byte, 4+base64.RawURLEncoding.EncodedLen(len(b)))
	copy(out, "Tm1!")
	base64.RawURLEncoding.Encode(out[4:], b)
	clear(b)
	return out, nil
}

// Install creates the runner user and the files jobs need. Run it as an
// administrator. It is safe to run again: it resets the password, the
// user's groups and rights, and the ACLs, and keeps a verified runner.
func Install(ctx context.Context, o Options) error {
	e := New(o)
	pin, ok := e.pin()
	if !ok {
		return fmt.Errorf("no actions/runner build for windows/%s", e.arch)
	}
	if ok, err := e.api.IsAdmin(); err != nil || !ok {
		return errors.New("install needs an administrator or SYSTEM")
	}
	if err := os.MkdirAll(e.o.StateDir, 0o755); err != nil {
		return err
	}
	for _, d := range []string{e.jobsDir(), e.libDir(), e.runnerRoot(), e.secretDir(), e.distDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := e.api.SetDACL(d, adminACL()); err != nil {
			return fmt.Errorf("protect %s: %w", d, err)
		}
	}
	sid, err := e.installUser()
	if err != nil {
		return err
	}
	if err := e.api.SetDACL(e.libDir(), libACL(sid)); err != nil {
		return fmt.Errorf("protect %s: %w", e.libDir(), err)
	}
	if err := config.WriteAtomic(e.script(), []byte(jobRunnerPS1), 0o644); err != nil {
		return err
	}
	if o.Binary == nil {
		return errors.New("install needs the tentmaker binary for check jobs")
	}
	bin, err := o.Binary(core.Platform{OS: "windows", Arch: e.arch})
	if err != nil {
		return fmt.Errorf("find the tentmaker binary for windows/%s: %w", e.arch, err)
	}
	tmp := e.guestBinary() + ".new"
	os.Remove(tmp)
	if err := copyFile(bin, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, e.guestBinary()); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := e.installRunner(ctx, pin); err != nil {
		return err
	}
	e.log.Info("installed", "executor", Name, "runner", pin.Version, "arch", e.arch)
	return nil
}

// installUser creates or resets the runner user and returns its SID. The
// new password is protected and written before the account changes, and
// moved into place after, so a failure leaves the old pair intact.
func (e *Executor) installUser() (string, error) {
	pw, err := newPassword()
	if err != nil {
		return "", err
	}
	defer clear(pw)
	blob, err := e.api.Protect(pw)
	if err != nil {
		return "", fmt.Errorf("protect the runner password: %w", err)
	}
	next := e.passwordPath() + ".new"
	if err := config.WriteAtomic(next, blob, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(next)
	if _, err := e.api.UserSID(User); errors.Is(err, ErrNoUser) {
		if err := e.api.CreateUser(User, pw); err != nil {
			return "", fmt.Errorf("create %s: %s", User, redact(err.Error(), pw))
		}
	} else if err != nil {
		return "", err
	} else if err := e.api.SetPassword(User, pw); err != nil {
		return "", fmt.Errorf("reset %s's password: %s", User, redact(err.Error(), pw))
	}
	if err := os.Rename(next, e.passwordPath()); err != nil {
		return "", err
	}
	sid, err := e.api.UserSID(User)
	if err != nil {
		return "", err
	}
	groups, err := e.api.LocalGroups(User)
	if err != nil {
		return "", err
	}
	for _, g := range groups {
		if g.SID != SIDUsers {
			if err := e.api.RemoveFromGroup(g.Name, User); err != nil {
				return "", fmt.Errorf("remove %s from %s: %w", User, g.Name, err)
			}
		}
	}
	if err := e.api.AddRights(sid, append(slices.Clone(GrantRights), DenyRights...)); err != nil {
		return "", fmt.Errorf("set %s's logon rights: %w", User, err)
	}
	if err := e.api.HideUser(User, true); err != nil {
		return "", fmt.Errorf("hide %s from the sign-in screen: %w", User, err)
	}
	return sid, nil
}

func (e *Executor) installRunner(ctx context.Context, pin Pin) error {
	dst := filepath.Join(e.runnerRoot(), pin.Dir())
	if _, err := os.Stat(filepath.Join(dst, "run.cmd")); err != nil {
		zipPath := filepath.Join(e.distDir(), "actions-runner-win-"+pin.Arch+"-"+pin.Version+".zip")
		if err := fetchVerified(ctx, e.api, pin.URL(), zipPath, pin.SHA256); err != nil {
			return fmt.Errorf("download actions/runner %s: %w", pin.Version, err)
		}
		tmp := filepath.Join(e.runnerRoot(), ".extract-"+pin.Dir())
		os.RemoveAll(tmp)
		if err := unzip(zipPath, tmp); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("extract actions/runner: %w", err)
		}
		if _, err := os.Stat(filepath.Join(tmp, "run.cmd")); err != nil {
			os.RemoveAll(tmp)
			return errors.New("the actions/runner zip has no run.cmd")
		}
		os.RemoveAll(dst)
		if err := os.Rename(tmp, dst); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	ents, err := os.ReadDir(e.runnerRoot())
	if err != nil {
		return err
	}
	for _, d := range ents {
		if d.Name() != pin.Dir() {
			os.RemoveAll(filepath.Join(e.runnerRoot(), d.Name()))
		}
	}
	return nil
}

// unzip extracts src into a new directory dst. Entries that would land
// outside dst, links and anything but files and directories are refused.
func unzip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	if len(r.File) > maxZipFiles {
		return fmt.Errorf("%d entries; the limit is %d", len(r.File), maxZipFiles)
	}
	if err := os.Mkdir(dst, 0o700); err != nil {
		return err
	}
	var total int64
	for _, f := range r.File {
		name := filepath.FromSlash(f.Name)
		if strings.Contains(f.Name, `\`) || !filepath.IsLocal(name) {
			return fmt.Errorf("entry %q leaves the target directory", trunc(f.Name, 100))
		}
		to := filepath.Join(dst, name)
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(to, 0o700); err != nil {
				return err
			}
			continue
		case !mode.IsRegular():
			return fmt.Errorf("entry %q is not a regular file", trunc(f.Name, 100))
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			return err
		}
		n, err := extractOne(f, to, maxZipBytes-total)
		if err != nil {
			return err
		}
		total += n
	}
	return nil
}

func extractOne(f *zip.File, to string, room int64) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, io.LimitReader(rc, room+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > room {
		err = fmt.Errorf("the zip is over %d bytes unpacked", int64(maxZipBytes))
	}
	return n, err
}

// Uninstall removes what Install made. Without purge it keeps the user,
// its password and the download cache. A missing piece is not an error.
func Uninstall(ctx context.Context, o Options, purge bool) error {
	e := New(o)
	var errs []error
	sid, err := e.api.UserSID(User)
	if err != nil && !errors.Is(err, ErrNoUser) {
		errs = append(errs, err)
	}
	if err == nil {
		if _, err := e.api.DeleteUserTasks(User, sid); err != nil {
			errs = append(errs, err)
		}
		if _, err := e.api.KillUserProcesses(sid); err != nil {
			errs = append(errs, err)
		}
	}
	for _, d := range []string{e.jobsDir(), e.libDir(), e.runnerRoot()} {
		if err := e.removeAll(d); err != nil {
			errs = append(errs, err)
		}
	}
	if !purge {
		return errors.Join(errs...)
	}
	if sid != "" {
		if err := e.api.RemoveAllRights(sid); err != nil {
			errs = append(errs, fmt.Errorf("remove %s's logon rights: %w", User, err))
		}
	}
	if err := e.api.DeleteUser(User); err != nil {
		errs = append(errs, fmt.Errorf("delete %s: %w", User, err))
	}
	if err := e.api.HideUser(User, false); err != nil {
		errs = append(errs, err)
	}
	for _, d := range []string{e.secretDir(), e.distDir()} {
		if err := e.removeAll(d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
