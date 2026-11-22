package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The service runs as root, so it must never run a binary a user could
// change: that user would get root on the next restart or update. Review r1
// partB second-F7: install.sh puts the binary in ~/.local/bin without root
// and then runs `sudo ~/.local/bin/tentmaker setup`.

func (f *fixture) writeExe(content string) string {
	f.t.Helper()
	exe, _ := f.env.Executable()
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte(content), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return exe
}

// noUserWritable fails when the service spec names a path a user can
// change.
func (f *fixture) noUserWritable() {
	f.t.Helper()
	if f.svc.spec == nil {
		return
	}
	if why, _ := f.env.BinaryProblem(f.svc.spec.Binary); why != "" {
		f.t.Fatalf("the service runs %s: %s", f.svc.spec.Binary, why)
	}
}

func TestSetupCopiesUserWritableBinary(t *testing.T) {
	f := newFixture(t)
	exe := f.writeExe("tentmaker build 1")
	f.userWritable[filepath.Dir(exe)] = filepath.Dir(exe) + " is owned by uid 1000, not root"
	if code := f.run("", "setup", "--yes", "--no-mesh"); code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, f.stdout, f.stderr)
	}
	want := filepath.Join(f.env.ServiceBinDirs[0], "tentmaker")
	if f.svc.spec == nil || f.svc.spec.Binary != want {
		t.Fatalf("service spec %+v, want binary %s", f.svc.spec, want)
	}
	f.noUserWritable()
	got, err := os.ReadFile(want)
	if err != nil || string(got) != "tentmaker build 1" {
		t.Fatalf("copy %q %v", got, err)
	}
	// Windows has no mode bits: os.Stat reports 0666 for any file without
	// an executable extension, and ACLs guard the folder instead.
	if st, _ := os.Stat(want); runtime.GOOS != "windows" && st.Mode().Perm() != 0o755 {
		t.Errorf("copy mode %v", st.Mode())
	}
	out := f.stdout.String()
	if !strings.Contains(out, want) || !strings.Contains(out, "remove "+exe) {
		t.Errorf("setup does not say where the service binary went and to remove the old one:\n%s", out)
	}

	// A rerun from a newer user copy replaces the root-owned one.
	f.writeExe("tentmaker build 2")
	if code := f.run("", "setup", "--yes", "--no-mesh"); code != ExitOK {
		t.Fatalf("rerun exit %d\n%s", code, f.stdout)
	}
	if got, _ := os.ReadFile(want); string(got) != "tentmaker build 2" {
		t.Errorf("rerun left %q", got)
	}
}

// When the first folder isn't root-owned either, setup uses the next.
func TestSetupSkipsUserWritableServiceDir(t *testing.T) {
	f := newFixture(t)
	exe := f.writeExe("tm")
	f.userWritable[filepath.Dir(exe)] = "user's"
	f.userWritable[f.env.ServiceBinDirs[0]] = "Homebrew's"
	if code := f.run("", "setup", "--yes", "--no-mesh"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, f.stdout)
	}
	want := filepath.Join(f.env.ServiceBinDirs[1], "tentmaker")
	if f.svc.spec == nil || f.svc.spec.Binary != want {
		t.Fatalf("service spec %+v, want %s", f.svc.spec, want)
	}
	f.noUserWritable()
	if _, err := os.Stat(filepath.Join(f.env.ServiceBinDirs[0], "tentmaker")); err == nil {
		t.Error("copied into the user-writable folder")
	}
}

// With no root-owned folder to copy into, setup installs no service.
func TestSetupRefusesWithNoSafeServiceDir(t *testing.T) {
	f := newFixture(t)
	exe := f.writeExe("tm")
	f.userWritable[filepath.Dir(exe)] = "user's"
	for _, d := range f.env.ServiceBinDirs {
		f.userWritable[d] = "user's"
	}
	code := f.run("", "setup", "--yes", "--no-mesh")
	if f.svc.spec != nil {
		t.Fatalf("service installed with %s", f.svc.spec.Binary)
	}
	if code == ExitOK && !strings.Contains(f.stdout.String(), "root-owned") {
		t.Errorf("exit %d, no word on why:\n%s", code, f.stdout)
	}
}

