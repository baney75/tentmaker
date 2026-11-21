// Command tentmaker turns spare capacity on your own machines into CI runners.
package main

import (
	"os"

	"github.com/baney75/tentmaker/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
