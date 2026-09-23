// Command pomar is the coordinator CLI for isolated, per-attempt Linux
// micro-VMs on Apple silicon. Subcommands arrive one per change.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/zeroemployeeorg/pomar/internal/smoke"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

const version = "0.0.0-dev"

const usage = `usage:
  pomar version
  pomar venue status [-root DIR]    data root, fill, open ledgered objects
  pomar smoke fetch-kernel [-root DIR] -host-bin PATH
  pomar smoke boot [-root DIR] -host-bin PATH -kernel-sha256 HEX -id ID

The data root comes from -root or POMAR_DATA_ROOT. It has no default.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && args[0] == "version":
		fmt.Fprintln(stdout, "pomar", version)
		return 0
	case len(args) >= 2 && args[0] == "venue" && args[1] == "status":
		return venueStatus(args[2:], stdout, stderr)
	case len(args) >= 2 && args[0] == "smoke" && (args[1] == "fetch-kernel" || args[1] == "boot"):
		return smokeCmd(args[1], args[2:], stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	return 2
}

func venueStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("venue status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("POMAR_DATA_ROOT"), "data root")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	v, err := venue.Open(*root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	pct, err := v.Fill()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	heavy := "allowed"
	if pct > venue.DefaultMaxFillPercent {
		heavy = "REFUSED"
	}
	open := v.OpenObjects()
	sort.Slice(open, func(i, j int) bool {
		if open[i].Kind != open[j].Kind {
			return open[i].Kind < open[j].Kind
		}
		return open[i].ID < open[j].ID
	})
	fmt.Fprintf(stdout, "fill: %d%% (limit %d%%, heavy work %s)\n", pct, venue.DefaultMaxFillPercent, heavy)
	fmt.Fprintf(stdout, "open objects: %d\n", len(open))
	for _, o := range open {
		fmt.Fprintf(stdout, "  %s %s %s path=%s\n", o.Kind, o.ID, o.Last, o.Path)
	}
	return 0
}

func smokeCmd(step string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("smoke "+step, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("POMAR_DATA_ROOT"), "data root")
	hostBin := fs.String("host-bin", "", "signed pomar-host binary")
	kernelSum := fs.String("kernel-sha256", "", "pinned sha256 of the extracted kernel (boot)")
	id := fs.String("id", "", "attempt id for the guest (boot)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *hostBin == "" || (step == "boot" && (*kernelSum == "" || *id == "")) {
		fmt.Fprint(stderr, usage)
		return 2
	}
	v, err := venue.Open(*root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	e := &smoke.Env{Venue: v, HostBin: *hostBin, Client: smoke.NewClient(), Out: stdout}
	ctx := context.Background()
	if step == "fetch-kernel" {
		err = smoke.FetchKernel(ctx, e)
	} else {
		err = smoke.Boot(ctx, e, *kernelSum, *id)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
