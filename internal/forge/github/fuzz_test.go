package github

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var migratePlatforms = []string{"linux", "macos", "windows"}

// FuzzMigrateWorkflow: migrating is idempotent, touches nothing but
// runs-on lines (a block list collapses into its runs-on line) and the
// added concurrency block, and keeps valid YAML valid.
func FuzzMigrateWorkflow(f *testing.F) {
	var files []string
	for _, g := range []string{"../../checks/testdata/*.yml", "../../../.github/workflows/*.yml"} {
		m, _ := filepath.Glob(g)
		files = append(files, m...)
	}
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		for i := range migratePlatforms {
			f.Add(src, uint8(i))
		}
	}
	for _, s := range []string{
		"on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n",
		"on: push\njobs:\n  a:\n    runs-on:\n      - ubuntu-latest\n    steps:\n      - run: echo\r\n",
		"on: push\njobs: {a: {\n    runs-on: ubuntu-latest\n  }}\n",
		"on: push\njobs:\n  a: {steps: [], runs-on: [\n    ubuntu-latest\n    ]}\n",
		"on: push\njobs:\n  a:\n    runs-on: ${{ matrix.os }}\n    strategy:\n      matrix:\n        os: [ubuntu-latest, windows-2022, macos-15]\n    steps:\n      - run: echo\n",
		"on: push\nx: |\n  jobs:\n    runs-on: ubuntu-latest\njobs:\n  a:\n    runs-on: ubuntu-latest\n",
		"jobs:\n  a:\n    runs-on: \"ubuntu-latest\" # c\n",
		"on: [push]\njobs:\n  b:\n    runs-on: 'ubuntu-24.04'\n    steps:\n      - run: sudo apt-get install x\n",
	} {
		for i := range migratePlatforms {
			f.Add([]byte(s), uint8(i))
		}
	}
	f.Fuzz(func(t *testing.T, src []byte, which uint8) {
		platform := migratePlatforms[int(which)%len(migratePlatforms)]
		once, _, changed, err := MigrateWorkflow(src, platform)
		if err != nil {
			t.Fatalf("MigrateWorkflow: %v", err)
		}
		if changed != (string(once) != string(src)) {
			t.Fatalf("changed = %v, but the output %s", changed, map[bool]string{true: "differs", false: "is the same"}[string(once) != string(src)])
		}
		twice, _, changed2, err := MigrateWorkflow(once, platform)
		if err != nil {
			t.Fatalf("second MigrateWorkflow: %v", err)
		}
		if string(twice) != string(once) || changed2 {
			t.Fatalf("not idempotent on %s\nonce:\n%s\ntwice:\n%s", platform, once, twice)
		}
		if !changed {
			return
		}
		if a, b := untouchedLines(string(src), false), untouchedLines(string(once), true); a != b {
			t.Fatalf("lines other than runs-on changed on %s\nin:\n%s\nout:\n%s", platform, src, once)
		}
		var in, out yaml.Node
		if yaml.Unmarshal(src, &in) == nil {
			if err := yaml.Unmarshal(once, &out); err != nil {
				t.Fatalf("valid YAML became invalid on %s: %v\nin:\n%s\nout:\n%s", platform, err, src, once)
			}
		}
	})
}

// untouchedLines drops runs-on lines, the block-list items under them,
// and (in the output) the added concurrency block, and returns the rest.
func untouchedLines(s string, output bool) string {
	if output {
		if i := strings.Index(s, strings.ReplaceAll(concurrencyBlock, "\n", "\r\n")); i >= 0 {
			s = s[:i] + s[i+len(concurrencyBlock)+strings.Count(concurrencyBlock, "\n"):]
		} else if i := strings.Index(s, concurrencyBlock); i >= 0 {
			s = s[:i] + s[i+len(concurrencyBlock):]
		}
	}
	var keep []string
	lines := splitLines(s)
	for i := 0; i < len(lines); i++ {
		l, _ := chomp(lines[i])
		m := runsOnRe.FindStringSubmatch(l)
		if m == nil {
			keep = append(keep, lines[i])
			continue
		}
		if m[2] == "" {
			for i+1 < len(lines) {
				n, _ := chomp(lines[i+1])
				im := itemRe.FindStringSubmatch(n)
				if im == nil || len(im[1]) <= len(m[1]) {
					break
				}
				i++
			}
		}
	}
	return strings.Join(keep, "")
}
