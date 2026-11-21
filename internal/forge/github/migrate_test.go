package github

import (
	"strings"
	"testing"
)

func migrate(t *testing.T, src, platform string) (string, []string, bool) {
	t.Helper()
	out, report, changed, err := MigrateWorkflow([]byte(src), platform)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), report, changed
}

func hasLine(report []string, prefix, sub string) bool {
	for _, r := range report {
		if strings.HasPrefix(r, prefix) && strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

const linuxPool = `fromJSON('["self-hosted","tentmaker","linux"]')`

func TestMigrateRealisticCI(t *testing.T) {
	src := `name: CI
on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  lint:
    runs-on: ubuntu-latest   # fast
    steps:
      - uses: actions/checkout@v4
      - run: make lint
  test:
    name: Test (${{ matrix.os }})
    strategy:
      matrix:
        os: [ubuntu-latest, macos-14, windows-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - run: go test ./...
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
  release:
    needs: [lint, test]
    if: startsWith(github.ref, 'refs/tags/')
    runs-on: ubuntu-latest
    steps:
      - run: ./release.sh
  coverage:
    runs-on: ubuntu-22.04
    steps:
      - run: ./upload.sh
        env:
          CODECOV_TOKEN: ${{ secrets.CODECOV_TOKEN }}
  pages:
    runs-on: ubuntu-latest
    permissions:
      pages: write
      id-token: write
    steps:
      - run: echo hi
  e2e:
    runs-on: [ubuntu-24.04-arm]
    steps:
      - run: echo arm
  apt:
    runs-on: ubuntu-latest
    steps:
      - run: sudo apt-get install -y jq
`
	out, report, changed := migrate(t, src, "linux")
	if !changed {
		t.Fatal("nothing changed")
	}
	lines := strings.Split(out, "\n")
	find := func(prefix string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, prefix) {
				return l
			}
		}
		return ""
	}
	// lint: rewritten, comment kept.
	want := "    runs-on: ${{ github.event.pull_request.head.repo.fork && 'ubuntu-latest' || " + linuxPool + " }} # fast"
	if !strings.Contains(out, want+"\n") {
		t.Fatalf("lint not rewritten:\n%s", out)
	}
	// test: matrix leg for Linux only, GITHUB_TOKEN alone is not sensitive.
	wantMatrix := "    runs-on: ${{ matrix.os == 'ubuntu-latest' && !github.event.pull_request.head.repo.fork && " + linuxPool + " || matrix.os }}"
	if !strings.Contains(out, wantMatrix+"\n") {
		t.Fatalf("matrix not rewritten:\n%s", out)
	}
	// release, coverage (a secret), pages (write permission): left alone.
	for _, j := range []string{"release", "coverage", "pages"} {
		if !hasLine(report, "left", "job '"+j) {
			t.Errorf("job %s not reported as left: %v", j, report)
		}
	}
	if strings.Count(out, "runs-on: ubuntu-latest\n") != 3 || !strings.Contains(out, "runs-on: ubuntu-22.04\n") {
		t.Fatalf("sensitive or sudo jobs were rewritten:\n%s", out)
	}
	if !hasLine(report, "left", "ubuntu-24.04-arm") || !strings.Contains(out, "runs-on: [ubuntu-24.04-arm]\n") {
		t.Fatalf("ARM job: %v", report)
	}
	if !hasLine(report, "left", "job 'apt' uses sudo") {
		t.Fatalf("sudo job not reported: %v", report)
	}
	// Concurrency added before jobs:, once.
	if strings.Count(out, "\nconcurrency:\n") != 1 || !strings.Contains(out, "cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n\njobs:") {
		t.Fatalf("concurrency block:\n%s", out)
	}
	if !hasLine(report, "changed", "concurrency") {
		t.Fatalf("concurrency not reported: %v", report)
	}
	// Everything else is byte-identical.
	if find("  push:") == "" || !strings.Contains(out, "permissions:\n  contents: read\n") || !strings.HasSuffix(out, "sudo apt-get install -y jq\n") {
		t.Fatal("layout not preserved")
	}
}

func TestMigrateKeepsExistingConcurrency(t *testing.T) {
	src := `on: [push]
concurrency:
  group: mine
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - run: make
`
	out, report, changed := migrate(t, src, "linux")
	if !changed || strings.Count(out, "concurrency:") != 1 || !strings.Contains(out, "group: mine") {
		t.Fatalf("existing concurrency not kept:\n%s", out)
	}
	if hasLine(report, "changed", "concurrency") {
		t.Fatal("reported adding a concurrency block")
	}
}

func TestMigrateReusableWorkflowGetsNoConcurrency(t *testing.T) {
	src := `on:
  workflow_call:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: make
`
	out, report, _ := migrate(t, src, "linux")
	if strings.Contains(out, "concurrency:") || !hasLine(report, "left", "reusable") {
		t.Fatalf("reusable workflow:\n%s\n%v", out, report)
	}
	if !strings.Contains(out, linuxPool) {
		t.Fatal("job not migrated")
	}
}

