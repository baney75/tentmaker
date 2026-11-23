package cli

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baney75/tentmaker/internal/config"
	"github.com/baney75/tentmaker/internal/core"
	"github.com/baney75/tentmaker/internal/daemon"
)

// TestSetupTimings: setup reports each phase's wall time, executor phases
// as sandbox.<name>, and the phases add up to total_ms.
func TestSetupTimings(t *testing.T) {
	f := newFixture(t)
	f.serveControl()
	clock := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	f.env.Now = func() time.Time {
		clock = clock.Add(100 * time.Millisecond)
		return clock
	}
	f.env.InstallExecutors = func(ctx context.Context, cfg config.Config, bin func(core.Platform) (string, error), o InstallOptions) error {
		f.execInst++
		o.Phase("runner")
		o.Phase("podman")
		return nil
	}
	args := []string{"setup", "--yes", "--json", "--no-gh", "--no-mesh"}
	if code := f.run("", args...); code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, f.stdout, f.stderr)
	}
	var out struct {
		Timings []setupTiming `json:"timings"`
		TotalMS int64         `json:"total_ms"`
	}
	if err := json.Unmarshal(f.stdout.Bytes(), &out); err != nil {
		t.Fatalf("%v\n%s", err, f.stdout)
	}
	var phases []string
	var sum int64
	for _, p := range out.Timings {
		phases = append(phases, p.Phase)
		sum += p.MS
		if p.MS <= 0 {
			t.Errorf("phase %s took %d ms", p.Phase, p.MS)
		}
	}
	want := []string{"check", "wizard", "token", "save", "sandbox.preflight", "sandbox.runner", "sandbox.podman", "service.install", "service.wait", "doctor"}
	if !reflect.DeepEqual(phases, want) {
		t.Errorf("phases %v, want %v", phases, want)
	}
	if out.TotalMS == 0 || sum != out.TotalMS {
		t.Errorf("phases add up to %d ms, total_ms is %d", sum, out.TotalMS)
	}
}

// TestJSONOneLineOffATerminal: an agent reading a pipe gets one JSON line;
// a person at a terminal gets it indented.
func TestJSONOneLineOffATerminal(t *testing.T) {
	f := newFixture(t)
	f.run("", "version", "--json")
	if s := strings.TrimSuffix(f.stdout.String(), "\n"); s == "" || strings.Contains(s, "\n") {
		t.Errorf("not one line off a terminal: %q", f.stdout)
	}
	f.env.StdoutTTY = true
	f.run("", "version", "--json")
	if !strings.Contains(f.stdout.String(), "\n  \"") {
		t.Errorf("not indented on a terminal: %q", f.stdout)
	}
	// An installer's --json run on an agent's pty still gets one line.
	f.env.Getenv = func(k string) string { return map[string]string{"TENTMAKER_JSON_ONELINE": "1"}[k] }
	f.run("", "version", "--json")
	if s := strings.TrimSuffix(f.stdout.String(), "\n"); s == "" || strings.Contains(s, "\n") {
		t.Errorf("not one line on a pty with TENTMAKER_JSON_ONELINE: %q", f.stdout)
	}
}

// TestJSONErrorSaysNotOK: a caller that checks "ok" reads an error as one.
func TestJSONErrorSaysNotOK(t *testing.T) {
	f := newFixture(t)
	f.isRoot = false
	if code := f.run("", "setup", "--yes", "--json"); code == ExitOK {
		t.Fatalf("setup without root exited 0")
	}
	out := f.jsonObject([]string{"setup", "--yes", "--json"})
	if ok, has := out["ok"]; !has || ok != false || out["error"] == nil {
		t.Errorf("error object without \"ok\": false: %v", out)
	}
}

// TestFirstFix: the first failed check's fix comes before any warning's.
func TestFirstFix(t *testing.T) {
	r := daemon.Report{Checks: []daemon.Finding{
		{Check: "a", Status: daemon.OK},
		{Check: "b", Status: daemon.Warn, Fix: "warn fix"},
		{Check: "c", Status: daemon.Fail, Fix: "sudo tentmaker setup"},
		{Check: "d", Status: daemon.Fail, Fix: "later"},
	}}
	if got := firstFix(r); got != "sudo tentmaker setup" {
		t.Errorf("firstFix = %q", got)
	}
	r.Checks = r.Checks[:2]
	if got := firstFix(r); got != "warn fix" {
		t.Errorf("firstFix with only a warning = %q", got)
	}
	if got := firstFix(daemon.Report{}); got != "" {
		t.Errorf("firstFix of nothing = %q", got)
	}
}

// TestSetupNextNamesDoctorsFix: when doctor finds a problem, setup's next
// step is its first fix, not "add a repo".
func TestSetupNextNamesDoctorsFix(t *testing.T) {
	v := setupView{OK: true, Doctor: &daemon.Report{OK: false, Checks: []daemon.Finding{
		{Check: "podman", Status: daemon.Fail, What: "podman is missing", Fix: "sudo apt install podman"},
	}}}
	if got := setupNext(v, config.Config{}); !strings.HasPrefix(got, "sudo apt install podman") {
		t.Errorf("next = %q", got)
	}
	v = setupView{OK: false, Steps: []setupStep{{Step: "sandbox", Status: daemon.Fail, Detail: "podman is required"}}}
	if got := setupNext(v, config.Config{}); !strings.Contains(got, "sandbox step (podman is required)") {
		t.Errorf("next = %q", got)
	}
}
