package github

import (
	"fmt"
	"regexp"
	"strings"
)

// MigrateWorkflow points a workflow's hosted jobs for one platform ("linux",
// "macos" or "windows") at the Tentmaker pool. It edits line by line, so
// comments and layout survive. A job's runs-on becomes
//
//	${{ github.event.pull_request.head.repo.fork && '<hosted image>' || fromJSON('["self-hosted","tentmaker","<os label>"]') }}
//
// which keeps fork pull requests on GitHub-hosted runners. This is routing,
// not the security boundary: a fork PR can edit the workflow it runs. The
// boundary is the broker's fork refusal plus the repo's fork policy.
//
// A matrix runs-on (`${{ matrix.os }}`) whose matrix names images of the
// platform keeps its other legs:
//
//	${{ matrix.os == 'ubuntu-latest' && !github.event.pull_request.head.repo.fork && fromJSON('[...]') || matrix.os }}
//
// It leaves alone, and reports, jobs that deploy, publish, release or sign
// (by name), use an environment, ask for any `<scope>: write` permission or
// write-all, or read secrets other than GITHUB_TOKEN (and every job when the
// workflow level does any of that); Linux jobs that use sudo (the Linux
// sandbox has none); macOS jobs that launch a GUI app (the VM is headless);
// runner images Tentmaker doesn't stand in for (ARM Ubuntu or Windows,
// Intel or large macOS); and expressions it can't read. A workflow it
// changes gets a cancel-superseded concurrency block when it has none,
// except a reusable (workflow_call) one.
//
// report lines start with "changed" or "left". changed is true when out
// differs from src.
func MigrateWorkflow(src []byte, platform string) (out []byte, report []string, changed bool, err error) {
	spec, ok := platforms[platform]
	if !ok {
		return src, nil, false, fmt.Errorf("migrate: platform %q must be linux, macos or windows", platform)
	}
	text := string(src)
	lines := splitLines(text)
	owners := jobsByLine(lines, platform)
	var outside strings.Builder
	for i, l := range lines {
		if _, in := owners[i]; !in {
			outside.WriteString(l)
		}
	}
	sensitiveWorkflow := sensitiveBody(outside.String())
	var b strings.Builder
	var changes, notes []string
	for i := 0; i < len(lines); {
		line, eol := chomp(lines[i])
		where := fmt.Sprintf("line %d", i+1)
		m := runsOnRe.FindStringSubmatch(line)
		if m == nil {
			b.WriteString(lines[i])
			i++
			continue
		}
		indent, value, comment := m[1], m[2], m[3]
		end := i + 1
		var items []string
		switch {
		case strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]"):
			for _, v := range strings.Split(value[1:len(value)-1], ",") {
				if strings.TrimSpace(v) != "" {
					items = append(items, strings.TrimSpace(v))
				}
			}
		case value != "":
			items = []string{value}
		default: // a block list on the following, deeper lines
			for end < len(lines) {
				l, _ := chomp(lines[end])
				im := itemRe.FindStringSubmatch(l)
				if im == nil || len(im[1]) <= len(indent) {
					break
				}
				items = append(items, im[2])
				end++
			}
		}
		keep := func() {
			for _, l := range lines[i:end] {
				b.WriteString(l)
			}
		}
		own, placed := owners[i]
		if !placed {
			own = jobInfo{label: "?", sensitive: true} // a runs-on outside any job stays hosted
		}
		if comment != "" {
			comment = " " + comment
		}
		if len(items) == 1 && strings.Contains(items[0], "${{") {
			key := matrixOnlyRe.FindStringSubmatch(strings.TrimSpace(items[0]))
			labels := matrixLabels(lines, owners, i, spec)
			switch {
			case len(labels) == 0:
				// No leg of this platform: not our business.
			case key == nil:
				notes = append(notes, fmt.Sprintf("left %s: runs-on %s: an expression with %s legs (%s); check it by hand",
					where, strings.TrimSpace(items[0]), spec.name, strings.Join(labels, ", ")))
			case own.gui:
				notes = append(notes, fmt.Sprintf("left %s: job '%s' launches a GUI app; the macOS VM is headless, so it stays on hosted runners", where, own.label))
			case sensitiveWorkflow || own.sensitive:
				notes = append(notes, fmt.Sprintf("left %s: job '%s' has %s matrix legs but deploys or holds credentials; it stays on hosted runners", where, own.label, spec.name))
			case own.sudo:
				notes = append(notes, fmt.Sprintf("left %s: job '%s' uses sudo, which Tentmaker's Linux sandbox doesn't have; it stays on hosted runners", where, own.label))
			default:
				b.WriteString(indent + "runs-on: " + matrixExpr("matrix."+key[1], labels, spec) + comment + eol)
				changes = append(changes, fmt.Sprintf("changed %s: runs-on matrix.%s: the %s leg(s) go to Tentmaker (other legs and fork PRs keep the matrix value)",
					where, key[1], strings.Join(labels, ", ")))
				i = end
				continue
			}
			keep()
			i = end
			continue
		}
		if len(items) == 1 && classify(items[0], spec) == "pool" {
			hosted := bare(items[0])
			switch {
			case own.gui:
				notes = append(notes, fmt.Sprintf("left %s: job '%s' launches a GUI app; the macOS VM is headless, so it stays on hosted runners", where, own.label))
				keep()
			case sensitiveWorkflow || own.sensitive:
				notes = append(notes, fmt.Sprintf("left %s: job '%s' deploys or holds credentials (its name, an environment, a write permission or secrets); it stays on hosted runners", where, own.label))
				keep()
			case own.sudo:
				notes = append(notes, fmt.Sprintf("left %s: job '%s' uses sudo, which Tentmaker's Linux sandbox doesn't have; it stays on hosted runners", where, own.label))
				keep()
			default:
				b.WriteString(indent + "runs-on: " + poolExpr(hosted, spec) + comment + eol)
				changes = append(changes, fmt.Sprintf("changed %s: runs-on %s -> Tentmaker %s pool (fork PRs stay on %s)", where, hosted, spec.name, hosted))
			}
			i = end
			continue
		}
		var reasons []string
		seen := map[string]bool{}
		for _, it := range items {
			if r := classify(it, spec); r != "pool" && r != "" && !seen[r] {
				seen[r] = true
				reasons = append(reasons, r)
			}
		}
		if len(reasons) > 0 {
			bareItems := make([]string, len(items))
			for k, it := range items {
				bareItems[k] = bare(it)
			}
			notes = append(notes, fmt.Sprintf("left %s: runs-on %s: %s", where, strings.Join(bareItems, ", "), strings.Join(reasons, "; ")))
		}
		keep()
		i = end
	}
	result := b.String()
	report = append(report, changes...)
	if len(changes) > 0 {
		switch {
		case reusableRe.MatchString(text):
			notes = append(notes, "left: reusable workflow (workflow_call); no concurrency block added")
		case concurrencyRe.MatchString(text):
		default:
			if loc := jobsRe.FindStringIndex(result); loc != nil {
				eol := "\n"
				if strings.Contains(text, "\r\n") {
					eol = "\r\n"
				}
				block := strings.ReplaceAll(concurrencyBlock, "\n", eol)
				result = result[:loc[0]] + block + result[loc[0]:]
				report = append(report, "changed: added a cancel-superseded concurrency block")
			}
		}
	}
	report = append(report, notes...)
	return []byte(result), report, result != text, nil
}

