package cli

import (
	"os"
	"path/filepath"
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
	if st, _ := os.Stat(want); st.Mode().Perm() != 0o755 {
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
