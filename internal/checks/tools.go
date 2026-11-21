package checks

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// toolBins names the binary that reports each tool's version.
var toolBins = map[string]string{
	"go": "go", "node": "node", "python": "python3", "java": "java",
	"zig": "zig", "nu": "nu", "rust": "rustc",
}

var versionArgs = map[string][]string{
	"go": {"version"}, "zig": {"version"}, "java": {"-version"},
}

var (
	versionRe      = regexp.MustCompile(`\d+(?:\.\d+)+`)
	leadingVersion = regexp.MustCompile(`^\d+(?:\.\d+)*`)
)

// want is a parsed version spec from a setup action.
type want struct {
	any     bool   // any version will do
	series  string // "1.25" matches 1.25 and 1.25.x
	min     string // at least this version
	channel string // rust: "stable", "beta" or "nightly"
	desc    string // for messages: "1.25.x", ">= 1.22", "any version"
}

func (w want) fits(version, output string) bool {
	if w.channel != "" {
		isNightly := strings.Contains(output, "nightly")
		isBeta := strings.Contains(output, "beta")
		switch w.channel {
		case "nightly":
			return isNightly
		case "beta":
			return isBeta
		default:
			if isNightly || isBeta {
				return false
			}
		}
	}
	switch {
	case w.any || w.channel != "":
		return version != ""
	case w.min != "":
		return version != "" && compareVersions(version, w.min) >= 0
	default:
		return version == w.series || strings.HasPrefix(version, w.series+".")
	}
}

// parseSpec reads a setup action's version: "1.25", "1.25.x", "^20",
// "~3.12", ">=1.22", "stable", "lts/*", "file:go.mod".
func parseSpec(tool, spec, src string) (want, error) {
	spec = strings.TrimSpace(spec)
	if rest, ok := strings.CutPrefix(spec, "file:"); ok {
		v, err := versionFromFile(tool, src, rest)
		if err != nil {
			return want{}, err
		}
		w, err := parseSpec(tool, v.spec, src)
		if err == nil && v.minimum && w.series != "" {
			w = want{min: w.series, desc: ">= " + w.series}
		}
		return w, err
	}
	lower := strings.ToLower(spec)
	if tool == "rust" {
		for _, ch := range []string{"stable", "beta", "nightly"} {
			if lower == ch || strings.HasPrefix(lower, ch+"-") {
				return want{channel: ch, desc: ch}, nil
			}
		}
	}
	switch lower {
	case "", "latest", "stable", "lts", "lts/*", "node", "current", "*", "x":
		return want{any: true, desc: "any version"}, nil
	}
	if len(spec) > 1 && (spec[0] == 'v' || spec[0] == 'V') && spec[1] >= '0' && spec[1] <= '9' {
		spec = spec[1:]
	}
	if rest, ok := strings.CutPrefix(spec, ">="); ok {
		v := strings.TrimSpace(rest)
		if !versionRe.MatchString(v) && !isDigits(v) {
			return want{}, fmt.Errorf("can't read the version %q", spec)
		}
		return want{min: v, desc: ">= " + v}, nil
	}
	keep := 0
	switch {
	case strings.HasPrefix(spec, "^"):
		spec, keep = spec[1:], 1
	case strings.HasPrefix(spec, "~"):
		spec, keep = spec[1:], 2
	}
	for strings.HasSuffix(spec, ".x") || strings.HasSuffix(spec, ".*") {
		spec = spec[:len(spec)-2]
	}
	for _, p := range strings.Split(spec, ".") {
		if !isDigits(p) {
			return want{}, fmt.Errorf("can't read the version %q", spec)
		}
	}
	if keep > 0 {
		parts := strings.Split(spec, ".")
		if len(parts) > keep {
			spec = strings.Join(parts[:keep], ".")
		}
	}
	desc := spec
	if strings.Count(spec, ".") < 2 {
		desc = spec + ".x"
	}
	return want{series: spec, desc: desc}, nil
}

type fileVersion struct {
	spec    string
	minimum bool
}

// versionFromFile reads go.mod's go or toolchain line, or the first version
// in a file like .nvmrc, .python-version or .tool-versions.
func versionFromFile(tool, src, rel string) (fileVersion, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fileVersion{}, fmt.Errorf("the version file %s is outside the repo", rel)
	}
	data, err := os.ReadFile(filepath.Join(src, clean))
	if err != nil {
		return fileVersion{}, fmt.Errorf("can't read the version file %s", rel)
	}
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	if tool == "go" && filepath.Base(clean) == "go.mod" {
		goLine, toolchain := "", ""
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) >= 2 && f[0] == "go" {
				goLine = f[1]
			}
			if len(f) >= 2 && f[0] == "toolchain" {
				toolchain = strings.TrimPrefix(f[1], "go")
			}
		}
		v := goLine
		if toolchain != "" {
			v = toolchain
		}
		if v == "" {
			return fileVersion{}, fmt.Errorf("%s has no go line", rel)
		}
		lead := leadingVersion.FindString(v)
		if lead == "" {
			return fileVersion{}, fmt.Errorf("%s has an unreadable go version %q", rel, v)
		}
		// go.mod names the oldest Go that builds the module.
		return fileVersion{spec: lead, minimum: true}, nil
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) > 1 {
			// .tool-versions: "nodejs 20.11.0"
			found := false
			for _, l := range strings.Split(string(data), "\n") {
				g := strings.Fields(l)
				if len(g) > 1 && toolAlias(g[0]) == tool {
					f, found = g, true
					break
				}
			}
			if !found {
				continue
			}
		}
		return fileVersion{spec: f[len(f)-1]}, nil
	}
	return fileVersion{}, fmt.Errorf("the version file %s has no version", rel)
}

