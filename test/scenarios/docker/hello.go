//go:build ignore

// hello is what the docker-cache scenario's Dockerfile builds. The ignore
// tag keeps it out of this module's packages.
package main

import "fmt"

func main() { fmt.Println("hello from a tentmaker container build") }