type platformSpec struct {
	name    string         // "Linux", "macOS", "Windows"
	osLabel string         // the runner's OS label
	hosted  *regexp.Regexp // hosted images Tentmaker stands in for
	family  string         // prefix of every hosted image of this OS
	why     string         // why other images of the family stay hosted
}

var platforms = map[string]platformSpec{
	"linux": {
		name: "Linux", osLabel: "linux", family: "ubuntu-",
		hosted: regexp.MustCompile(`^ubuntu-(latest|\d\d\.\d\d)$`),
		why:    "an ARM or larger Ubuntu image; Tentmaker migrates only ubuntu-latest and ubuntu-NN.NN",
	},
	"macos": {
		name: "macOS", osLabel: "macOS", family: "macos-",
		hosted: regexp.MustCompile(`^macos-(latest|14|15|26)$`),
		why:    "an Intel or larger macOS image; Tentmaker's macOS VMs are Apple silicon (14, 15, 26 and latest)",
	},
	"windows": {
		name: "Windows", osLabel: "Windows", family: "windows-",
		hosted: regexp.MustCompile(`^windows-(latest|2019|2022|2025)$`),
		why:    "an ARM or larger Windows image; Tentmaker migrates only windows-latest and windows-YYYY",
	},
}

var (
	runsOnRe      = regexp.MustCompile(`^(\s*)runs-on:\s*([^#]*?)\s*(#.*)?$`)
	itemRe        = regexp.MustCompile(`^(\s*)-\s*([^#]*?)\s*(?:#.*)?$`)
	keyRe         = regexp.MustCompile(`^( +)([A-Za-z0-9_-]+):\s*(.*?)\s*$`)
	matrixOnlyRe  = regexp.MustCompile(`^\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}$`)
	sensitiveName = regexp.MustCompile(`(?i)release|deploy|publish|sign`)
	envRe         = regexp.MustCompile(`(?m)^\s*environment:`)
	writeRe       = regexp.MustCompile(`\b[a-z-]+:\s*['"]?write\b|write-all`)
	credDotRe     = regexp.MustCompile(`secrets\s*\.\s*(\w+)`)
	credIdxRe     = regexp.MustCompile(`secrets\s*\[\s*['"]([^'"]*)['"]`)
	credAllRe     = regexp.MustCompile(`toJSON\(\s*secrets|secrets\s*\[\s*[^'"\s]`)
	sudoRe        = regexp.MustCompile(`(^|[\s;&|])sudo\s`)
	guiRe         = regexp.MustCompile(`--selftest|\bopen\s+(?:-[A-Za-z]+\s+)*\S*\.app\b|\bopen\s+-a\b|Contents/MacOS/|osascript|screencapture|UITests`)
	ifLineRe      = regexp.MustCompile(`^\s*(?:-\s+)?if:\s*(.*?)\s*(?:#.*)?$`)
	notLinuxIfRe  = regexp.MustCompile(`^(?:\$\{\{\s*)?(?:runner\.os\s*==\s*['"](?:macOS|Windows)['"]|runner\.os\s*!=\s*['"]Linux['"]|matrix\.os\s*==\s*['"](?:macos|windows)[A-Za-z0-9._-]*['"])(?:\s*\}\})?$`)
	stepStartRe   = regexp.MustCompile(`^\s*-\s`)
	reusableRe    = regexp.MustCompile(`(?m)^\s*workflow_call\s*:|^on:.*\bworkflow_call\b|^\s*-\s*workflow_call\s*$`)
	concurrencyRe = regexp.MustCompile(`(?m)^concurrency:`)
	jobsRe        = regexp.MustCompile(`(?m)^jobs:`)
	tokenRe       = regexp.MustCompile(`[A-Za-z0-9_.-]+`)
	jobNameRe     = regexp.MustCompile(`(?m)^\s+name:\s*(.+?)\s*$`)
)

