package checks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
)

var planForges = []string{"github", "forgejo", "gitea", "gitlab"}

// FuzzPlan: Plan never panics, and every job it returns can run as
// planned: a known OS, a timeout within (0, 6h], a name on the job and
// every step, and no ${{ }} left except the sha and ref placeholders
// RunBundle fills.
func FuzzPlan(f *testing.F) {
	files, _ := filepath.Glob(filepath.Join("testdata", "*.yml"))
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		forge := uint8(0)
		if strings.Contains(p, "gitlab") {
			forge = 3
		}
		f.Add(src, forge)
	}
	f.Add([]byte("on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    timeout-minutes: 1e300\n    steps:\n      - run: echo hi\n"), uint8(0))
	f.Add([]byte("on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    timeout-minutes: inf\n    steps:\n      - name: ''\n        run: echo hi\n"), uint8(0))
	f.Add([]byte("on: push\njobs:\n  a:\n    runs-on: ${{ matrix.os }}\n    strategy:\n      matrix:\n        os: [ubuntu-latest]\n        v: ['${{ secrets.X }}']\n    steps:\n      - run: echo ${{ matrix.v }}\n"), uint8(1))
	f.Add([]byte("a: &a [x, x, x, x, x, x, x, x]\nb: &b [*a, *a, *a, *a, *a, *a, *a, *a]\nc: &c [*b, *b, *b, *b, *b, *b, *b, *b]\non: push\njobs: {}\n"), uint8(0))
	f.Add([]byte("job:\n  script: echo $[[ inputs.x ]]\n  timeout: 99999999999 weeks\n"), uint8(3))
	f.Fuzz(func(t *testing.T, src []byte, forge uint8) {
		fg := planForges[int(forge)%len(planForges)]
		jobs, _, err := Plan(".github/workflows/x.yml", src, fg)
		if err != nil {
			return
		}
		for _, j := range jobs {
			checkPlannedJob(t, fg, j)
		}
	})
}

func checkPlannedJob(t *testing.T, forge string, j core.CheckJob) {
	t.Helper()
	switch j.Platform.OS {
	case "linux", "macos", "windows":
	default:
		t.Fatalf("job %q has platform %q", j.ID, j.Platform.OS)
	}
	if j.Timeout <= 0 || j.Timeout > maxTimeout {
		t.Fatalf("job %q has timeout %v, outside (0, %v]", j.ID, j.Timeout, maxTimeout)
	}
	if strings.TrimSpace(j.Name) == "" && strings.TrimSpace(j.ID) == "" {
		t.Fatalf("job has no name or id: %+v", j)
	}
	if len(j.Steps) == 0 {
		t.Fatalf("job %q has no steps", j.ID)
	}
	unresolved := func(what, s string) {
		if forge == "gitlab" {
			return // GitLab has no ${{ }}; the text is the script's own
		}
		s = strings.ReplaceAll(strings.ReplaceAll(s, shaExpr, ""), refExpr, "")
		if strings.Contains(s, "${{") {
			t.Fatalf("job %q: %s keeps an unresolved expression: %q", j.ID, what, s)
		}
	}
	unresolved("name", j.Name)
	for k, v := range j.Env {
		unresolved("env "+k, v)
	}
	for i, s := range j.Steps {
		if strings.TrimSpace(s.Name) == "" {
			t.Fatalf("job %q step %d has no name: %+v", j.ID, i+1, s)
		}
		unresolved("step name", s.Name)
		unresolved("step run", s.Run)
		unresolved("step shell", s.Shell)
		unresolved("step working-directory", s.WorkingDirectory)
		for k, v := range s.Env {
			unresolved("step env "+k, v)
		}
		for k, v := range s.Tool {
			unresolved("tool "+k, v)
		}
	}
}

// FuzzExtract builds hostile tarballs (escaping names, symlink chains,
// hard links, devices, FIFOs, duplicates) and checks that extract never
// writes outside the target, leaves no special file and no symlink that
// resolves outside it, whether it succeeds or not.
func FuzzExtract(f *testing.F) {
	for _, s := range []string{
		"\x01\x00\x00\x07\x00\x00\x02\x00", // a dir, a file under it
		// top/a/b/up -> ../.. then top/a/b/esc -> up/..: each target
		// looks local, but esc resolves above the target dir.
		"\x01\x02\x00\x02\x03\x00\x02\x03\x00\x02\x03\x08\x00\x04\x02\x04\x04\x00\x02\x03\x09\x00\x02\x08\x04",
		"\x02\x01\x00\x07\x00\x01\x04\x04",
		"\x04\x00\x00\x00", "\x05\x00\x00\x00", "\x06\x00\x00\x00", "\x0a\x00\x00", "\x41\x00\x00\x00",
		"\x03\x01\x00\x07\x00\x00\x01\x00\x0e",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, prog []byte) {
		a, dst, check := extractSandbox(t, hostileTar(prog))
		_ = extract(a, dst)
		check()
	})
}

// FuzzExtractGzip feeds extract arbitrary bytes: no panic, nothing outside.
func FuzzExtractGzip(f *testing.F) {
	f.Add([]byte{})
	f.Add(hostileTar([]byte("\x01\x00\x00\x07\x00\x00\x02\x00")))
	f.Add(hostileTar([]byte("\x02\x01\x00\x07\x00\x01\x04\x04")))
	f.Fuzz(func(t *testing.T, data []byte) {
		a, dst, check := extractSandbox(t, data)
		_ = extract(a, dst)
		check()
	})
}
