// Command pomar is the coordinator CLI for isolated, per-attempt Linux
// micro-VMs on Apple silicon. Subcommands arrive one per change.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/zeroemployeeorg/pomar/internal/venue"
)

const version = "0.0.0-dev"

const usage = `usage:
  pomar version
  pomar venue status [-root DIR]    fill, open ledgered objects, unaccounted entries (exit 3)
  pomar venue init [-root DIR]      create the structure directories (idempotent)
  pomar venue classify [-root DIR] -kind K -id ID -class attempt|cache
                                    class an object ledgered before classes existed

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
	case len(args) >= 2 && args[0] == "venue" && (args[1] == "init" || args[1] == "classify"):
		return venueChange(args[1], args[2:], stdout, stderr)
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
		class := string(o.Class)
		if class == "" {
			class = "UNCLASSIFIED"
		}
		fmt.Fprintf(stdout, "  %s %s %s %s path=%s\n", o.Kind, class, o.ID, o.Last, o.Path)
	}
	un, err := v.Unaccounted()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "unaccounted: %d\n", len(un))
	for _, p := range un {
		fmt.Fprintf(stdout, "  %s\n", p)
	}
	if len(un) > 0 {
		return 3
	}
	return 0
}

func venueChange(step string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("venue "+step, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("POMAR_DATA_ROOT"), "data root")
	kind := fs.String("kind", "", "object kind (classify)")
	id := fs.String("id", "", "object id (classify)")
	class := fs.String("class", "", "attempt or cache (classify)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	v, err := venue.Open(*root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if step == "init" {
		err = v.Init()
	} else {
		err = v.Classify(venue.Kind(*kind), *id, venue.Class(*class))
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "venue %s: ok\n", step)
	return 0
}
