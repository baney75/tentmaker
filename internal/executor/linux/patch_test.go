//go:build !windows

package linux

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// syntheticDLL looks like Runner.Worker.dll where the patch cares: each
// name once in UTF-16, between other heap strings.
func syntheticDLL(names ...string) []byte {
	var b bytes.Buffer
	b.WriteString("MZ\x90\x00 fake PE header ")
	for _, n := range append([]string{"ACTIONS_RUNTIME_URL", "ACTIONS_CACHE_SERVICE_V2"}, names...) {
		b.WriteByte(byte(2*len(n) + 1)) // #US length prefix
		b.Write(utf16le(n))
		b.WriteByte(0)
	}
	b.WriteString(" trailer")
	return b.Bytes()
}

func TestPatchWorkerDLL(t *testing.T) {
	orig := syntheticDLL("ACTIONS_RESULTS_URL", "ACTIONS_CACHE_URL")
	got, err := PatchWorkerDLL(orig)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(orig) {
		t.Fatalf("length %d, want %d", len(got), len(orig))
	}
	for name, want := range map[string]int{
		"ACTIONS_RESULTS_URL": 0, "ACTIONS_CACHE_URL": 0,
		"ACTIONS_RESULTS_ORL": 1, "ACTIONS_CACHE_ORL": 1,
		"ACTIONS_RUNTIME_URL": 1, "ACTIONS_CACHE_SERVICE_V2": 1,
	} {
		if n := bytes.Count(got, utf16le(name)); n != want {
			t.Errorf("%s appears %d times, want %d", name, n, want)
		}
	}
	if !bytes.Equal(syntheticDLL("ACTIONS_RESULTS_URL", "ACTIONS_CACHE_URL"), orig) {
		t.Fatal("the input was changed")
	}
	// Exactly the renamed bytes differ.
	diff := 0
	for i := range orig {
		if orig[i] != got[i] {
			diff++
		}
	}
	if diff != 2 { // 'U' -> 'O' in each name
		t.Fatalf("%d bytes changed, want 2", diff)
	}

	for name, dll := range map[string][]byte{
		"missing":         syntheticDLL("ACTIONS_RESULTS_URL"),
		"twice":           syntheticDLL("ACTIONS_RESULTS_URL", "ACTIONS_CACHE_URL", "ACTIONS_CACHE_URL"),
		"already patched": got,
		"half patched":    syntheticDLL("ACTIONS_RESULTS_URL", "ACTIONS_CACHE_URL", "ACTIONS_RESULTS_ORL"),
		"ascii only":      []byte("ACTIONS_RESULTS_URL ACTIONS_CACHE_URL"),
	} {
		if _, err := PatchWorkerDLL(dll); err == nil {
			t.Errorf("%s: patched, want a refusal", name)
		}
	}
}

func TestWorkerDLLPinsCoverPinnedRunner(t *testing.T) {
	for _, arch := range []string{"x64", "arm64"} {
		p, _ := PinFor(arch)
		d, ok := workerDLLPins[p.Version+"-"+p.Arch]
		if !ok || len(d.Original) != 64 || len(d.Patched) != 64 || d.Original == d.Patched {
			t.Errorf("runner %s %s has no usable Runner.Worker.dll pin: %+v", p.Version, arch, d)
		}
	}
}

// withDLL puts a synthetic Runner.Worker.dll in the installed template and
// pins it (pinOK false pins a hash that doesn't match).
func withDLL(t *testing.T, v *env, pinOK bool) []byte {
	t.Helper()
	orig := syntheticDLL("ACTIONS_RESULTS_URL", "ACTIONS_CACHE_URL")
	tpl, err := filepath.EvalSymlinks(v.e.template())
	must(t, err)
	must(t, os.WriteFile(filepath.Join(tpl, "bin", "Runner.Worker.dll"), orig, 0o644))
	patched, err := PatchWorkerDLL(orig)
	must(t, err)
	pin := &DLLPin{Original: sha256Hex(orig), Patched: sha256Hex(patched)}
	if !pinOK {
		pin.Patched = strings.Repeat("0", 64)
	}
	v.e.o.WorkerDLL = pin
	return orig
}

