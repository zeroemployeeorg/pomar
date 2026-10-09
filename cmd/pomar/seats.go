package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/zeroemployeeorg/pomar/internal/localclient"
	"github.com/zeroemployeeorg/pomar/internal/seatapi"
)

// The operator's seat commands (POMAR-CC SOW 15 §4): `pomar seats` lists
// every declared seat and, on a terminal, offers a numbered menu; `pomar
// seat status SEAT` prints one seat's record as JSON. They read the
// development host's seat routes over its owner-only socket and change
// nothing. Starting, attaching and stopping a seat come with the routes
// that do them; until then the menu says so instead of pretending.

func seatHostRoot(fs *flag.FlagSet) *string {
	return fs.String("root", os.Getenv("POMAR_AGENT_HOST_ROOT"), "the development host's data root (its host.sock is used)")
}

func seatGet(root, path string, out any) error {
	if root == "" {
		return errors.New("-root (or POMAR_AGENT_HOST_ROOT) names the development host's data root")
	}
	client := localclient.New(filepath.Join(root, "host.sock"), os.Geteuid())
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://owner" + path)
	if err != nil {
		return fmt.Errorf("the development host can't be reached: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(body, &e)
		if resp.StatusCode == http.StatusNotFound && e.Error == "" {
			return errors.New("this host serves no seat routes (its configuration has no seatRoot)")
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error)
	}
	return json.Unmarshal(body, out)
}

func seatsCmd(args []string, stdin io.Reader, interactive bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("seats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := seatHostRoot(fs)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	var list struct {
		Seats []seatapi.Seat `json:"seats"`
	}
	if err := seatGet(*root, "/v1/seats", &list); err != nil {
		fmt.Fprintln(stderr, "pomar seats:", err)
		return 1
	}
	if len(list.Seats) == 0 {
		fmt.Fprintln(stdout, "No seats are declared on this host.")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tSEAT\tROLE\tPROVIDER ACCOUNT\tLOCATION\tDECLARATION")
	for i, s := range list.Seats {
		where := "none recorded"
		if s.Location != nil {
			where = fmt.Sprintf("%s (rev %d)", s.Location.Where, s.Location.Revision)
			if !s.LocationCurrent {
				where += ", names an older declaration"
			}
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\trev %d, %s\n", i+1, s.Seat, s.Role, s.ProviderAccount, where, s.DeclarationRevision, s.DeclarationSHA256[:12])
	}
	tw.Flush()
	if !interactive {
		return 0
	}
	fmt.Fprint(stdout, "\nPick a seat by number (Enter to quit): ")
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return 0
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(list.Seats) {
		fmt.Fprintln(stderr, "pomar seats: no such seat number")
		return 2
	}
	s := list.Seats[n-1]
	fmt.Fprintf(stdout, "\n%s: role %s, provider account %s.\n", s.Seat, s.Role, s.ProviderAccount)
	fmt.Fprintf(stdout, "Starting and attaching to a seat aren't available on this host yet (POMAR-CC SOW 15 step 1 continues).\nIts record: pomar seat status %s\n", s.Seat)
	return 0
}

func seatCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "status" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("seat status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := seatHostRoot(fs)
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var out json.RawMessage
	if err := seatGet(*root, "/v1/seats/"+fs.Arg(0), &out); err != nil {
		fmt.Fprintln(stderr, "pomar seat status:", err)
		return 1
	}
	stdout.Write(out)
	return 0
}

func onTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