func toolAlias(s string) string {
	switch s {
	case "nodejs":
		return "node"
	case "golang":
		return "go"
	case "rust", "rustc":
		return "rust"
	case "nushell":
		return "nu"
	}
	return s
}

// toolFinder finds tools on the job's PATH or under the tools folder,
// as local-ci.nu's find-tool does.
type toolFinder struct {
	dir string // <state>/tools or $TENTMAKER_TOOLS
	src string // the checkout, for file: specs
}

// find returns the folder holding a binary of tool that fits spec.
func (f toolFinder) find(ctx context.Context, tool, spec, pathList string) (dir, version string, err error) {
	w, err := parseSpec(tool, spec, f.src)
	if err != nil {
		return "", "", fmt.Errorf("%s: %v", tool, err)
	}
	bin := toolBins[tool]
	if bin == "" {
		bin = tool
	}
	var cands []string
	if p := lookPathIn(bin, pathList); p != "" {
		cands = append(cands, p)
	}
	cands = append(cands, f.installed(tool, bin)...)
	var seen []string
	for _, c := range cands {
		v, out := probeVersion(ctx, tool, c)
		if w.fits(v, out) {
			return filepath.Dir(c), v, nil
		}
		if v != "" {
			seen = append(seen, fmt.Sprintf("%s %s at %s", tool, v, c))
		}
	}
	// The folder name only orders candidates; the binary's own version
	// decides, so "go-1.25.x" is a fine name.
	example := w.desc
	if w.series == "" {
		example = "<version>"
	}
	msg := fmt.Sprintf("%s %s is not installed on this device.\nFix: put %s %s on PATH, or unpack it so %s exists (set TENTMAKER_TOOLS to use another folder).",
		tool, w.desc, tool, w.desc, filepath.Join(f.dir, tool+"-"+example, "bin", exeName(bin)))
	if len(seen) > 0 {
		msg += "\nFound, but not a fit: " + strings.Join(seen, "; ")
	}
	return "", "", fmt.Errorf("%s", msg)
}

// installed lists <dir>/<tool>-*/bin/<bin> and <dir>/<tool>-*/<bin>, newest first.
func (f toolFinder) installed(tool, bin string) []string {
	matches, _ := filepath.Glob(filepath.Join(f.dir, tool+"-*"))
	sort.Slice(matches, func(a, b int) bool {
		va := strings.TrimPrefix(filepath.Base(matches[a]), tool+"-")
		vb := strings.TrimPrefix(filepath.Base(matches[b]), tool+"-")
		return compareVersions(va, vb) > 0
	})
	var out []string
	for _, m := range matches {
		for _, c := range []string{filepath.Join(m, "bin", exeName(bin)), filepath.Join(m, exeName(bin))} {
			if isExecutable(c) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func probeVersion(ctx context.Context, tool, bin string) (version, output string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := versionArgs[tool]
	if args == nil {
		args = []string{"--version"}
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 2 * time.Second
	out, _ := cmd.CombinedOutput()
	s := string(out)
	return versionRe.FindString(s), s
}

// lookPathIn finds an executable in a PATH-style list, like exec.LookPath
// but for the job's PATH rather than ours.
func lookPathIn(name, pathList string) string {
	if strings.ContainsAny(name, `/\`) {
		if isExecutable(name) {
			return name
		}
		return ""
	}
	for _, d := range filepath.SplitList(pathList) {
		if d == "" {
			continue
		}
		for _, n := range exeNames(name) {
			p := filepath.Join(d, n)
			if isExecutable(p) {
				return p
			}
		}
	}
	return ""
}

func exeNames(name string) []string {
	if runtime.GOOS != "windows" || filepath.Ext(name) != "" {
		return []string{name}
	}
	return []string{name + ".exe", name + ".cmd", name + ".bat"}
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || fi.Mode()&0o111 != 0
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// compareVersions compares dotted numeric versions; missing parts count as 0.
func compareVersions(a, b string) int {
	pa := strings.Split(versionRe.FindString(a+".0"), ".")
	pb := strings.Split(versionRe.FindString(b+".0"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

var (
	nixDevelop = regexp.MustCompile(`^nix develop (\S+) --command `)
	nixZig     = regexp.MustCompile(`pkgs\.zig_(\d+)_(\d+)`)
)

// nixShellTools finds host tools standing in for a flake's dev shell when
// nix isn't installed, as local-ci.nu does: today the zig series it pins.
func (f toolFinder) nixShellTools(ctx context.Context, flake, pathList string) ([]string, error) {
	data, err := os.ReadFile(flake)
	if err != nil {
		return nil, nil
	}
	var dirs []string
	for _, m := range nixZig.FindAllStringSubmatch(string(data), -1) {
		d, _, err := f.find(ctx, "zig", m[1]+"."+m[2], pathList)
		if err != nil {
			return nil, err
		}
		if !contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	return dirs, nil
}
