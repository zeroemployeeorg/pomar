// Command pomar is the coordinator CLI for isolated, per-attempt Linux
// micro-VMs on Apple silicon. Subcommands arrive one per change.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/zeroemployeeorg/pomar/internal/manager"
	"github.com/zeroemployeeorg/pomar/internal/proc"
	"github.com/zeroemployeeorg/pomar/internal/smoke"
	"github.com/zeroemployeeorg/pomar/internal/venue"
)

const version = "0.0.0-dev"

const usage = `usage:
  pomar version
  pomar venue status [-root DIR]    fill, open ledgered objects, unaccounted entries (exit 3)
  pomar venue init [-root DIR]      create the structure directories (idempotent)
  pomar venue classify [-root DIR] -kind K -id ID -class attempt|cache
                                    class an object ledgered before classes existed
  pomar manager [-root DIR] -host-bin PATH -kernel-sha256 HEX
                                    supervise helpers; reconcile on start; serve the socket
  pomar attempt start [-root DIR] -id ID -- CMD...
  pomar attempt list|reconcile [-root DIR]
  pomar attempt stop|rm [-root DIR] -id ID
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
	case len(args) >= 2 && args[0] == "venue" && (args[1] == "init" || args[1] == "classify"):
		return venueChange(args[1], args[2:], stdout, stderr)
	case len(args) >= 2 && args[0] == "smoke" && (args[1] == "fetch-kernel" || args[1] == "boot"):
		return smokeCmd(args[1], args[2:], stdout, stderr)
	case len(args) >= 1 && args[0] == "manager":
		return managerCmd(args[1:], stdout, stderr)
	case len(args) >= 2 && args[0] == "attempt":
		return attemptCmd(args[1], args[2:], stdout, stderr)
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

func managerCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("POMAR_DATA_ROOT"), "data root")
	hostBin := fs.String("host-bin", "", "signed pomar-host binary")
	kernelSum := fs.String("kernel-sha256", "", "pinned sha256 of the extracted kernel")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *hostBin == "" || *kernelSum == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	// Helper identity is matched against this exact path on every restart.
	bin, err := filepath.EvalSymlinks(*hostBin)
	if err == nil {
		bin, err = filepath.Abs(bin)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	v, err := venue.Open(*root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	e := &smoke.Env{Venue: v, HostBin: bin, Out: stdout}
	if err := smoke.VerifyKernel(e, *kernelSum); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	m, err := manager.Open(manager.Config{
		Venue:   v,
		HostBin: bin,
		Guest: manager.Guest{
			Kernel:  e.KernelPath(),
			InitRef: smoke.InitRepo + "@" + smoke.InitDigest, InitDigest: smoke.InitDigest,
			ImageRef: smoke.ImageRepo + "@" + smoke.ImageDigest, ImageDigest: smoke.ImageDigest,
		},
		Procs: proc.PS{},
		UID:   os.Getuid(),
		Log:   stdout,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer m.Close()
	for _, f := range m.Report() {
		fmt.Fprintf(stdout, "reconcile: %s attempt=%s pid=%d (%s)\n", f.Decision, f.Attempt, f.PID, f.Reason)
	}
	fmt.Fprintf(stdout, "manager: serving %s\n", manager.SocketPath(v.Root()))
	// SIGINT or SIGTERM stops the manager only; helpers keep running and
	// the next manager adopts them.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := m.Serve(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func attemptCmd(step string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("attempt "+step, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("POMAR_DATA_ROOT"), "data root")
	id := fs.String("id", "", "attempt id")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" {
		fmt.Fprintln(stderr, "attempt: data root not set")
		return 2
	}
	c := manager.NewClient(*root)
	var out any
	var err error
	switch step {
	case "start":
		cmd := fs.Args()
		if len(cmd) > 0 && cmd[0] == "--" {
			cmd = cmd[1:]
		}
		var e manager.Entry
		err = c.Do("POST", "/v1/attempts", manager.StartRequest{ID: *id, Command: cmd}, &e)
		out = e
	case "list":
		var l []manager.Entry
		err = c.Do("GET", "/v1/attempts", nil, &l)
		out = l
	case "reconcile":
		var r []manager.Finding
		err = c.Do("GET", "/v1/reconcile", nil, &r)
		out = r
	case "stop":
		var e manager.Entry
		err = c.Do("POST", "/v1/attempts/"+*id+"/stop", nil, &e)
		out = e
	case "rm":
		err = c.Do("DELETE", "/v1/attempts/"+*id, nil, nil)
		out = map[string]string{"removed": *id}
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(stdout, string(b))
	return 0
}