const concurrencyBlock = "# Added by tentmaker: a new push to a pull request cancels its older run.\n" +
	"# Every other run (pushes, schedules, manual runs) gets its own group and is never cancelled.\n" +
	"concurrency:\n" +
	"  group: ${{ github.workflow }}-${{ github.event_name == 'pull_request' && format('pr-{0}', github.event.pull_request.number) || format('run-{0}', github.run_id) }}\n" +
	"  cancel-in-progress: ${{ github.event_name == 'pull_request' }}\n" +
	"\n"

func poolJSON(spec platformSpec) string {
	return `["self-hosted","tentmaker","` + spec.osLabel + `"]`
}

func poolExpr(hosted string, spec platformSpec) string {
	return "${{ github.event.pull_request.head.repo.fork && '" + hosted + "' || fromJSON('" + poolJSON(spec) + "') }}"
}

func matrixExpr(key string, labels []string, spec platformSpec) string {
	tests := make([]string, len(labels))
	for i, l := range labels {
		tests[i] = key + " == '" + l + "'"
	}
	test := strings.Join(tests, " || ")
	if len(labels) > 1 {
		test = "(" + test + ")"
	}
	return "${{ " + test + " && !github.event.pull_request.head.repo.fork && fromJSON('" + poolJSON(spec) + "') || " + key + " }}"
}

func bare(v string) string { return strings.Trim(strings.TrimSpace(v), `'"`) }

