package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
	"github.com/zeroemployeeorg/pomar/internal/localclient"
	"github.com/zeroemployeeorg/pomar/internal/ownerclient"
	"github.com/zeroemployeeorg/pomar/internal/seatapi"
	"github.com/zeroemployeeorg/pomar/internal/seatdecl"
)

// The seat lifecycle commands (POMAR-CC SOW 15 §4):
//
//	pomar seat profile SEAT -classes FILE -source-bundle FILE
//	    compiles the seat's current declaration against the owner's seat
//	    class catalogue into the profile the owner adds to the host's
//	    configuration, as {"seat-SEAT": {...}}
//	pomar seat up SEAT -records REC -classes FILE [-here WHERE]
//	    creates the seat's environment from that profile if it doesn't
//	    exist, and starts it if it isn't running; a running seat is left
//	    alone. It refuses unless the seat's location names this host, as
//	    the host's configuration states it (seatHere; -here only states
//	    what the caller expects), and its current declaration, so one seat
//	    never runs in two places.
//	pomar seat stop SEAT -records REC
//	    stops the seat's environment, keeping its workspace.
//
// Every action goes through the owner client's operation journal in REC
// with an operation ID derived from what it acts on, so a retry carries the
// same ID and never acts twice.

func seatEnvironment(seat string) string { return "seat-" + seat }

type seatRecord struct {
	Seat        seatapi.Seat         `json:"seat"`
	Declaration seatdecl.Declaration `json:"declaration"`
}

func seatLookup(root, seat string) (seatRecord, error) {
	var r seatRecord
	err := seatGet(root, "/v1/seats/"+seat, &r)
	return r, err
}

func seatProfileCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("seat profile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := seatHostRoot(fs)
	classes := fs.String("classes", "", "the owner's seat class catalogue (pomar.seat-classes/v1)")
	bundle := fs.String("source-bundle", "", "the owner's verified source artifact for the seat's work source")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *classes == "" || !filepath.IsAbs(*bundle) {
		fmt.Fprint(stderr, usage)
		return 2
	}
	r, err := seatLookup(*root, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat profile:", err)
		return 1
	}
	p, err := seatProfile(r, *classes, *bundle)
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat profile:", err)
		return 1
	}
	out, _ := json.MarshalIndent(map[string]agentenv.EnvironmentProfile{seatEnvironment(r.Declaration.Seat): p}, "", "  ")
	stdout.Write(append(out, '\n'))
	return 0
}

// seatProfile compiles the seat's current declaration against the owner's
// catalogue into its profile, bound to the declaration's digest. An
// environment carries only the sources in carriedSources, so a declaration
// with any other is refused rather than started without it.
func seatProfile(r seatRecord, classes, bundle string) (agentenv.EnvironmentProfile, error) {
	raw, err := os.ReadFile(classes)
	if err != nil {
		return agentenv.EnvironmentProfile{}, err
	}
	cat, err := seatdecl.ParseClasses(raw)
	if err != nil {
		return agentenv.EnvironmentProfile{}, err
	}
	compiled, err := seatdecl.Compile(r.Declaration, r.Seat.DeclarationSHA256, cat)
	if err != nil {
		return agentenv.EnvironmentProfile{}, err
	}
	var work *seatdecl.Binding
	for i, s := range compiled.Sources {
		if !slices.Contains(carriedSources, s.Role) {
			return agentenv.EnvironmentProfile{}, fmt.Errorf("%s declares a %s source; an environment carries only %v so far, and a seat isn't started without a source it declares", r.Declaration.Seat, s.Role, carriedSources)
		}
		if s.Role == "work" {
			work = &compiled.Sources[i]
		}
	}
	if work == nil {
		return agentenv.EnvironmentProfile{}, errors.New("the declaration has no work source")
	}
	p := compiled.Profile
	p.Seat, p.SeatDeclarationSHA256 = r.Declaration.Seat, compiled.DeclarationSHA256
	p.SourceBundle, p.SourceSHA = bundle, work.Commit
	return p, nil
}

// carriedSources are the source roles an environment receives. The
// declaration's digest, which the profile carries, binds every source in
// order; the work source is also bound by its commit.
var carriedSources = []string{"work"}