// A root-owned binary is used in place, and nothing is copied.
func TestSetupRootOwnedBinaryInPlace(t *testing.T) {
	f := newFixture(t)
	exe := f.writeExe("tm")
	if code := f.run("", "setup", "--yes", "--no-mesh"); code != ExitOK {
		t.Fatalf("exit %d\n%s", code, f.stdout)
	}
	if f.svc.spec == nil || f.svc.spec.Binary != exe {
		t.Fatalf("service spec %+v, want %s", f.svc.spec, exe)
	}
	for _, d := range f.env.ServiceBinDirs {
		if _, err := os.Stat(filepath.Join(d, "tentmaker")); err == nil {
			t.Errorf("copied into %s", d)
		}
	}
}

// update and rollback run as root on the root-owned copy, never on a
// binary a user can change (rollback copies <bin>.prev back, so a user
// who could write it would get root).
func TestUpdateAndRollbackUseRootOwnedCopy(t *testing.T) {
	f := newFixture(t)
	exe := f.writeExe("tm")
	f.userWritable[filepath.Dir(exe)] = "user's"
	f.setUp()
	f.up.newer = true

	// No root-owned copy yet: refuse.
	if code := f.run("", "update", "--json"); code == ExitOK || len(f.up.appliedTo) != 0 {
		t.Fatalf("update of a user-writable binary: exit %d applied to %v", code, f.up.appliedTo)
	}
	if code := f.run("", "rollback", "--json"); code == ExitOK || len(f.up.rollbackTo) != 0 {
		t.Fatalf("rollback of a user-writable binary: exit %d to %v", code, f.up.rollbackTo)
	}

	// Setup made the copy: both work on it.
	if code := f.run("", "setup", "--yes", "--no-mesh"); code != ExitOK {
		t.Fatalf("setup exit %d\n%s", code, f.stdout)
	}
	target := filepath.Join(f.env.ServiceBinDirs[0], "tentmaker")
	f.svc.installed = false
	if code := f.run("", "update", "--json"); code != ExitOK || len(f.up.appliedTo) != 1 || f.up.appliedTo[0] != target {
		t.Fatalf("update: exit %d applied to %v, want %s\n%s", code, f.up.appliedTo, target, f.stdout)
	}
	if code := f.run("", "rollback", "--json"); code != ExitOK || len(f.up.rollbackTo) != 1 || f.up.rollbackTo[0] != target {
		t.Fatalf("rollback: exit %d to %v, want %s", code, f.up.rollbackTo, target)
	}
}

// symlinkedServiceDir makes the first service folder a symlink, in a
// folder a user can write, to a root-owned folder: user-writable
// /usr/local with bin -> /opt/safe. It returns the resolved folder.
func (f *fixture) symlinkedServiceDir() string {
	f.t.Helper()
	safe := filepath.Join(f.root, "opt", "safe")
	link := f.env.ServiceBinDirs[0]
	if err := os.MkdirAll(safe, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(safe, link); err != nil {
		f.t.Skipf("can't make a symlink here: %v", err)
	}
	f.userWritable[filepath.Dir(link)] = filepath.Dir(link) + " is owned by uid 1000, not root"
	return safe
}

// The service, update and rollback use the resolved folder, never the
// path through a symlink a user could repoint after the check (review r2
// partB sol-second-F7).
func TestServiceBinaryUsesTheResolvedPath(t *testing.T) {
	f := newFixture(t)
	exe := f.writeExe("tm")
	f.userWritable[filepath.Dir(exe)] = "user's"
	safe := f.symlinkedServiceDir()
	if code := f.run("", "setup", "--yes", "--no-mesh"); code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, f.stdout, f.stderr)
	}
	want := filepath.Join(safe, "tentmaker")
	if f.svc.spec == nil || f.svc.spec.Binary != want {
		t.Fatalf("service spec %+v, want binary %s", f.svc.spec, want)
	}
	f.noUserWritable()
	f.up.newer = true
	f.svc.installed = false
	if code := f.run("", "update", "--json"); code != ExitOK || len(f.up.appliedTo) != 1 || f.up.appliedTo[0] != want {
		t.Fatalf("update: exit %d applied to %v, want %s\n%s%s", code, f.up.appliedTo, want, f.stdout, f.stderr)
	}
	if code := f.run("", "rollback", "--json"); code != ExitOK || len(f.up.rollbackTo) != 1 || f.up.rollbackTo[0] != want {
		t.Fatalf("rollback: exit %d to %v, want %s", code, f.up.rollbackTo, want)
	}
}

