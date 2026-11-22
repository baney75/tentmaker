package checks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/baney75/tentmaker/internal/core"
)

// gitlabGlobals are top-level .gitlab-ci.yml keys that aren't jobs.
var gitlabGlobals = map[string]bool{
	"image": true, "services": true, "stages": true, "types": true, "before_script": true,
	"after_script": true, "variables": true, "cache": true, "include": true, "workflow": true,
	"default": true, "spec": true,
}

// GitLab job keys checks mode can ignore: they change where or how fast a
// job runs, not what it does.
var gitlabIgnored = map[string]bool{
	"image": true, "needs": true, "dependencies": true, "cache": true, "allow_failure": true,
	"interruptible": true, "retry": true, "coverage": true, "resource_group": true,
	"stage": true, "script": true, "before_script": true, "after_script": true,
	"variables": true, "tags": true, "timeout": true, "extends": true, "when": true,
	"rules": true, "inherit": true, "artifacts": true,
}

var gitlabRefused = map[string]string{
	"only":               "uses only:, which depends on the pipeline",
	"except":             "uses except:, which depends on the pipeline",
	"services":           "needs service containers (services:)",
	"trigger":            "triggers another pipeline",
	"parallel":           "runs in parallel copies (parallel:)",
	"environment":        "deploys to an environment",
	"release":            "creates a release",
	"pages":              "publishes GitLab Pages",
	"secrets":            "needs CI secrets",
	"id_tokens":          "needs ID tokens",
	"hooks":              "uses hooks:",
	"identity":           "uses identity:",
	"run":                "uses CI steps (run:)",
	"dast_configuration": "uses dast_configuration:",
}

var defaultStages = []string{"build", "test", "deploy"}

func planGitLab(path string, src []byte) ([]core.CheckJob, []Skip, error) {
	root, inputs, err := gitlabDoc(src)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if !root.isMap() {
		return nil, nil, fmt.Errorf("%s: .gitlab-ci.yml must be a mapping", path)
	}
	var skips []Skip
	if root.has("include") {
		skips = append(skips, Skip{"include", "checks mode doesn't read included files; jobs defined there don't run"})
	}

	stageList := defaultStages
	if st := root.get("stages"); st != nil {
		l, ok := st.strings()
		if !ok {
			return nil, nil, fmt.Errorf("%s: stages must be a list of names", path)
		}
		stageList = l
	}
	stages := map[string]int{".pre": 0}
	for i, s := range stageList {
		stages[s] = i + 1
	}
	stages[".post"] = len(stageList) + 1

	globalSkip := ""
	if wf := root.get("workflow"); wf != nil && wf.has("rules") {
		run, reason := gitlabRules(wf.get("rules"))
		if reason != "" {
			globalSkip = "workflow:rules " + reason
		} else if !run {
			return nil, skips, nil
		}
	}

	// default: wins over the deprecated top-level globals.
	defaults := newMap()
	for _, k := range []string{"image", "services", "before_script", "after_script", "cache"} {
		if v := root.get(k); v != nil {
			defaults.set(k, v)
		}
	}
	if d := root.get("default"); d.isMap() {
		defaults = deepMerge(defaults, d)
	}

	type planned struct {
		job   core.CheckJob
		stage int
		order int
	}
	var jobs []planned
	for i, name := range root.keys {
		if gitlabGlobals[name] || strings.HasPrefix(name, ".") {
			continue
		}
		if globalSkip != "" {
			skips = append(skips, Skip{name, globalSkip})
			continue
		}
		job, stage, reason := gitlabJob(path, name, root, defaults, stages, inputs)
		if reason != "" {
			skips = append(skips, Skip{name, reason})
			continue
		}
		jobs = append(jobs, planned{job, stage, i})
	}
	sort.SliceStable(jobs, func(a, b int) bool { return jobs[a].stage < jobs[b].stage })
	out := make([]core.CheckJob, len(jobs))
	for i, p := range jobs {
		out[i] = p.job
	}
	return out, skips, nil
}

// gitlabDoc reads the jobs document. A file with a spec: header has its jobs
// in the second YAML document.
func gitlabDoc(src []byte) (*val, bool, error) {
	dec := yaml.NewDecoder(bytes.NewReader(src))
	var docs []*val
	budget := maxNodes
	for len(docs) < 2 {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, false, err
		}
		v, err := convert(&n, 0, &budget)
		if err != nil {
			return nil, false, err
		}
		docs = append(docs, v)
	}
	switch {
	case len(docs) == 0:
		return newMap(), false, nil
	case len(docs) == 2 && docs[0].isMap() && docs[0].has("spec"):
		return docs[1], true, nil
	}
	return docs[0], false, nil
}