// staleness lists where an environment's spec differs from the profile the
// seat's current declaration compiles to; empty means it is current.
func staleness(spec environmentSpec, want agentenv.EnvironmentProfile) []string {
	var d []string
	diff := func(field, got, want string) {
		if got != want {
			d = append(d, fmt.Sprintf("%s is %q, the declaration says %q", field, got, want))
		}
	}
	diff("seatDeclarationSHA256", spec.SeatDeclarationSHA256, want.SeatDeclarationSHA256)
	diff("seat", spec.Seat, want.Seat)
	diff("sourceSHA", spec.SourceSHA, want.SourceSHA)
	diff("base", spec.Base, want.Base)
	diff("imageRef", spec.ImageRef, want.ImageRef)
	diff("imageDigest", spec.ImageDigest, want.ImageDigest)
	diff("agent", spec.Agent, want.Agent)
	diff("agentVersion", spec.AgentVersion, want.AgentVersion)
	diff("agentArchiveSHA256", spec.CodexArchiveSHA256, want.AgentArchiveSHA256)
	diff("allowedHosts", strings.Join(spec.AllowedHosts, ","), strings.Join(want.AllowedHosts, ","))
	diff("controllerCapabilities", strings.Join(spec.ControllerCapabilities, ","), strings.Join(want.ControllerCapabilities, ","))
	return d
}

type environmentRecord struct {
	Environment struct {
		Phase string          `json:"phase"`
		Spec  environmentSpec `json:"spec"`
	} `json:"environment"`
}

// environmentSpec is what `seat up` reads of an environment's spec: its
// identity, and the inputs it was made from, which must be the ones the
// seat's current declaration compiles to.
type environmentSpec struct {
	Profile                string   `json:"profile"`
	Session                string   `json:"session"`
	Incarnation            string   `json:"incarnation"`
	Seat                   string   `json:"seat"`
	SeatDeclarationSHA256  string   `json:"seatDeclarationSHA256"`
	SourceSHA              string   `json:"sourceSHA"`
	Base                   string   `json:"base"`
	ImageRef               string   `json:"imageRef"`
	ImageDigest            string   `json:"imageDigest"`
	Agent                  string   `json:"agent"`
	AgentVersion           string   `json:"agentVersion"`
	CodexArchiveSHA256     string   `json:"codexArchiveSHA256"`
	AllowedHosts           []string `json:"allowedHosts"`
	ControllerCapabilities []string `json:"controllerCapabilities"`
}