func TestStartRunnerUsesActionsCache(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	orig := withDLL(t, v, true)
	if !v.e.ActionsCacheReady() {
		t.Fatalf("not ready; logs:\n%s", v.logs)
	}
	s := spec("job-abc", "")
	s.ActionsCache = "http://127.0.0.1:7436/"
	must(t, v.e.Start(context.Background(), s))

	jd := v.e.jobDir("job-abc")
	got, err := os.ReadFile(filepath.Join(jd, "runner", "bin", "Runner.Worker.dll"))
	must(t, err)
	if bytes.Count(got, utf16le("ACTIONS_RESULTS_ORL")) != 1 || bytes.Count(got, utf16le("ACTIONS_CACHE_ORL")) != 1 {
		t.Fatal("the job's Runner.Worker.dll is not the patched one")
	}
	tpl, _ := filepath.EvalSymlinks(v.e.template())
	if cur, _ := os.ReadFile(filepath.Join(tpl, "bin", "Runner.Worker.dll")); !bytes.Equal(cur, orig) {
		t.Fatal("the template's Runner.Worker.dll changed")
	}
	env, _ := os.ReadFile(filepath.Join(jd, "runner", ".env"))
	for _, l := range []string{"ACTIONS_RESULTS_URL=http://127.0.0.1:7436/\n", "ACTIONS_CACHE_URL=http://127.0.0.1:7436/\n", "ACTIONS_CACHE_SERVICE_V2=True\n"} {
		if !strings.Contains(string(env), l) {
			t.Errorf(".env lacks %q:\n%s", l, env)
		}
	}
	// Never on the unit's command line.
	if args := strings.Join(findUnit(t, v.f, unitName("job-abc")).Args, " "); strings.Contains(args, "ACTIONS_") {
		t.Errorf("cache variables in argv: %s", args)
	}
	// The patched file is root's and never chowned to the runner.
	if c, ok := v.f.chowns[filepath.Join(jd, "runner", "bin", "Runner.Worker.dll")]; ok && c != [2]int{0, 0} {
		t.Errorf("the patched dll was chowned to %v", c)
	}
	shim, err := os.ReadFile(filepath.Join(v.e.shimDir(), "docker"))
	if err != nil || !strings.Contains(string(shim), "ACTIONS_RESULTS") {
		t.Fatalf("the docker shim was not brought up to date: %v", err)
	}

	// A job without the cache keeps the original.
	must(t, v.e.Start(context.Background(), spec("job-plain", "")))
	plain, _ := os.ReadFile(filepath.Join(v.e.jobDir("job-plain"), "runner", "bin", "Runner.Worker.dll"))
	if !bytes.Equal(plain, orig) {
		t.Fatal("a job without the cache got the patched dll")
	}
	if env, _ := os.ReadFile(filepath.Join(v.e.jobDir("job-plain"), "runner", ".env")); strings.Contains(string(env), "ACTIONS_") {
		t.Fatalf("a job without the cache has cache variables:\n%s", env)
	}
}

func TestActionsCacheFallsBackOnPinMismatch(t *testing.T) {
	v := newEnv(t)
	v.installed(t)
	orig := withDLL(t, v, false)
	if v.e.ActionsCacheReady() {
		t.Fatal("ready with a mismatched pin")
	}
	for _, n := range []string{"job-a", "job-b"} {
		s := spec(n, "")
		s.ActionsCache = "http://127.0.0.1:7436/"
		must(t, v.e.Start(context.Background(), s))
		got, _ := os.ReadFile(filepath.Join(v.e.jobDir(n), "runner", "bin", "Runner.Worker.dll"))
		if !bytes.Equal(got, orig) {
			t.Fatalf("%s: not the original dll", n)
		}
		if env, _ := os.ReadFile(filepath.Join(v.e.jobDir(n), "runner", ".env")); strings.Contains(string(env), "ACTIONS_") {
			t.Fatalf("%s: cache variables without the patch:\n%s", n, env)
		}
	}
	if n := strings.Count(v.logs.String(), "jobs use GitHub's Actions cache"); n != 1 {
		t.Fatalf("logged the fallback %d times, want once:\n%s", n, v.logs)
	}
	if _, err := os.Stat(v.e.patchedDLL(Pin{Version: RunnerVersion, Arch: "x64"})); err == nil {
		t.Fatal("a patched dll that failed its pin was written")
	}
}

