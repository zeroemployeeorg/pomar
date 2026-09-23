// Command pomar-hygiene checks every tracked file for private operational
// detail. See package hygiene.
//
// Usage:
//
//	pomar-hygiene [-denylist FILE] [-require-denylist]
//
// Without a deny-list only the generic checks run (IP literals and home
// paths). The maintainers' gate passes -require-denylist, so a missing list
// fails the run instead of silently weakening it.
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/hygiene"
)

func main() {
	denyPath := flag.String("denylist", "", "file of case-insensitive substrings that must not appear")
	require := flag.Bool("require-denylist", false, "fail if -denylist is missing or empty")
	flag.Parse()

	var list []byte
	if *denyPath != "" {
		b, err := os.ReadFile(*denyPath)
		if err != nil && *require {
			fail("reading deny-list: %v", err)
		}
		list = b
	}
	c, err := hygiene.ParseDenyList(bytes.NewReader(list))
	if err != nil {
		fail("parsing deny-list: %v", err)
	}
	if *require && c.Entries() == 0 {
		fail("deny-list required but has no entries")
	}
	fmt.Printf("hygiene: deny-list entries=%d sha256=%x\n", c.Entries(), sha256.Sum256(list))

	out, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		fail("git ls-files: %v", err)
	}
	var findings []hygiene.Finding
	files := 0
	for _, path := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if path == "" {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			fail("reading %s: %v", path, err)
		}
		files++
		findings = append(findings, c.CheckFile(path, content)...)
	}
	for _, f := range findings {
		fmt.Println(f)
	}
	fmt.Printf("hygiene: files=%d findings=%d\n", files, len(findings))
	if len(findings) > 0 {
		os.Exit(1)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pomar-hygiene: "+format+"\n", args...)
	os.Exit(2)
}
