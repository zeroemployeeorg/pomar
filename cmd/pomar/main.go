// Command pomar is the coordinator CLI for isolated, per-attempt Linux
// micro-VMs on Apple silicon. This is the skeleton; subcommands arrive one
// per change.
package main

import (
	"fmt"
	"os"
)

const version = "0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Println("pomar", version)
		return 0
	}
	fmt.Fprintln(os.Stderr, "usage: pomar version")
	return 2
}
