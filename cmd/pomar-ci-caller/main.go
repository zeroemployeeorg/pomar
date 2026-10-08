// pomar-ci-caller has no command-line options. A root-owned UID-selected policy
// restricts stdin requests; the control server still checks the kernel peer UID.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/zeroemployeeorg/pomar/internal/cicaller"
)

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "pomar-ci-caller: no arguments permitted")
		os.Exit(2)
	}
	p, err := cicaller.ReadPolicy("/usr/local/etc/pomar/callers/" + strconv.Itoa(os.Geteuid()) + ".json")
	if err == nil {
		var r cicaller.Reply
		r, err = cicaller.Run(p, os.Stdin)
		if encodeErr := json.NewEncoder(os.Stdout).Encode(r); encodeErr != nil && err == nil {
			err = encodeErr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pomar-ci-caller:", err)
		os.Exit(1)
	}
}