func TestMigrateWorkflowLevelSecretsLeaveEverything(t *testing.T) {
	src := `on: push
env:
  API_KEY: ${{ secrets.API_KEY }}
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - run: make
`
	out, report, changed := migrate(t, src, "linux")
	if changed || out != src || !hasLine(report, "left", "job 'build'") {
		t.Fatalf("workflow-level secret: changed=%v\n%v", changed, report)
	}
}

func TestMigrateEnvironmentAndWriteAll(t *testing.T) {
	src := `on: push
jobs:
  deployish:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - run: make
  all:
    runs-on: ubuntu-latest
    permissions: write-all
    steps:
      - run: make
  indexed:
    runs-on: ubuntu-latest
    steps:
      - run: echo ${{ secrets['NPM_TOKEN'] }}
`
	out, _, changed := migrate(t, src, "linux")
	if changed || out != src {
		t.Fatalf("sensitive jobs rewritten:\n%s", out)
	}
}

func TestMigrateBlockListAndCRLF(t *testing.T) {
	src := "on: push\r\njobs:\r\n  build:\r\n    runs-on:\r\n      - ubuntu-latest\r\n    steps:\r\n      - run: make\r\n"
	out, _, changed := migrate(t, src, "linux")
	if !changed {
		t.Fatal("block list not migrated")
	}
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Fatalf("line endings not preserved: %q", out)
	}
	if strings.Contains(out, "- ubuntu-latest") || !strings.Contains(out, "    runs-on: ${{ github.event.pull_request.head.repo.fork && 'ubuntu-latest' || "+linuxPool+" }}\r\n    steps:") {
		t.Fatalf("got %q", out)
	}
}

func TestMigrateMacOS(t *testing.T) {
	src := `on: pull_request
jobs:
  app:
    runs-on: macos-15
    steps:
      - run: sudo xcode-select -s /Applications/Xcode.app
      - run: xcodebuild test
  intel:
    runs-on: macos-13
    steps:
      - run: make
  ui:
    runs-on: macos-14
    steps:
      - run: open -a MyApp
  linux:
    runs-on: ubuntu-latest
    steps:
      - run: make
`
	out, report, changed := migrate(t, src, "macos")
	if !changed || !strings.Contains(out, `'macos-15' || fromJSON('["self-hosted","tentmaker","macOS"]')`) {
		t.Fatalf("macos-15 job not migrated (sudo is fine in a VM):\n%s", out)
	}
	if !hasLine(report, "left", "macos-13") || !strings.Contains(out, "runs-on: macos-13\n") {
		t.Fatalf("Intel image: %v", report)
	}
	if !hasLine(report, "left", "job 'ui' launches a GUI app") {
		t.Fatalf("GUI job: %v", report)
	}
	if !strings.Contains(out, "runs-on: ubuntu-latest\n") || hasLine(report, "left", "ubuntu") {
		t.Fatalf("Linux job touched or reported on a macOS migration: %v", report)
	}
}

func TestMigrateWindowsMatrix(t *testing.T) {
	src := `on: push
jobs:
  test:
    strategy:
      matrix:
        include:
          - runner: windows-2022
          - runner: windows-latest
          - runner: ubuntu-latest
    runs-on: ${{ matrix.runner }}
    steps:
      - run: make
`
	out, _, changed := migrate(t, src, "windows")
	want := `runs-on: ${{ (matrix.runner == 'windows-2022' || matrix.runner == 'windows-latest') && !github.event.pull_request.head.repo.fork && fromJSON('["self-hosted","tentmaker","Windows"]') || matrix.runner }}`
	if !changed || !strings.Contains(out, want) {
		t.Fatalf("got:\n%s", out)
	}
}

func TestMigrateComplexExpressionLeft(t *testing.T) {
	src := `on: push
jobs:
  test:
    strategy:
      matrix:
        os: [ubuntu-latest]
    runs-on: ${{ matrix.os == 'x' && 'big' || matrix.os }}
    steps:
      - run: make
`
	out, report, changed := migrate(t, src, "linux")
	if changed || out != src || !hasLine(report, "left", "check it by hand") {
		t.Fatalf("complex expression: %v\n%s", report, out)
	}
}

func TestMigrateNothingToDo(t *testing.T) {
	src := "on: push\njobs:\n  b:\n    runs-on: [self-hosted, gpu]\n    steps:\n      - run: make\n"
	out, report, changed := migrate(t, src, "linux")
	if changed || out != src || len(report) != 0 {
		t.Fatalf("changed=%v report=%v", changed, report)
	}
	if _, _, _, err := MigrateWorkflow([]byte(src), "beos"); err == nil {
		t.Fatal("accepted an unknown platform")
	}
	if out, _, changed, err := MigrateWorkflow(nil, "linux"); err != nil || changed || len(out) != 0 {
		t.Fatal("empty input")
	}
}

func TestMigrateSudoSkippedOnOtherOS(t *testing.T) {
	src := `on: push
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - name: mac only
        if: runner.os == 'macOS'
        run: sudo true
      - run: make
`
	out, _, changed := migrate(t, src, "linux")
	if !changed || !strings.Contains(out, linuxPool) {
		t.Fatalf("a sudo step that never runs on Linux blocked the move:\n%s", out)
	}
}
