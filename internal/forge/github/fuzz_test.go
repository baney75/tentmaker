package github

import (
	"fmt"
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
		"on: push\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: |\n          runs-on: ubuntu-latest\n",
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
			if why := sameButRunsOn(&in, &out, 0); why != "" {
				t.Fatalf("YAML outside runs-on values changed on %s: %s\nin:\n%s\nout:\n%s", platform, why, src, once)
			}
		}
	})
}

// sameButRunsOn compares two parsed workflows and returns what differs,
// or "" when they match. Values of runs-on keys are not compared, and a
// concurrency key that only the output has is skipped: those are the
// edits MigrateWorkflow makes. The line check above can't see a runs-on
// line inside a script or string, so this one reads what YAML reads.
func sameButRunsOn(a, b *yaml.Node, depth int) string {
	if depth > 64 {
		return ""
	}
	if a.Kind != b.Kind {
		return fmt.Sprintf("line %d: kind %v became %v", a.Line, a.Kind, b.Kind)
	}
	if a.Kind == yaml.ScalarNode || a.Kind == yaml.AliasNode {
		if a.Value != b.Value {
			return fmt.Sprintf("line %d: %q became %q", a.Line, a.Value, b.Value)
		}
		return ""
	}
	ac, bc := a.Content, b.Content
	if a.Kind == yaml.MappingNode && depth <= 2 && len(bc) == len(ac)+2 {
		// The added concurrency block: drop it from the output's top level.
		for k := 0; k+1 < len(bc); k += 2 {
			if bc[k].Value == "concurrency" && !hasKey(ac, "concurrency") {
				bc = append(append([]*yaml.Node{}, bc[:k]...), bc[k+2:]...)
				break
			}
		}
	}
	if len(ac) != len(bc) {
		return fmt.Sprintf("line %d: %d children became %d", a.Line, len(ac), len(bc))
	}
	for i := range ac {
		if a.Kind == yaml.MappingNode && i%2 == 1 && ac[i-1].Kind == yaml.ScalarNode && ac[i-1].Value == "runs-on" {
			continue
		}
		if why := sameButRunsOn(ac[i], bc[i], depth+1); why != "" {
			return why
		}
	}
	return ""
}

func hasKey(content []*yaml.Node, key string) bool {
	for k := 0; k+1 < len(content); k += 2 {
		if content[k].Value == key {
			return true
		}
	}
	return false
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
