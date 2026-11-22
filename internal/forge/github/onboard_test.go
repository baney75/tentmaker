package github

import (
	"context"
	"strings"
	"testing"

	"github.com/baney75/tentmaker/internal/core"
)

func TestMigrateLeavesJobsThePlatformCantRun(t *testing.T) {
	src := `on: push
jobs:
  db:
    runs-on: macos-latest
    services:
      postgres:
        image: postgres:16
    steps:
      - run: make test
  plain:
    runs-on: macos-latest
    steps:
      - run: make
`
	out, report, changed := migrate(t, src, "macos")
	if !hasLine(report, "left line 4", "services:") || !hasLine(report, "left line 4", "macOS") {
		t.Fatalf("services on macOS not reported: %v", report)
	}
	if !strings.Contains(out, "    runs-on: macos-latest\n    services:") || !changed {
		t.Fatalf("db job moved or plain job not moved:\n%s", out)
	}
	// The same services job moves on Linux, where podman serves services.
	linuxSrc := strings.ReplaceAll(src, "macos-latest", "ubuntu-latest")
	out, report, _ = migrate(t, linuxSrc, "linux")
	if strings.Count(out, linuxPool) != 2 {
		t.Fatalf("linux services job not moved: %v\n%s", report, out)
	}

	win := `on: push
jobs:
  svc:
    runs-on: windows-latest
    services:
      redis:
        image: redis
    steps:
      - run: make
  box:
    runs-on: windows-2022
    container: mcr.microsoft.com/windows/servercore:ltsc2022
    steps:
      - run: make
  steps-only:
    runs-on: windows-latest
    steps:
      - name: container
        run: echo "a step named container: is not a job container"
`
	out, report, _ = migrate(t, win, "windows")
	if !hasLine(report, "left line 4", "services:") || !hasLine(report, "left line 11", "container:") {
		t.Fatalf("windows services/container not reported: %v", report)
	}
	if strings.Count(out, `fromJSON('["self-hosted","tentmaker","Windows"]')`) != 1 {
		t.Fatalf("want only the steps-only job moved: %v\n%s", report, out)
	}

	for _, label := range []string{"ubuntu-latest-8-cores", "ubuntu-22.04-16core", "gpu-t4-4-core", "macos-14-xlarge", "nvidia-a10"} {
		src := "on: push\njobs:\n  big:\n    runs-on: " + label + "\n    steps:\n      - run: make\n"
		out, report, changed := migrate(t, src, map[bool]string{true: "macos", false: "linux"}[strings.HasPrefix(label, "macos")])
		if changed || out != src || !hasLine(report, "left line 4", "GPU or larger-runner") {
			t.Fatalf("%s: changed=%v report=%v", label, changed, report)
		}
	}
}