func TestActionsCacheURLChecked(t *testing.T) {
	for _, bad := range []string{
		"http://10.0.0.1:7436/", "https://127.0.0.1:7436/", "http://127.0.0.1:7436/\nEVIL=1",
		"http://user@127.0.0.1:7436/", "http://localhost:7436/", "http://127.0.0.1:7436/?x=1",
	} {
		if _, err := cacheURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got, err := cacheURL("http://127.0.0.1:7436"); err != nil || got != "http://127.0.0.1:7436/" {
		t.Fatalf("good URL: %q %v", got, err)
	}
	v := newEnv(t)
	v.installed(t)
	withDLL(t, v, true)
	s := spec("job-abc", "")
	s.ActionsCache = "http://192.0.2.1:7436/"
	if err := v.e.Start(context.Background(), s); err == nil {
		t.Fatal("a job with a non-loopback cache URL started")
	}
}

func TestDockerShim(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real docker")
	must(t, os.WriteFile(real, []byte("#!/bin/bash\nprintf '%s\\n' \"$@\"\necho \"R=${ACTIONS_RESULTS_URL:-}\"\necho \"C=${ACTIONS_CACHE_URL:-}\"\n"), 0o755))
	shim := filepath.Join(dir, "docker")
	must(t, os.WriteFile(shim, []byte(dockerShim(real)), 0o755))
	run := func(env []string, args ...string) string {
		t.Helper()
		c := exec.Command(bash, append([]string{shim}, args...)...)
		c.Env = append([]string{"PATH=/usr/bin:/bin"}, env...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	gh := []string{
		"ACTIONS_RESULTS_ORL=https://results-receiver.actions.githubusercontent.com/",
		"ACTIONS_CACHE_ORL=https://acghub.actions.githubusercontent.com/x/",
		"ACTIONS_RESULTS_URL=http://127.0.0.1:7436/",
		"ACTIONS_CACHE_URL=http://127.0.0.1:7436/",
	}
	for _, c := range []struct {
		name string
		env  []string
		args []string
		want string
	}{
		{"container step", gh, []string{"create", "-e", "ACTIONS_RESULTS_ORL", "-e", "ACTIONS_CACHE_ORL", "img"},
			"create\n--pids-limit\n4096\n-e\nACTIONS_RESULTS_URL\n-e\nACTIONS_CACHE_URL\n-e\nACTIONS_RESULTS_ORL\n-e\nACTIONS_CACHE_ORL\nimg\n" +
				"R=https://results-receiver.actions.githubusercontent.com/\nC=https://acghub.actions.githubusercontent.com/x/\n"},
		{"exec in a job container", gh, []string{"exec", "--env=ACTIONS_RESULTS_ORL", "ctr", "sh"},
			"exec\n-e\nACTIONS_RESULTS_URL\n--env=ACTIONS_RESULTS_ORL\nctr\nsh\n" +
				"R=https://results-receiver.actions.githubusercontent.com/\nC=http://127.0.0.1:7436/\n"},
		{"container run with its own pids limit", gh, []string{"container", "run", "--pids-limit=10", "img"},
			"container\nrun\n--pids-limit=10\nimg\nR=http://127.0.0.1:7436/\nC=http://127.0.0.1:7436/\n"},
		{"no ORL variable set", nil, []string{"run", "-e", "ACTIONS_RESULTS_ORL", "img"},
			"run\n--pids-limit\n4096\n-e\nACTIONS_RESULTS_ORL\nimg\nR=\nC=\n"},
		{"other commands", gh, []string{"ps", "-a"},
			"ps\n-a\nR=http://127.0.0.1:7436/\nC=http://127.0.0.1:7436/\n"},
	} {
		if got := run(c.env, c.args...); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
