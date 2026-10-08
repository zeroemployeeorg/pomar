// Command pomar-agent-login signs a Claude Code agent environment in, or
// renews its sign-in, over the development host's owner-only socket:
//
//	pomar-agent-login -root DIR -environment ID
//	    prints the authorization URL, reads the code (not echoed), completes
//	    the sign-in and reports whether the environment is authenticated
//	pomar-agent-login -root DIR -environment ID -start -operation-id OP
//	    prints {"operation_id","login_id","auth_url"} as JSON and leaves the
//	    sign-in waiting
//	pomar-agent-login -root DIR -environment ID -complete LOGIN_ID -operation-id OP
//	    reads the code from stdin and completes that waiting sign-in
//	pomar-agent-login -root DIR -environment ID -status
//	    prints whether the environment is authenticated
//
// -start and -complete are for automation, so they require -operation-id: the
// caller supplies the operation ID and keeps it before the request, and a
// retry carries the same ID. Retrying with the same ID and input returns the
// broker's retained record and never signs in again; the same ID with other
// input, or for the other action, is refused by the broker. A retained record
// is never reported as success, whatever its state: the broker keeps whether
// the request was sent, not whether the sign-in succeeded, and the account
// may be authenticated by another sign-in. A retained start's authorization
// URL is not journaled, so it is never returned again. An uncertain original
// is to be inspected, not reissued under a new operation ID. The interactive
// form makes its own operation IDs, and never recovers an existing operation.
//
// The code goes only to the environment's actor: it is never printed,
// logged or journaled, and no credential ever leaves the environment.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "pomar-agent-login:", err)
		os.Exit(1)
	}
}

type client struct {
	http *http.Client
	base string
}

func newClient(socket, environment string) *client {
	return &client{
		http: &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		}}},
		base: "http://host/v1/environments/" + environment + "/agent/",
	}
}

func (c *client) do(method, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &e)
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, e.Error)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

type session struct {
	Authenticated bool `json:"authenticated"`
	Session       struct {
		Incarnation string `json:"incarnation"`
	} `json:"session"`
}

func (c *client) session() (session, error) {
	var s session
	err := c.do("GET", "session", nil, &s)
	return s, err
}

func operationID(kind string) string {
	return fmt.Sprintf("%s-%d", kind, time.Now().UTC().UnixNano())
}

// validOperation is the broker's rule for an operation ID.
var validOperation = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// retained is the broker's reply for an operation ID it already recorded:
// that operation's retained control record, not a new dispatch.
type retained struct {
	OperationID string `json:"operation_id"`
	State       string `json:"state"`
}

// Error is the only way a retained record is reported: never as success.
func (r retained) Error() string {
	switch r.State {
	case "acceptance_unknown":
		return fmt.Sprintf("operation %s is already recorded and its acceptance is uncertain: inspect -status, and do not reissue it under a new operation ID", r.OperationID)
	case "dispatching":
		return fmt.Sprintf("operation %s is already recorded and still dispatching; it was not repeated and its outcome is not known yet", r.OperationID)
	}
	return fmt.Sprintf("operation %s is already recorded (state %s) and was not repeated; its outcome is not retained, so this is not a success: inspect -status", r.OperationID, r.State)
}

// reply is the broker's login reply: a new dispatch carries "login", and a
// retained operation is its control record.
type reply struct {
	Login json.RawMessage `json:"login"`
	retained
}

func (c *client) start(incarnation, op string) (id, url string, err error) {
	var out reply
	if err = c.do("POST", "login", map[string]string{"expected_incarnation": incarnation, "operation_id": op}, &out); err != nil {
		return "", "", err
	}
	if len(out.Login) == 0 && out.OperationID != "" {
		// The URL was never journaled, so a retained start can't return it.
		return "", "", out.retained
	}
	var login struct {
		LoginID string `json:"loginId"`
		AuthURL string `json:"authUrl"`
	}
	json.Unmarshal(out.Login, &login)
	if login.LoginID == "" || !strings.HasPrefix(login.AuthURL, "https://") {
		return "", "", errors.New("the environment returned no authorization URL (is it a Claude Code environment?)")
	}
	return login.LoginID, login.AuthURL, nil
}