func TestMigrateAllSkipsTheForkExpression(t *testing.T) {
	src := `on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
  matrix:
    strategy:
      matrix:
        os: [ubuntu-latest, macos-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - run: make
`
	safe, err := Migrate([]byte(src), "linux", MigrateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(safe.Moves) != 2 || safe.Moves[0].Job != "test" || safe.Moves[0].Before != "ubuntu-latest" ||
		!strings.Contains(safe.Moves[0].After, "head.repo.fork") || safe.Moves[1].Before != "${{ matrix.os }}" {
		t.Fatalf("moves: %+v", safe.Moves)
	}
	all, err := Migrate([]byte(src), "linux", MigrateOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(all.Out), "fork") {
		t.Fatalf("--all kept a fork expression:\n%s", all.Out)
	}
	if !strings.Contains(string(all.Out), "    runs-on: [self-hosted, tentmaker, linux]\n") {
		t.Fatalf("--all list missing:\n%s", all.Out)
	}
	if !strings.Contains(string(all.Out), "${{ matrix.os == 'ubuntu-latest' && "+linuxPool+" || matrix.os }}") {
		t.Fatalf("--all matrix:\n%s", all.Out)
	}
	if !hasLine(all.Report, "changed line 4", "--all") {
		t.Fatalf("report: %v", all.Report)
	}
}

const pullsQuery = "/repos/o/r/pulls?base=main&head=o%3Atentmaker%2Fmigrate&per_page=5&state=open"

func TestOpenOrUpdatePR(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", "/repos/o/r", 200, map[string]any{"full_name": "o/r", "default_branch": "main"})
	h.f.json("GET", pullsQuery, 200, []any{})
	h.f.json("POST", "/repos/o/r/pulls", 201, map[string]any{"number": 7, "html_url": "https://github.com/o/r/pull/7"})
	pr := core.PullRequest{Repo: "o/r", Head: "tentmaker/migrate", Title: "t", Body: "b"}
	res, err := h.c.OpenOrUpdatePR(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Number != 7 || res.URL != "https://github.com/o/r/pull/7" {
		t.Fatalf("create: %+v", res)
	}
	b := h.f.body("POST", "/repos/o/r/pulls")
	if b["head"] != "tentmaker/migrate" || b["base"] != "main" || b["title"] != "t" || b["body"] != "b" {
		t.Fatalf("create body: %v", b)
	}

	// Now one is open: the same call updates it.
	h.f.json("GET", pullsQuery, 200, []any{map[string]any{"number": 7, "html_url": "https://github.com/o/r/pull/7"}})
	h.f.json("PATCH", "/repos/o/r/pulls/7", 200, map[string]any{"number": 7, "html_url": "https://github.com/o/r/pull/7"})
	pr.Body = "b2"
	res, err = h.c.OpenOrUpdatePR(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if res.Created || res.Number != 7 {
		t.Fatalf("update: %+v", res)
	}
	if h.f.body("PATCH", "/repos/o/r/pulls/7")["body"] != "b2" || h.f.count("POST", "/repos/o/r/pulls") != 1 {
		t.Fatal("update did not patch, or posted a second PR")
	}

	if _, err := h.c.OpenOrUpdatePR(context.Background(), core.PullRequest{Repo: "o/r", Head: "-x", Base: "main"}); err == nil {
		t.Fatal("accepted a bad head")
	}
	h.f.json("GET", strings.Replace(pullsQuery, "base=main", "base=dev", 1), 200, []any{})
	h.f.json("POST", "/repos/o/r/pulls", 422, map[string]any{"message": "Validation Failed"})
	_, err = h.c.OpenOrUpdatePR(context.Background(), core.PullRequest{Repo: "o/r", Head: "tentmaker/migrate", Base: "dev"})
	if err == nil || !strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), leakMarker) {
		t.Fatalf("422: %v", err)
	}
}

func TestFirstJob(t *testing.T) {
	h := newHarness(t)
	h.f.json("GET", "/repos/o/r/actions/runners?per_page=100", 200, map[string]any{"runners": []any{
		map[string]any{"name": "tm-box-1", "status": "offline", "labels": []any{map[string]any{"name": "tentmaker"}}},
		map[string]any{"name": "other", "status": "online", "labels": []any{map[string]any{"name": "self-hosted"}}},
	}})
	h.f.json("GET", "/repos/o/r/actions/runs?per_page=5", 200, map[string]any{"workflow_runs": []any{
		map[string]any{"id": 2, "created_at": "2026-11-21T12:00:00Z"},
		map[string]any{"id": 1, "created_at": "2026-11-20T12:00:00Z"},
	}})
	h.f.json("GET", "/repos/o/r/actions/runs/2/jobs?per_page=100", 200, map[string]any{"jobs": []any{
		map[string]any{"name": "test", "status": "queued", "labels": []any{"self-hosted", "tentmaker", "linux"}, "created_at": "2026-11-21T12:00:00Z"},
		map[string]any{"name": "lint", "status": "queued", "labels": []any{"ubuntu-latest"}, "created_at": "2026-11-21T11:00:00Z"},
	}})
	h.f.json("GET", "/repos/o/r/actions/runs/1/jobs?per_page=100", 200, map[string]any{"jobs": []any{
		map[string]any{"name": "test", "status": "completed", "labels": []any{"self-hosted", "tentmaker", "linux"}, "runner_name": "tm-box-9", "created_at": "2026-11-20T12:00:00Z"},
	}})
	fj, err := h.c.FirstJob(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(fj.Online) != 0 || fj.RanOn != "tm-box-9" || fj.RanJob != "test" || fj.Queued != 1 || fj.OldestQueued.Day() != 21 {
		t.Fatalf("%+v", fj)
	}
}
