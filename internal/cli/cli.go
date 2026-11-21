// Package cli is the tentmaker command line. Wave 2 wires every package here.
package cli

import (
	"fmt"
	"os"
)

// Version is set at build time with -ldflags "-X github.com/baney75/tentmaker/internal/cli.Version=...".
var Version = "0.0.0-dev"

// Main runs the command line and returns the exit code.
func Main(args []string) int {
	if len(args) > 0 && (args[0] == "version" || args[0] == "--version") {
		fmt.Println("tentmaker", Version)
		return 0
	}
	fmt.Fprintln(os.Stderr, "tentmaker: not wired yet")
	return 2
}
