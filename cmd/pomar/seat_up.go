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
//	pomar seat up SEAT -records REC -here WHERE
//	    creates the seat's environment from that profile if it doesn't
//	    exist, and starts it if it isn't running; a running seat is left
//	    alone. It refuses unless the seat's location names WHERE and its
//	    current declaration, so one seat never runs in two places.
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
	raw, err := os.ReadFile(*classes)
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat profile:", err)
		return 1
	}
	cat, err := seatdecl.ParseClasses(raw)
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat profile:", err)
		return 1
	}
	compiled, err := seatdecl.Compile(r.Declaration, r.Seat.DeclarationSHA256, cat)
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat profile:", err)
		return 1
	}
	p := compiled.Profile
	p.Seat = r.Declaration.Seat
	for _, s := range compiled.Sources {
		if s.Role == "work" {
			p.SourceBundle, p.SourceSHA = *bundle, s.Commit
		}
	}
	out, _ := json.MarshalIndent(map[string]agentenv.EnvironmentProfile{seatEnvironment(r.Declaration.Seat): p}, "", "  ")
	stdout.Write(append(out, '\n'))
	return 0
}

type environmentRecord struct {
	Environment struct {
		Phase string `json:"phase"`
		Spec  struct {
			Profile     string `json:"profile"`
			Session     string `json:"session"`
			Incarnation string `json:"incarnation"`
		} `json:"spec"`
	} `json:"environment"`
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
	here := fs.String("here", "", "this host's location name, as the seat's location names it (for example pomar:macbook)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *records == "" || *here == "" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	seat := fs.Arg(0)
	r, err := seatLookup(*root, seat)
	if err != nil {
		fmt.Fprintln(stderr, "pomar seat up:", err)
		return 1
	}
	switch {
	case r.Seat.Location == nil:
		fmt.Fprintf(stderr, "pomar seat up: %s has no recorded location; it doesn't start until its location names %s\n", seat, *here)
		return 1
	case r.Seat.Location.Where != *here:
		fmt.Fprintf(stderr, "pomar seat up: %s is active at %s, not %s; it never runs in two places (move its location first)\n", seat, r.Seat.Location.Where, *here)
		return 1
	case !r.Seat.LocationCurrent:
		fmt.Fprintf(stderr, "pomar seat up: %s's location names an older declaration; move it to the current one first\n", seat)
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