// On Windows an elevated user owns what it creates, and an owner can always
// rewrite the DACL, so setup must make Administrators the owner before it
// makes the service's folder and copy, not after. Its JSON names the binary
// the service runs, which is not the one on PATH after a copy.
func TestSetupOwnsTheCopyBeforeMakingIt(t *testing.T) {
	f := newFixture(t)
	exe := f.writeExe("tentmaker build 1")
	f.userWritable[filepath.Dir(exe)] = filepath.Dir(exe) + " is owned by uid 1000, not root"
	dir := f.env.ServiceBinDirs[0]
	calls := 0
	f.env.OwnNewFiles = func() error {
		calls++
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("OwnNewFiles ran after %s was made", dir)
		}
		return nil
	}
	if code := f.run("", "setup", "--yes", "--no-mesh", "--json"); code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, f.stdout, f.stderr)
	}
	if calls == 0 {
		t.Fatal("setup copied the binary without OwnNewFiles")
	}
	var v struct{ Binary string }
	if err := json.Unmarshal(f.stdout.Bytes(), &v); err != nil {
		t.Fatalf("%v\n%s", err, f.stdout)
	}
	if want := filepath.Join(dir, "tentmaker"); v.Binary != want {
		t.Errorf("setup --json binary %q, want %s", v.Binary, want)
	}
}

// When OwnNewFiles fails (Windows, not elevated), setup makes no copy.
func TestSetupRefusesCopyWhenItCannotOwnIt(t *testing.T) {
	f := newFixture(t)
	exe := f.writeExe("tentmaker build 1")
	f.userWritable[filepath.Dir(exe)] = filepath.Dir(exe) + " is owned by uid 1000, not root"
	f.env.OwnNewFiles = func() error { return errors.New("not elevated") }
	if code := f.run("", "setup", "--yes", "--no-mesh"); code == ExitOK {
		t.Fatalf("setup succeeded\n%s", f.stdout)
	}
	for _, d := range f.env.ServiceBinDirs {
		if _, err := os.Stat(d); err == nil {
			t.Errorf("setup made %s", d)
		}
	}
	if f.svc.spec != nil {
		t.Errorf("service installed: %+v", f.svc.spec)
	}
}

// update and rollback write the new binary and the .prev copy as root. On
// Windows those files would be owned by the elevated user, and the next
// rollback refused them (the upgrade scenario's rollback failed on exactly
// this), so both make Administrators the owner before they write anything.
func TestUpdateAndRollbackOwnNewFilesFirst(t *testing.T) {
	f := newFixture(t)
	f.writeExe("tm")
	f.setUp()
	f.up.newer = true
	f.svc.installed = false
	owned := false
	f.env.OwnNewFiles = func() error { owned = true; return nil }
	f.up.onApply = func() {
		if !owned {
			t.Error("update applied before OwnNewFiles")
		}
	}
	if code := f.run("", "update", "--json"); code != ExitOK || len(f.up.appliedTo) != 1 {
		t.Fatalf("update: exit %d applied to %v\n%s", code, f.up.appliedTo, f.stdout)
	}
	owned = false
	f.env.OwnNewFiles = func() error { return errors.New("not elevated") }
	if code := f.run("", "rollback", "--json"); code == ExitOK || len(f.up.rollbackTo) != 0 {
		t.Fatalf("rollback without OwnNewFiles: exit %d to %v", code, f.up.rollbackTo)
	}
	if code := f.run("", "update", "--json"); code == ExitOK || len(f.up.appliedTo) != 1 {
		t.Fatalf("update without OwnNewFiles: exit %d applied to %v", code, f.up.appliedTo)
	}
}
