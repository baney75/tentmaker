// Command bench turns the raw output of the bench workflow
// (.github/workflows/bench.yml) into docs/BENCHMARKS.md and
// docs/benchmarks.json, and gates the run.
//
//	go run ./scripts/bench report -raw DIR -md FILE -json FILE
//	go run ./scripts/bench report -from FILE -md FILE -json FILE
//	go run ./scripts/bench gate -json FILE
//	go run ./scripts/bench install -os OS -marks FILE -setup FILE [-statuses FILE -ctx C] [-budget-ms N] -json FILE -md FILE
//
// report writes both files and prints the Markdown; with -from it reads the
// samples of an earlier benchmarks.json instead of a raw folder, so a change
// to this tool can re-render a published run. gate recomputes every
// number from the samples in the JSON and exits 1 when the run fails the
// gate (see Gate). install times one e2e job's path from the install
// command to its first green job (see install.go). It is a repo tool, not
// part of tentmaker.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bench report (-raw DIR | -from FILE) -md FILE -json FILE | bench gate -json FILE")
		return 2
	}
	if args[0] == "install" {
		return runInstall(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("bench "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	raw := fs.String("raw", "", "raw bench folder (report)")
	from := fs.String("from", "", "an earlier benchmarks.json to re-render (report)")
	md := fs.String("md", "", "Markdown output (report)")
	js := fs.String("json", "", "JSON output (report) or input (gate)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	switch args[0] {
	case "report":
		if (*raw == "") == (*from == "") || *md == "" || *js == "" {
			fmt.Fprintln(stderr, "bench report: -md, -json and one of -raw or -from are required")
			return 2
		}
		var rep Report
		if *raw != "" {
			r, err := LoadRun(*raw)
			if err != nil {
				fmt.Fprintln(stderr, "bench report:", err)
				return 1
			}
			rep = Summarize(r)
		} else {
			data, err := os.ReadFile(*from)
			if err == nil {
				rep, err = LoadReport(data)
			}
			if err != nil {
				fmt.Fprintln(stderr, "bench report:", err)
				return 1
			}
		}
		data, err := marshal(rep)
		if err != nil {
			fmt.Fprintln(stderr, "bench report:", err)
			return 1
		}
		text := Markdown(rep)
		for path, b := range map[string][]byte{*js: data, *md: []byte(text)} {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				fmt.Fprintln(stderr, "bench report:", err)
				return 1
			}
			if err := os.WriteFile(path, b, 0o644); err != nil {
				fmt.Fprintln(stderr, "bench report:", err)
				return 1
			}
		}
		fmt.Fprint(stdout, text)
		return 0
	case "gate":
		if *js == "" {
			fmt.Fprintln(stderr, "bench gate: -json is required")
			return 2
		}
		data, err := os.ReadFile(*js)
		if err != nil {
			fmt.Fprintln(stderr, "bench gate:", err)
			return 1
		}
		rep, err := LoadReport(data)
		if err != nil {
			fmt.Fprintln(stderr, "bench gate:", err)
			return 1
		}
		if rep.Gate.Pass {
			fmt.Fprintln(stdout, "bench gate: pass")
			return 0
		}
		for _, f := range rep.Gate.Failures {
			fmt.Fprintln(stdout, "bench gate: FAIL:", f)
		}
		return 1
	default:
		fmt.Fprintf(stderr, "bench: unknown command %q\n", args[0])
		return 2
	}
}

func marshal(rep Report) ([]byte, error) {
	data, err := jsonIndent(rep)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
