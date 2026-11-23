package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const setupOut = `{"ok":true,"timings":[{"phase":"check","ms":100},{"phase":"sandbox.runner","ms":4000},{"phase":"service.wait","ms":900}],"total_ms":5000}`

func TestInstallReport(t *testing.T) {
	dir := t.TempDir()
	marks := filepath.Join(dir, "marks")
	// Connect ends at +20.4 s; the job goes pending at +24 and green at +35.
	os.WriteFile(marks, []byte("command begin 1795400000.000\ncommand end 1795400006.250\n"+
		"connect begin 1795400019.000\nconnect end 1795400020.400\n"), 0o644)
	setup := filepath.Join(dir, "setup.json")
	os.WriteFile(setup, []byte(setupOut), 0o644)
	st := filepath.Join(dir, "statuses.json")
	// Newest first, as the API lists them; an older run's success is ignored.
	os.WriteFile(st, []byte(`[
		{"state":"success","context":"tentmaker/smoke","created_at":"2026-11-22T00:00:00Z"},
		{"state":"success","context":"tentmaker/smoke","created_at":"`+at(1795400035)+`"},
		{"state":"success","context":"other","created_at":"`+at(1795400022)+`"},
		{"state":"pending","context":"tentmaker/smoke","created_at":"`+at(1795400024)+`"}
	]`), 0o644)
	js, md := filepath.Join(dir, "out.json"), filepath.Join(dir, "summary.md")
	var out, errb bytes.Buffer
	code := run([]string{"install", "-os", "linux", "-marks", marks, "-setup", setup, "-statuses", st, "-ctx", "tentmaker/smoke",
		"-budget-ms", "7000", "-json", js, "-md", md}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out.String(), errb.String())
	}
	var rep InstallReport
	b, _ := os.ReadFile(js)
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, p := range rep.Phases {
		got[p.Phase] = p.MS
	}
	want := map[string]int64{"download_verify": 1250, "setup.check": 100, "setup.sandbox.runner": 4000, "setup.service.wait": 900,
		"connect": 1400, "first_job_pickup": 3600, "first_job_green": 11000}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d ms, want %d (all: %v)", k, got[k], v, got)
		}
	}
	if rep.InstallMS != 6250 || rep.ToGreenMS != 6250+1400+3600+11000 || !rep.WithinBudget {
		t.Errorf("report %+v", rep)
	}
	if s, _ := os.ReadFile(md); !strings.Contains(string(s), "| setup.sandbox.runner | 4.0 |") {
		t.Errorf("summary:\n%s", s)
	}

	// Over budget: exit 1 and say so.
	out.Reset()
	code = run([]string{"install", "-os", "linux", "-marks", marks, "-setup", setup, "-budget-ms", "6000", "-json", js, "-md", md}, &out, &errb)
	if code != 1 || !strings.Contains(out.String(), "over the 6.0 s budget") {
		t.Errorf("over budget: exit %d\n%s", code, out.String())
	}
}

func TestInstallNeedsSetupTimings(t *testing.T) {
	m := map[string]*span{"command": {begin: 1, end: 7}}
	if _, err := BuildInstall("linux", m, []byte(`{"ok":true}`), nil, "", 0); err == nil {
		t.Error("setup output with no timings was accepted")
	}
	if _, err := BuildInstall("linux", map[string]*span{}, []byte(setupOut), nil, "", 0); err == nil {
		t.Error("no command marks was accepted")
	}
	short := map[string]*span{"command": {begin: 1, end: 2}}
	if _, err := BuildInstall("linux", short, []byte(setupOut), nil, "", 0); err == nil {
		t.Error("setup longer than the command was accepted")
	}
	if _, err := BuildInstall("linux", m, []byte(setupOut), []commitStatus{}, "c", 0); err == nil {
		t.Error("statuses with no connect marks was accepted")
	}
}

func at(sec int64) string { return time.Unix(sec, 0).UTC().Format(time.RFC3339) }