// environment reads the seat's environment; ok is false when it doesn't
// exist.
func environment(root, id string) (environmentRecord, bool, error) {
	client := localclient.New(filepath.Join(root, "host.sock"), os.Geteuid())
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://owner/v1/environments/" + id)
	if err != nil {
		return environmentRecord{}, false, fmt.Errorf("the development host can't be reached: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusNotFound {
		return environmentRecord{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return environmentRecord{}, false, fmt.Errorf("HTTP %d reading %s", resp.StatusCode, id)
	}
	var e environmentRecord
	if err := json.Unmarshal(body, &e); err != nil {
		return environmentRecord{}, false, err
	}
	return e, true, nil
}

// createEnvironment creates the seat's environment from its profile, with
// an operation ID bound to the declaration, so a retry is the same create.
func createEnvironment(root, id, profile, op string) error {
	client := localclient.New(filepath.Join(root, "host.sock"), os.Geteuid())
	defer client.CloseIdleConnections()
	b, _ := json.Marshal(map[string]string{"id": id, "operation_id": op, "profile": profile})
	resp, err := client.Post("http://owner/v1/environments", "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("create %s: outcome unknown; run it again, it's the same operation: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		return fmt.Errorf("create %s: HTTP %d: %s", id, resp.StatusCode, e.Error)
	}
	return nil
}

// seatAction starts or stops the seat's environment through the owner
// client's journal, under an operation ID bound to the incarnation it acts
// on.
func seatAction(root, records, id, action string, e environmentRecord) (ownerclient.Receipt, error) {
	op := "seat-" + action + "-" + e.Environment.Spec.Incarnation
	dir := filepath.Join(records, ".requests")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ownerclient.Receipt{}, err
	}
	req := filepath.Join(dir, op+".json")
	body, _ := json.Marshal(map[string]string{"operation_id": op, "expected_incarnation": e.Environment.Spec.Incarnation})
	if prior, err := os.ReadFile(req); err == nil {
		if !bytes.Equal(prior, body) {
			return ownerclient.Receipt{}, errors.New("a different request is recorded for this operation; inspect it")
		}
	} else if err := os.WriteFile(req, body, 0o600); err != nil {
		return ownerclient.Receipt{}, err
	}
	return ownerclient.Run(ownerclient.Config{Root: root, Records: records, Environment: id, Action: action, OperationID: op,
		Request: req, Session: e.Environment.Spec.Session, Incarnation: e.Environment.Spec.Incarnation})
}

func seatUpCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("seat up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := seatHostRoot(fs)
	records := fs.String("records", "", "the owner's private (0700) operation records directory")
	here := fs.String("here", "", "optional: the location name you expect this host to have; refused if the host's configuration says otherwise")
	classes := fs.String("classes", "", "the owner's seat class catalogue (pomar.seat-classes/v1), as the seat's profile was compiled against")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *records == "" || *classes == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	seat := fs.Arg(0)
	r, err := seatLookup(*root, seat)
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat up:", err)
		return 1
	}
	// This host's location is its owner's statement, from the host's
	// configuration (seatHere), never the caller's flag: -here only states
	// what the caller expects.
	host := r.Seat.HostLocation
	switch {
	case host == "":
		fmt.Fprintln(stderr, "pomar seat up: this host names no seat location (seatHere in its configuration); no seat starts here until it does")
		return 1
	case *here != "" && *here != host:
		fmt.Fprintf(stderr, "pomar seat up: this host is %s, not %s as you expected\n", host, *here)
		return 1
	case r.Seat.Location == nil:
		fmt.Fprintf(stderr, "pomar seat up: %s has no recorded location; it doesn't start until its location names %s\n", seat, host)
		return 1
	case r.Seat.Location.Where != host:
		fmt.Fprintf(stderr, "pomar seat up: %s is active at %s, not %s; it never runs in two places (move its location first)\n", seat, r.Seat.Location.Where, host)
		return 1
	case !r.Seat.LocationCurrent:
		fmt.Fprintf(stderr, "pomar seat up: %s's location names an older declaration; move it to the current one first\n", seat)
		return 1
	}
	want, err := seatProfile(r, *classes, "")
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat up:", err)
		return 1
	}
	id := seatEnvironment(seat)
	e, ok, err := environment(*root, id)
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat up:", err)
		return 1
	}
	if !ok {
		op := "seat-create-" + seat + "-" + r.Seat.DeclarationSHA256[:16]
		if err := createEnvironment(*root, id, id, op); err != nil {
			fmt.Fprintln(stderr, "pomar seat up:", err)
			return 1
		}
		if e, ok, err = environment(*root, id); err != nil || !ok {
			fmt.Fprintln(stderr, "pomar seat up: the created environment can't be read back:", err)
			return 1
		}
	}
	if e.Environment.Spec.Profile != id {
		fmt.Fprintf(stderr, "pomar seat up: environment %s exists with profile %q, not the seat's; inspect it\n", id, e.Environment.Spec.Profile)
		return 1
	}
	// An environment made from another declaration, source or catalogue is
	// never reported as the seat, nor started: it is left as it is, its
	// incarnation kept, for the owner to stop and retire before a new one.
	if d := staleness(e.Environment.Spec, want); len(d) > 0 {
		fmt.Fprintf(stderr, "pomar seat up: environment %s (%s, incarnation %s) isn't the seat's current declaration; it is left as it is:\n  %s\nRecompile the profile (pomar seat profile), update the host's configuration, and retire this environment first.\n",
			id, e.Environment.Phase, e.Environment.Spec.Incarnation, strings.Join(d, "\n  "))
		return 1
	}
	if e.Environment.Phase == "running" {
		fmt.Fprintf(stdout, "%s is running (environment %s). Attach: pomar seat attach -environment %s\n", seat, id, id)
		return 0
	}
	receipt, err := seatAction(*root, *records, id, "start", e)
	json.NewEncoder(stdout).Encode(receipt)
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat up:", err)
		return 1
	}
	return 0
}

func seatStopCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("seat stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := seatHostRoot(fs)
	records := fs.String("records", "", "the owner's private (0700) operation records directory")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *records == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	id := seatEnvironment(fs.Arg(0))
	e, ok, err := environment(*root, id)
	if err != nil || !ok {
		fmt.Fprintln(stderr, "pomar seat stop: no environment for this seat", err)
		return 1
	}
	if e.Environment.Phase != "running" {
		fmt.Fprintf(stdout, "%s isn't running (phase %s).\n", id, e.Environment.Phase)
		return 0
	}
	receipt, err := seatAction(*root, *records, id, "stop", e)
	json.NewEncoder(stdout).Encode(receipt)
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat stop:", err)
		return 1
	}
	return 0
}