func (c *client) complete(incarnation, op, id, code string) (bool, error) {
	var out reply
	if err := c.do("POST", "login/complete", map[string]string{"expected_incarnation": incarnation, "operation_id": op, "login_id": id, "code": code}, &out); err != nil {
		return false, err
	}
	if len(out.Login) == 0 && out.OperationID != "" {
		return false, out.retained
	}
	var login struct {
		Success bool `json:"success"`
	}
	json.Unmarshal(out.Login, &login)
	return login.Success, nil
}

// readCode reads one line; on a terminal, without echo.
func readCode(in *os.File, prompt io.Writer) (string, error) {
	if fi, err := in.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(prompt, "Paste the code from the sign-in page (not shown): ")
		stty := func(arg string) { c := exec.Command("/bin/stty", arg); c.Stdin = in; c.Run() }
		stty("-echo")
		defer func() { stty("echo"); fmt.Fprintln(prompt) }()
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no code read")
	}
	return strings.TrimSpace(line), nil
}

func run(args []string, stdin *os.File, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pomar-agent-login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "the development host's data root (its host.sock is used)")
	environment := fs.String("environment", "", "the agent environment's ID")
	startOnly := fs.Bool("start", false, "print the operation ID, login ID and authorization URL as JSON, and leave the sign-in waiting")
	complete := fs.String("complete", "", "complete the waiting sign-in with this login ID, reading the code from stdin")
	status := fs.Bool("status", false, "print whether the environment is authenticated")
	opID := fs.String("operation-id", "", "required with -start or -complete: the request's operation ID, supplied and kept by the caller and reused on retries")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *root == "" || *environment == "" {
		return errors.New("-root and -environment are required")
	}
	modes := 0
	for _, on := range []bool{*startOnly, *complete != "", *status} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("-start, -complete and -status are exclusive")
	}
	if (*startOnly || *complete != "") && *opID == "" {
		return errors.New("-start and -complete require -operation-id: supply the operation ID and keep it before the request, so a retry can carry it")
	}
	if *opID != "" {
		if !*startOnly && *complete == "" {
			return errors.New("-operation-id applies only to -start or -complete")
		}
		if !validOperation.MatchString(*opID) {
			return fmt.Errorf("-operation-id %q is not a valid operation ID (1-128 letters, digits, '.', '_' or '-', starting with a letter or digit)", *opID)
		}
	}
	op := func(kind string) string {
		if *opID != "" {
			return *opID
		}
		return operationID(kind)
	}
	c := newClient(filepath.Join(*root, "host.sock"), *environment)
	s, err := c.session()
	if err != nil {
		return err
	}
	inc := s.Session.Incarnation
	switch {
	case *status:
		fmt.Fprintf(stdout, "authenticated: %v\n", s.Authenticated)
		return nil
	case *startOnly:
		o := op("login")
		id, url, err := c.start(inc, o)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(map[string]string{"operation_id": o, "login_id": id, "auth_url": url})
	case *complete != "":
		code, err := readCode(stdin, stderr)
		if err != nil {
			return err
		}
		return finish(c, inc, op("login-complete"), *complete, code, stdout, stderr)
	}
	id, url, err := c.start(inc, op("login"))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Open this URL, sign in, and copy the code it shows:\n\n%s\n\n", url)
	code, err := readCode(stdin, stdout)
	if err != nil {
		return err
	}
	return finish(c, inc, op("login-complete"), id, code, stdout, stderr)
}

func finish(c *client, incarnation, op, id, code string, stdout, stderr io.Writer) error {
	fmt.Fprintf(stderr, "operation: %s\n", op)
	// A retained record comes back as the error: the original completion's
	// result isn't retained, and an authenticated account doesn't prove it,
	// so it is never reported as signed in.
	ok, err := c.complete(incarnation, op, id, code)
	if err != nil {
		return err
	}
	s, err := c.session()
	if err != nil {
		return err
	}
	if !ok || !s.Authenticated {
		return errors.New("sign-in did not complete; run the command again for a fresh URL")
	}
	fmt.Fprintln(stdout, "signed in: authenticated: true")
	return nil
}