// classify returns "pool" for an image this platform stands in for, a
// reason for one of its family that stays hosted, or "" (another OS, a
// custom label, an expression): nothing to say.
func classify(label string, spec platformSpec) string {
	l := bare(label)
	if spec.hosted.MatchString(l) {
		return "pool"
	}
	if strings.HasPrefix(l, spec.family) {
		return spec.why
	}
	return ""
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.SplitAfter(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// chomp splits a line from its ending ("\n", "\r\n" or none).
func chomp(l string) (string, string) {
	switch {
	case strings.HasSuffix(l, "\r\n"):
		return l[:len(l)-2], "\r\n"
	case strings.HasSuffix(l, "\n"):
		return l[:len(l)-1], "\n"
	}
	return l, "\n"
}

func indentOf(l string) int { return len(l) - len(strings.TrimLeft(l, " \t")) }

func isComment(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "#") }

// sensitiveBody: an environment, a write permission, or secrets other than
// GITHUB_TOKEN.
func sensitiveBody(body string) bool {
	if envRe.MatchString(body) || writeRe.MatchString(body) || credAllRe.MatchString(body) {
		return true
	}
	for _, m := range credDotRe.FindAllStringSubmatch(body, -1) {
		if m[1] != "GITHUB_TOKEN" {
			return true
		}
	}
	for _, m := range credIdxRe.FindAllStringSubmatch(body, -1) {
		if m[1] != "GITHUB_TOKEN" {
			return true
		}
	}
	return false
}

type jobInfo struct {
	label     string // job id and name
	sensitive bool
	sudo      bool // uses sudo (counted for Linux only)
	gui       bool // launches a GUI app (counted for macOS only)
}

// stepBounds returns [start, end) of the step that line k belongs to.
func stepBounds(lines []string, k int) (int, int) {
	j := k
	if !stepStartRe.MatchString(lines[k]) {
		j = k - 1
		for j > 0 && !(stepStartRe.MatchString(lines[j]) && indentOf(lines[j]) < indentOf(lines[k])) {
			j--
		}
	}
	base := indentOf(lines[j])
	e := j + 1
	for e < len(lines) && (strings.TrimSpace(lines[e]) == "" || isComment(lines[e]) || indentOf(lines[e]) > base) {
		e++
	}
	return j, e
}

// sudoApplies: a step whose whole `if:` is one equality no Linux runner
// satisfies never reaches Linux, so its sudo does not count.
func sudoApplies(lines []string, k int) bool {
	start, end := stepBounds(lines, k)
	for n := start; n < end; n++ {
		l, _ := chomp(lines[n])
		if m := ifLineRe.FindStringSubmatch(l); m != nil && notLinuxIfRe.MatchString(strings.TrimSpace(m[1])) {
			return false
		}
	}
	return true
}

// jobsByLine maps each line inside `jobs:` to its job. The job indent is
// whatever the first key under `jobs:` uses.
func jobsByLine(lines []string, platform string) map[int]jobInfo {
	type block struct {
		start, end int
		id         string
	}
	var blocks []block
	inJobs, jobIndent, start, current := false, -1, 0, ""
	all := append(append([]string(nil), lines...), "end:\n")
	for i, line := range all {
		if strings.TrimSpace(line) == "" || isComment(line) {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			if current != "" {
				blocks = append(blocks, block{start, i, current})
			}
			inJobs, jobIndent, current = strings.HasPrefix(line, "jobs:"), -1, ""
			continue
		}
		l, _ := chomp(line)
		m := keyRe.FindStringSubmatch(l)
		if !inJobs || m == nil {
			continue
		}
		if jobIndent < 0 {
			jobIndent = len(m[1])
		}
		if len(m[1]) == jobIndent {
			if current != "" {
				blocks = append(blocks, block{start, i, current})
			}
			start, current = i, m[2]
		}
	}
	owner := map[int]jobInfo{}
	for _, bl := range blocks {
		body := strings.Join(lines[bl.start:bl.end], "")
		label := bl.id
		if bl.start+1 < bl.end {
			if nm := jobNameRe.FindStringSubmatch(strings.Join(lines[bl.start+1:bl.end], "")); nm != nil {
				label += " " + strings.TrimRight(nm[1], "\r")
			}
		}
		info := jobInfo{label: label, sensitive: sensitiveName.MatchString(label) || sensitiveBody(body)}
		for k := bl.start; k < bl.end; k++ {
			if isComment(lines[k]) {
				continue
			}
			if platform == "linux" && sudoRe.MatchString(lines[k]) && sudoApplies(lines, k) {
				info.sudo = true
			}
			if platform == "macos" && guiRe.MatchString(lines[k]) {
				info.gui = true
			}
		}
		for k := bl.start; k < bl.end; k++ {
			owner[k] = info
		}
	}
	return owner
}

// matrixLabels lists, in order, this platform's images named inside the
// job that line i belongs to (outside its runs-on line).
func matrixLabels(lines []string, owners map[int]jobInfo, i int, spec platformSpec) []string {
	own, ok := owners[i]
	if !ok {
		return nil
	}
	var found []string
	for j, l := range lines {
		o, in := owners[j]
		if !in || o.label != own.label || j == i {
			continue
		}
		if c, _ := chomp(l); runsOnRe.MatchString(c) || isComment(l) {
			continue
		}
		for _, tok := range tokenRe.FindAllString(l, -1) {
			if spec.hosted.MatchString(tok) && !contains(found, tok) {
				found = append(found, tok)
			}
		}
	}
	return found
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
