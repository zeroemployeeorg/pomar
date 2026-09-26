// Command pomar is the coordinator CLI for isolated, per-attempt Linux
// micro-VMs on Apple silicon. Subcommands arrive one per change.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/base"
	"github.com/zeroemployeeorg/pomar/internal/boundary"
	"github.com/zeroemployeeorg/pomar/internal/capacity"
	"github.com/zeroemployeeorg/pomar/internal/debs"
	"github.com/zeroemployeeorg/pomar/internal/goproxy"
	"github.com/zeroemployeeorg/pomar/internal/manager"
	"github.com/zeroemployeeorg/pomar/internal/mirror"
	"github.com/zeroemployeeorg/pomar/internal/proc"
	"github.com/zeroemployeeorg/pomar/internal/result"
	"github.com/zeroemployeeorg/pomar/internal/sign"
	"github.com/zeroemployeeorg/pomar/internal/smoke"
	"github.com/zeroemployeeorg/pomar/internal/venue"
	"github.com/zeroemployeeorg/pomar/internal/volume"
)

const version = "0.0.0-dev"

const usage = `usage:
  pomar version
  pomar venue status [-root DIR]    fill, open ledgered objects, unaccounted entries (exit 3)
  pomar venue init [-root DIR]      create the structure directories (idempotent)
  pomar venue classify [-root DIR] -kind K -id ID -class attempt|cache
                                    class an object ledgered before classes existed
  pomar manager [-root DIR] -host-bin PATH -kernel-sha256 HEX [-shim-bin PATH]
                [-class-vcpu N -class-memory-mib M -class-disk-peak-gib G -class-concurrency N]
                [-ctl-socket PATH] [-sign-results] [-mirror-url NAME=URL]... [-class-job-user]
                                    with -shim-bin, attempts with a source get the Go module proxy
                                    supervise helpers; reconcile on start; serve the socket
  pomar attempt start [-root DIR | -socket PATH] -id ID [-class NAME] [-mirror NAME -ref REF [-git] [-readonly-source] [-input NAME=PATH]... [-output NAME]...] -- CMD...
                                    with a mirror, REF is pinned to a commit SHA at admission
                                    -output: a file the command leaves at /pomar/outputs/NAME, copied out when it exits
                                    -readonly-source: /work stays root-owned and not writable by the job
  pomar base build [-root DIR] -host-bin PATH
                                    unpack the pinned image once into a read-only base rootfs
  pomar mirror sync [-root DIR] -name NAME -url URL
  pomar mirror resolve [-root DIR] -name NAME -ref REF
  pomar attempt list|reconcile|vm-orphans [-root DIR]
                                    vm-orphans: VM services no live attempt accounts for (reported, never signalled)
  pomar attempt stop|rm|result [-root DIR | -socket PATH] -id ID
                                    result: the attempt's result document and signature
  pomar attempt pins [-root DIR | -socket PATH] -id ID
                                    the pins document, the same bytes the guest reads at /pomar/pins.json
  pomar attempt output [-root DIR | -socket PATH] -id ID -name NAME -o PATH
                                    write a copied-out output to PATH, checked against its recorded sha256
  pomar attempt signing-key [-root DIR | -socket PATH]
                                    the public key the manager signs results with
  pomar result verify -reply FILE -public-key BASE64
                                    exit 0 only if the result verifies against the pinned key
  pomar attempt capacity [-root DIR] the job class, slots, space and how many fit when idle
  pomar attempt caches|evict [-root DIR]
                                    cache budgets and planned evictions; evict applies them
  pomar volume create [-root DIR] -id ID -size-mib N
                                    a bounded APFS volume from a sparse image, for tests
  pomar volume rm [-root DIR] -id ID
  pomar smoke fetch-kernel [-root DIR] -host-bin PATH
  pomar smoke boot [-root DIR] -host-bin PATH -kernel-sha256 HEX -id ID
  pomar smoke boundaries [-root DIR | -socket PATH] -mirror NAME [-ref REF] [-class NAME] [-id ID]
                                    run the boundary probe as a job with a read-only source; grade it on
                                    the host; exit 0 only if all four checks pass

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
	case len(args) >= 2 && args[0] == "smoke" && args[1] == "boundaries":
		return boundariesCmd(args[2:], stdout, stderr)
	case len(args) >= 2 && args[0] == "smoke" && (args[1] == "fetch-kernel" || args[1] == "boot"):
		return smokeCmd(args[1], args[2:], stdout, stderr)
	case len(args) >= 1 && args[0] == "manager":
		return managerCmd(args[1:], stdout, stderr)
	case len(args) >= 2 && args[0] == "base" && args[1] == "build":
		return baseBuild(args[2:], stdout, stderr)
	case len(args) >= 2 && args[0] == "mirror" && (args[1] == "sync" || args[1] == "resolve"):
		return mirrorCmd(args[1], args[2:], stdout, stderr)
	case len(args) >= 2 && args[0] == "attempt":
		return attemptCmd(args[1], args[2:], stdout, stderr)
	case len(args) >= 2 && args[0] == "volume" && (args[1] == "create" || args[1] == "rm"):
		return volumeCmd(args[1], args[2:], stdout, stderr)
	case len(args) >= 2 && args[0] == "result" && args[1] == "verify":
		return resultVerify(args[2:], stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// resultVerify checks a result reply (as `pomar attempt result` prints it)
// against a pinned public key. It verifies the exact bytes the manager wrote.
func resultVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("result verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	replyFile := fs.String("reply", "", "a result reply, as `pomar attempt result` prints it")
	pub := fs.String("public-key", "", "the pinned public key, base64 (as `pomar attempt signing-key` prints it)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *replyFile == "" || *pub == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	b, err := os.ReadFile(*replyFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var r manager.ResultReply
	if err := json.Unmarshal(b, &r); err != nil {
		fmt.Fprintln(stderr, "result verify:", err)
		return 1
	}
	key, err := base64.StdEncoding.DecodeString(*pub)
	if err != nil {
		fmt.Fprintln(stderr, "result verify: public key:", err)
		return 1
	}
	sig, err := base64.StdEncoding.DecodeString(r.Signature)
	if err != nil || len(sig) == 0 {
		fmt.Fprintln(stderr, "result verify: the result is not signed")
		return 1
	}
	if !result.Verify(key, r.Result, sig) {
		fmt.Fprintln(stdout, "result verify: FAILED: the signature does not match this result and key")
		return 1
	}
	fmt.Fprintf(stdout, "result verify: ok (key %s)\n", result.KeyID(key))
	return 0
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
	if sp, err := v.Space(); err == nil {
		floor := "shared with " + venue.SystemData + ": admission keeps the fill floor"
		if !sp.Shared {
			floor = "a dedicated container: no fill floor, peak plus headroom only"
		}
		fmt.Fprintf(stdout, "disk: %s, %s\n", sp.Device, floor)
	}
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
	shimBin := fs.String("shim-bin", "", "static linux/arm64 pomar-shim; enables the Go module proxy")
	vcpu := fs.Int("class-vcpu", capacity.CI.VCPU, "the CI class's vCPU cap")
	memMiB := fs.Int64("class-memory-mib", capacity.CI.MemoryBytes>>20, "the CI class's memory cap in MiB")
	diskGiB := fs.Int64("class-disk-peak-gib", capacity.CI.DiskPeakBytes>>30, "the CI class's disk peak in GiB")
	concurrency := fs.Int("class-concurrency", 0, "the CI class's measured concurrency limit on this host (0: not measured here; slots only)")
	kernelSum := fs.String("kernel-sha256", "", "pinned sha256 of the extracted kernel")
	ctlSocket := fs.String("ctl-socket", "", "a second socket for the stream: start, stop and reads only (mode 0660; its directory must not be open to others)")
	var mirrorURLs map[string]string
	fs.Func("mirror-url", "NAME=URL: a mirror attempts may name, synced by the manager from URL at each start (repeatable; with none, mirrors are synced by hand)", func(s string) error {
		name, url, ok := strings.Cut(s, "=")
		if !ok || name == "" || url == "" {
			return fmt.Errorf("want NAME=URL")
		}
		if mirrorURLs == nil {
			mirrorURLs = map[string]string{}
		}
		if _, dup := mirrorURLs[name]; dup {
			return fmt.Errorf("mirror %q given twice", name)
		}
		mirrorURLs[name] = url
		return nil
	})
	jobUser := fs.Bool("class-job-user", false, "run starts with no source as the job user too, so the class never runs a job as root")
	signResults := fs.Bool("sign-results", false, "sign each result with the key in the data root's keys/ (created on first use); refused unless the manager runs as a role user")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *hostBin == "" || *kernelSum == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	// The stream's own manager never holds a key (r14 condition 4): refused
	// before anything else is opened.
	if *signResults {
		if err := result.CheckRoleUser(os.Getuid()); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
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
	var proxy *goproxy.Proxy
	if *shimBin != "" {
		abs, err := filepath.Abs(*shimBin)
		if err == nil {
			*shimBin = abs
			_, err = os.Stat(abs)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		// The module proxy's cache: ledgered before it exists, budgeted like
		// every cache.
		if err := v.EnsureCache(venue.KindVolume, "goproxy", "goproxy"); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		proxy = &goproxy.Proxy{Cache: filepath.Join(v.Root(), "goproxy")}
	}
	var signer *result.Signer
	if *signResults {
		if err := v.EnsureCache(venue.KindVolume, "keys", "keys"); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		signer, err = result.LoadOrCreate(filepath.Join(v.Root(), "keys"), os.Getuid())
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "manager: signing results with key %s\n", signer.ID)
	}
	m, err := manager.Open(manager.Config{
		Signer:     signer,
		MirrorURLs: mirrorURLs,
		JobUser:    *jobUser,
		Venue:      v,
		HostBin:    bin,
		Guest: manager.Guest{
			Kernel: e.KernelPath(), KernelSHA256: *kernelSum,
			InitRef: smoke.InitRepo + "@" + smoke.InitDigest, InitDigest: smoke.InitDigest,
			ImageRef: smoke.ImageRepo + "@" + smoke.ImageDigest, ImageDigest: smoke.ImageDigest,
			ImageArm64: smoke.ImageArm64, PackageSet: debs.SetHash(smoke.CIPackages),
		},
		Procs:   proc.PS{},
		Mirrors: &mirror.Mirrors{Venue: v},
		GoProxy: proxy,
		// Caps can be raised to measure a job; the class stays unmeasured
		// until a SOW states its figures.
		Class:   ciClass(*vcpu, *memMiB, *diskGiB, *concurrency),
		ShimBin: *shimBin,
		Base: func() (string, error) {
			// Clone the pinned image's base when one has been built.
			bs := &base.Bases{Venue: v, HostBin: bin, PackageSet: debs.SetHash(smoke.CIPackages)}
			if !bs.Exists(smoke.ImageArm64) {
				return "", nil
			}
			return bs.Verify(smoke.ImageArm64)
		},
		// The kernel and the pinned image's base are never evicted.
		Pinned: func() []string {
			p, _ := (&base.Bases{Venue: v, PackageSet: debs.SetHash(smoke.CIPackages)}).Path(smoke.ImageArm64)
			return []string{e.KernelPath(), p}
		},
		UID:       os.Getuid(),
		Log:       stdout,
		CtlSocket: *ctlSocket,
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
	if *ctlSocket != "" {
		fmt.Fprintf(stdout, "manager: control socket %s (the stream's routes only)\n", *ctlSocket)
	}
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
	socket := fs.String("socket", "", "talk to the manager on this socket instead of the data root's (for example the permanent manager's control socket)")
	id := fs.String("id", "", "attempt id")
	mirrorName := fs.String("mirror", "", "source mirror (start)")
	ref := fs.String("ref", "", "source ref, pinned to a commit SHA at admission (start)")
	gitSrc := fs.Bool("git", false, "give the guest a repository (a bundle of the branch -ref and of main) instead of a tree (start)")
	readonlySrc := fs.Bool("readonly-source", false, "leave /work root-owned and not writable by the job; it writes to its home, /tmp and /pomar/outputs (start; needs -mirror)")
	className := fs.String("class", "", "the job class to run in (start); empty is the manager's default")
	var outputs []string
	fs.Func("output", "NAME: a file the command leaves at /pomar/outputs/NAME, copied out when it exits (start; repeatable)", func(v string) error {
		outputs = append(outputs, v)
		return nil
	})
	outName := fs.String("name", "", "the output to fetch (output)")
	outPath := fs.String("o", "", "the file to write the output to; it must not exist (output)")
	var inputs []manager.Input
	fs.Func("input", "NAME=PATH: send a file, copied into the guest at /pomar/inputs/NAME (start; repeatable)", func(v string) error {
		name, path, ok := strings.Cut(v, "=")
		if !ok {
			return fmt.Errorf("want NAME=PATH")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		h := sha256.Sum256(b)
		inputs = append(inputs, manager.Input{Name: name, Data: b, SHA256: hex.EncodeToString(h[:])})
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var c *manager.Client
	switch {
	case *socket != "":
		c = manager.NewSocketClient(*socket)
	case *root != "":
		c = manager.NewClient(*root)
	default:
		fmt.Fprintln(stderr, "attempt: data root not set (or give -socket)")
		return 2
	}
	var out any
	var err error
	switch step {
	case "start":
		cmd := fs.Args()
		if len(cmd) > 0 && cmd[0] == "--" {
			cmd = cmd[1:]
		}
		var e manager.Entry
		if *readonlySrc && *mirrorName == "" {
			fmt.Fprintln(stderr, "attempt start: -readonly-source needs -mirror and -ref")
			return 2
		}
		req := manager.StartRequest{ID: *id, Command: cmd, Inputs: inputs, Class: *className, Outputs: outputs}
		if *mirrorName != "" || *ref != "" {
			if *mirrorName == "" || *ref == "" {
				fmt.Fprintln(stderr, "attempt start: -mirror and -ref go together")
				return 2
			}
			req.Source = &manager.Source{Mirror: *mirrorName, Ref: *ref, Git: *gitSrc, ReadOnly: *readonlySrc}
		}
		err = c.Do("POST", "/v1/attempts", req, &e)
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
	case "pins":
		// The document's exact bytes, not re-encoded: they are what the
		// guest reads at /pomar/pins.json.
		b, perr := c.Pins(*id)
		if perr != nil {
			fmt.Fprintln(stderr, perr)
			return 1
		}
		stdout.Write(b)
		return 0
	case "output":
		if *outName == "" || *outPath == "" {
			fmt.Fprintln(stderr, "attempt output: -name and -o are required")
			return 2
		}
		sum, oerr := fetchOutput(c, *id, *outName, *outPath)
		if oerr != nil {
			fmt.Fprintln(stderr, oerr)
			return 1
		}
		out = map[string]string{"output": *outName, "path": *outPath, "sha256": sum}
	case "result":
		var r manager.ResultReply
		err = c.Do("GET", "/v1/attempts/"+*id+"/result", nil, &r)
		out = r
	case "signing-key":
		var k manager.KeyReply
		err = c.Do("GET", "/v1/signing-key", nil, &k)
		out = k
	case "vm-orphans":
		var o []manager.VMOrphan
		err = c.Do("GET", "/v1/vm-orphans", nil, &o)
		out = o
	case "capacity":
		var cp manager.Capacity
		err = c.Do("GET", "/v1/capacity", nil, &cp)
		out = cp
	case "caches", "evict":
		var rep manager.CacheReport
		if step == "caches" {
			err = c.Do("GET", "/v1/caches", nil, &rep)
		} else {
			err = c.Do("POST", "/v1/caches/evict", nil, &rep)
		}
		out = rep
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

func mirrorCmd(step string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mirror "+step, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("POMAR_DATA_ROOT"), "data root")
	name := fs.String("name", "", "mirror name")
	url := fs.String("url", "", "repository URL to mirror (sync)")
	ref := fs.String("ref", "", "ref to resolve (resolve)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" || (step == "sync" && *url == "") || (step == "resolve" && *ref == "") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	v, err := venue.Open(*root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	m := &mirror.Mirrors{Venue: v}
	ctx := context.Background()
	if step == "sync" {
		if err := m.Sync(ctx, *name, *url); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "mirror %s: synced\n", *name)
		return 0
	}
	sha, err := m.Resolve(ctx, *name, *ref)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, sha)
	return 0
}

func baseBuild(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("base build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("POMAR_DATA_ROOT"), "data root")
	hostBin := fs.String("host-bin", "", "signed pomar-host binary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *hostBin == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err := sign.Check(*hostBin); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	v, err := venue.Open(*root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := v.EnsureCache(venue.KindImage, "image-store", "store"); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	// The CI class's pinned packages, fetched once, checked, and unpacked into
	// the base after the image; guests gain no network path for them.
	layers, err := (&debs.Cache{Venue: v}).DataArchives(context.Background(), smoke.CIPackages)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	bs := &base.Bases{Venue: v, HostBin: *hostBin, PackageSet: debs.SetHash(smoke.CIPackages), Layers: layers}
	err = bs.Build(context.Background(), filepath.Join(v.Root(), "store"),
		smoke.ImageRepo+"@"+smoke.ImageDigest, smoke.ImageDigest, smoke.ImageArm64, smoke.BaseSizeBytes)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	p, _ := bs.Path(smoke.ImageArm64)
	fmt.Fprintf(stdout, "base %s: built at %s\n", smoke.ImageArm64, p)
	return 0
}

func volumeCmd(step string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("volume "+step, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("POMAR_DATA_ROOT"), "data root")
	id := fs.String("id", "", "volume id")
	sizeMiB := fs.Int64("size-mib", 0, "size in MiB (create)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *id == "" || (step == "create" && *sizeMiB <= 0) {
		fmt.Fprint(stderr, usage)
		return 2
	}
	v, err := venue.Open(*root)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	vs := &volume.Volumes{Venue: v}
	ctx := context.Background()
	if step == "create" {
		mnt, err := vs.Create(ctx, *id, *sizeMiB*volume.MiB)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "volume %s: mounted at %s\n", *id, mnt)
		return 0
	}
	if err := vs.Remove(ctx, *id); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "volume %s: removed\n", *id)
	return 0
}

// ciClass is the CI class with the manager's settings. The class stays
// measured only at the figures the elders set; any other caps are a
// measurement run's, and no capacity claim is made from them.
func ciClass(vcpu int, memMiB, diskGiB int64, concurrency int) capacity.Class {
	c := capacity.CI
	c.Concurrency = concurrency
	if vcpu != c.VCPU || memMiB<<20 != c.MemoryBytes || diskGiB<<30 != c.DiskPeakBytes {
		c.VCPU, c.MemoryBytes, c.DiskPeakBytes, c.Measured = vcpu, memMiB<<20, diskGiB<<30, false
	}
	return c
}

// fetchOutput writes a copied-out output to path, which must not exist: into
// a file beside it first, renamed into place only once its sha256 matches
// what the manager recorded.
func fetchOutput(c *manager.Client, id, name, path string) (string, error) {
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("attempt output: %s exists", path)
	}
	f, err := os.OpenFile(path+".part", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	sum, err := c.Output(id, name, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if _, serr := os.Lstat(path); serr == nil {
			err = fmt.Errorf("attempt output: %s exists", path)
		} else {
			err = os.Rename(path+".part", path)
		}
	}
	if err != nil {
		os.Remove(path + ".part")
		return "", err
	}
	return sum, nil
}

// boundariesCmd is the guest boundary self-test (POMAR-SOW-06 §4.3): it runs
// the fixed probe as a real attempt with a read-only source, fetches its
// report through copy-out (checked against its sha256), and grades it here.
func boundariesCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("smoke boundaries", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", os.Getenv("POMAR_DATA_ROOT"), "data root")
	socket := fs.String("socket", "", "talk to the manager on this socket instead of the data root's")
	mirrorName := fs.String("mirror", "", "the source mirror the probe runs against (read-only)")
	ref := fs.String("ref", "main", "the source ref")
	className := fs.String("class", "", "the job class to test; empty is the manager's default")
	id := fs.String("id", "boundaries-"+time.Now().UTC().Format("20060102-150405"), "attempt id")
	wait := fs.Duration("timeout", 10*time.Minute, "how long to wait for the attempt to end")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *mirrorName == "" {
		fmt.Fprintln(stderr, "smoke boundaries: -mirror is required: the read-only check needs a source")
		return 2
	}
	var c *manager.Client
	switch {
	case *socket != "":
		c = manager.NewSocketClient(*socket)
	case *root != "":
		c = manager.NewClient(*root)
	default:
		fmt.Fprintln(stderr, "smoke boundaries: data root not set (or give -socket)")
		return 2
	}
	probe := []byte(boundary.Probe)
	h := sha256.Sum256(probe)
	req := manager.StartRequest{
		ID: *id, Class: *className, Command: []string{"/bin/sh", "/pomar/inputs/probe.sh"},
		Source:  &manager.Source{Mirror: *mirrorName, Ref: *ref, ReadOnly: true},
		Inputs:  []manager.Input{{Name: "probe.sh", Data: probe, SHA256: hex.EncodeToString(h[:])}},
		Outputs: []string{boundary.ReportName},
	}
	var e manager.Entry
	if err := c.Do("POST", "/v1/attempts", req, &e); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	deadline := time.Now().Add(*wait)
	for !e.Terminal() {
		if time.Now().After(deadline) {
			fmt.Fprintf(stderr, "smoke boundaries: %s did not end within %s\n", *id, *wait)
			return 1
		}
		time.Sleep(2 * time.Second)
		var l []manager.Entry
		if err := c.Do("GET", "/v1/attempts", nil, &l); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		for _, x := range l {
			if x.Attempt == *id {
				e = x
			}
		}
	}
	var report bytes.Buffer
	sum, err := c.Output(*id, boundary.ReportName, &report)
	if err != nil {
		fmt.Fprintf(stderr, "smoke boundaries: %s ended %s with no report: %v\n", *id, e.State, err)
		return 1
	}
	g := boundary.Grade(report.String())
	b, _ := json.MarshalIndent(map[string]any{
		"attempt": *id, "state": e.State, "exit_code": e.ExitCode, "report_sha256": sum,
		"pass": g.Pass, "checks": g.Checks,
	}, "", "  ")
	fmt.Fprintln(stdout, string(b))
	if !g.Pass {
		return 1
	}
	return 0
}
