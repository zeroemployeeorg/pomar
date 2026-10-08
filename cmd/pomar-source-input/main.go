// pomar-source-input is an owner-side, local-only tree input exporter/verifier.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/zeroemployeeorg/pomar/internal/treeinput"
	"os"
	"strings"
	"time"
)

func main() {
	mirror := flag.String("mirror", "", "absolute existing bare owner mirror")
	ref := flag.String("ref", "", "exact refs/heads/ owner branch")
	commit := flag.String("commit", "", "independently pinned full source commit")
	out := flag.String("output", "", "new private tar.gz output")
	verify := flag.String("verify", "", "verify existing tar.gz instead of exporting")
	require := flag.String("require", "package.json,package-lock.json", "comma-separated required tracked source/lock paths")
	max := flag.Int64("max-bytes", treeinput.MaxArchive, "compressed input bound, at most32MiB")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var m treeinput.Manifest
	var err error
	if flag.NArg() != 0 {
		err = fmt.Errorf("unexpected positional arguments")
	} else if *verify != "" {
		if *mirror != "" || *ref != "" || *out != "" {
			err = fmt.Errorf("verification cannot select an export")
		} else {
			var f *os.File
			f, err = os.Open(*verify)
			if err == nil {
				defer f.Close()
				m, err = treeinput.Verify(f, *commit, strings.Split(*require, ","))
			}
		}
	} else {
		m, err = treeinput.Build(ctx, *mirror, *ref, *commit, *out, strings.Split(*require, ","), *max)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "pomar-source-input:", err)
		os.Exit(1)
	}
	json.NewEncoder(os.Stdout).Encode(struct {
		Commit string `json:"commit"`
		Tree   string `json:"tree"`
		Files  int    `json:"files"`
	}{m.Commit, m.Tree, len(m.Entries)})
}