func gitlabJob(path, name string, root, defaults *val, stages map[string]int, inputs bool) (core.CheckJob, int, string) {
	j := root.get(name)
	if !j.isMap() {
		return core.CheckJob{}, 0, "the job is not a mapping"
	}
	if ext := j.get("extends"); ext != nil {
		names, ok := ext.strings()
		if !ok {
			return core.CheckJob{}, 0, "extends: must name jobs"
		}
		merged := newMap()
		for _, t := range names {
			tmpl := root.get(t)
			if !tmpl.isMap() {
				return core.CheckJob{}, 0, "extends " + t + ", which isn't in this file"
			}
			if tmpl.has("extends") {
				return core.CheckJob{}, 0, "extends " + t + ", which extends another job; checks mode resolves one level"
			}
			merged = deepMerge(merged, tmpl)
		}
		j = deepMerge(merged, j)
	}
	for _, k := range j.keys {
		if r, ok := gitlabRefused[k]; ok {
			return core.CheckJob{}, 0, "the job " + r
		}
		if !gitlabIgnored[k] {
			return core.CheckJob{}, 0, "the job uses " + k + ":, which checks mode doesn't support"
		}
	}
	if inputs && strings.Contains(j.text(), "$[[") {
		return core.CheckJob{}, 0, "the job uses spec inputs ($[[ ]]), which checks mode can't evaluate"
	}

	useDefaults, useGlobals := true, true
	if inh := j.get("inherit"); inh != nil {
		for _, k := range inh.keys {
			v := inh.m[k]
			if !v.isScalar() || (v.tag != "!!bool" && !isBoolText(v.s)) {
				return core.CheckJob{}, 0, "inherit:" + k + " lists keys; checks mode handles only true or false"
			}
			switch k {
			case "default":
				useDefaults = isTrue(v.s)
			case "variables":
				useGlobals = isTrue(v.s)
			}
		}
	}
	if useDefaults {
		for _, k := range []string{"services"} {
			if defaults.has(k) && !j.has(k) {
				return core.CheckJob{}, 0, "the job " + gitlabRefused[k]
			}
		}
		for _, k := range []string{"before_script", "after_script", "tags", "timeout"} {
			if v := defaults.get(k); v != nil && !j.has(k) {
				j = j.clone()
				j.set(k, v)
			}
		}
	}

	if r := j.get("rules"); r != nil {
		run, reason := gitlabRules(r)
		if reason != "" {
			return core.CheckJob{}, 0, "rules: " + reason
		}
		if !run {
			return core.CheckJob{}, 0, "rules: never run this job"
		}
	}
	switch w := j.get("when").str(); w {
	case "", "on_success", "always":
	default:
		return core.CheckJob{}, 0, "the job runs when: " + w + ", which checks mode doesn't do"
	}

	stageName := j.get("stage").str()
	if stageName == "" {
		stageName = "test"
	}
	stage, ok := stages[stageName]
	if !ok {
		return core.CheckJob{}, 0, "stage " + stageName + " isn't in stages:"
	}

	platform := core.Platform{OS: "linux"}
	if tags, ok := j.get("tags").strings(); ok {
		for _, t := range tags {
			lt := strings.ToLower(t)
			switch {
			case strings.Contains(lt, "windows"):
				return core.CheckJob{}, 0, "Windows GitLab jobs aren't supported in checks mode yet (tag " + t + ")"
			case strings.Contains(lt, "macos"):
				platform.OS = "macos"
			}
			if strings.Contains(lt, "arm64") || strings.Contains(lt, "aarch64") {
				platform.Arch = "arm64"
			}
		}
	} else {
		return core.CheckJob{}, 0, "tags: must be a list of names"
	}

	script, ok := gitlabScript(j.get("script"))
	if !ok || len(script) == 0 {
		return core.CheckJob{}, 0, "the job has no script"
	}
	before, ok1 := gitlabScript(j.get("before_script"))
	after, ok2 := gitlabScript(j.get("after_script"))
	if !ok1 || !ok2 {
		return core.CheckJob{}, 0, "before_script and after_script must be lists of commands"
	}

	env := map[string]string{}
	if useGlobals {
		if r := gitlabVars(env, root.get("variables")); r != "" {
			return core.CheckJob{}, 0, r
		}
	}
	if r := gitlabVars(env, j.get("variables")); r != "" {
		return core.CheckJob{}, 0, r
	}
	env["CI_JOB_NAME"] = name
	env["CI_JOB_STAGE"] = stageName

	timeout := defaultTimeout
	if t := j.get("timeout").str(); t != "" {
		if d, ok := gitlabDuration(t); ok && d > 0 {
			timeout = d
		}
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}

	steps := []core.CheckStep{{
		Name:  "script",
		Run:   strings.Join(append(before, script...), "\n"),
		Shell: "bash",
	}}
	if len(after) > 0 {
		steps = append(steps, core.CheckStep{
			Name:            "after_script",
			Run:             strings.Join(after, "\n"),
			Shell:           "bash",
			ContinueOnError: true,
		})
	}
	if j.has("artifacts") {
		steps = append(steps, core.CheckStep{
			Name:  "artifacts (skipped)",
			Run:   "echo 'tentmaker: artifacts are a no-op in checks mode; nothing was uploaded'",
			Shell: "bash",
		})
	}
	return core.CheckJob{
		Workflow: path,
		ID:       name,
		Name:     name,
		Platform: platform,
		Env:      env,
		Steps:    steps,
		Timeout:  timeout,
	}, stage, ""
}

// gitlabRules evaluates rules: that don't depend on the pipeline. Only rules
// made of when: alone can be decided; the first one decides.
func gitlabRules(r *val) (run bool, reason string) {
	if !r.isSeq() {
		return false, "must be a list"
	}
	if len(r.seq) == 0 {
		return false, ""
	}
	for _, rule := range r.seq {
		if !rule.isMap() {
			return false, "each rule must be a mapping"
		}
		for _, k := range rule.keys {
			if k != "when" && k != "allow_failure" {
				return false, "use " + k + ":, which depends on the pipeline"
			}
		}
		switch w := rule.get("when").str(); w {
		case "", "on_success", "always":
			return true, ""
		case "never":
			return false, ""
		default:
			return false, "run the job when: " + w + ", which checks mode doesn't do"
		}
	}
	return false, ""
}

// gitlabScript flattens a script: string or list (one level of nested lists
// is allowed, as anchors produce).
func gitlabScript(v *val) ([]string, bool) {
	switch {
	case v.isNull():
		return nil, true
	case v.isScalar():
		return []string{v.s}, true
	case v.isSeq():
		var out []string
		for _, c := range v.seq {
			switch {
			case c.isScalar():
				out = append(out, c.s)
			case c.isSeq():
				for _, cc := range c.seq {
					if !cc.isScalar() {
						return nil, false
					}
					out = append(out, cc.s)
				}
			default:
				return nil, false
			}
		}
		return out, true
	}
	return nil, false
}

// gitlabVars merges variables: into env. RunBundle expands $VAR references
// in GitLab jobs, so a value with expand: false has its $ doubled.
func gitlabVars(env map[string]string, v *val) string {
	if v.isNull() {
		return ""
	}
	if !v.isMap() {
		return "variables: must be a mapping"
	}
	for _, k := range v.keys {
		x := v.m[k]
		switch {
		case x.isScalar():
			env[k] = x.str()
		case x.isMap():
			s := x.get("value").str()
			if e := x.get("expand"); e != nil && !isTrue(e.str()) {
				s = strings.ReplaceAll(s, "$", "$$")
			}
			env[k] = s
		default:
			return "variable " + k + " is not a literal value"
		}
	}
	return ""
}

var durationPart = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*([a-z]*)`)

// gitlabDuration reads GitLab timeouts like "1h 30m", "3 hours" or "90 minutes".
func gitlabDuration(s string) (time.Duration, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	parts := durationPart.FindAllStringSubmatch(s, -1)
	if len(parts) == 0 {
		return 0, false
	}
	var total float64 // nanoseconds, summed as a float so a huge value can't wrap
	for _, p := range parts {
		n, err := strconv.ParseFloat(p[1], 64)
		if err != nil {
			return 0, false
		}
		var unit time.Duration
		switch p[2] {
		case "", "s", "sec", "secs", "second", "seconds":
			unit = time.Second
		case "m", "min", "mins", "minute", "minutes":
			unit = time.Minute
		case "h", "hr", "hrs", "hour", "hours":
			unit = time.Hour
		case "d", "day", "days":
			unit = 24 * time.Hour
		case "w", "wk", "wks", "week", "weeks":
			unit = 7 * 24 * time.Hour
		default:
			return 0, false
		}
		total += n * float64(unit)
	}
	if total > float64(100*365*24*time.Hour) {
		total = float64(100 * 365 * 24 * time.Hour)
	}
	return time.Duration(total), true
}

func isBoolText(s string) bool {
	switch strings.ToLower(s) {
	case "true", "false":
		return true
	}
	return false
}
