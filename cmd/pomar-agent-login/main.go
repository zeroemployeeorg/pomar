// Command pomar-agent-login signs a Claude Code agent environment in, or
// renews its sign-in, over the development host's owner-only socket:
//
//	pomar-agent-login -root DIR -environment ID
//	    prints the authorization URL, reads the code (not echoed), completes
//	    the sign-in and reports whether the environment is authenticated
//	pomar-agent-login -root DIR -environment ID -start -operation-id OP -records REC
//	    starts a sign-in and prints its report as JSON, with "login_id" and
//	    "auth_url", leaving the sign-in waiting
//	pomar-agent-login -root DIR -environment ID -complete LOGIN_ID -operation-id OP -records REC
//	    completes that waiting sign-in, reading the code from stdin on the
//	    first dispatch only, and prints its report as JSON
//	pomar-agent-login -root DIR -environment ID -status
//	    prints whether the environment is authenticated
//
// -start and -complete are for automation and use the owner client's
// operation journal in REC, a private (0700) directory the caller keeps. The
// caller supplies the operation ID, in the broker's syntax, and keeps it
// before the request; the request is sealed before dispatch, and every
// dispatch, response and reconciliation is retained as its own event, never
// overwritten. Rerunning with the same ID reads the retained reply back
// (reported as cached) and never signs in again; -reconcile inspects an
// uncertain original afresh, and -retry-known-unaccepted resends the same
// request only when that inspection proves it was never accepted.
//
// The JSON report keeps the dimensions apart: transport_outcome,
// operation_state, response_available and retained_response_available,
// session_state (the environment now), and succeeded. succeeded is true only
// for a durably retained reply: an authorization URL, or a completion whose
// own success is true. The broker's retained control records that a request
// was sent, not its result, and an authenticated account doesn't prove this
// operation's result, so neither ever counts as success. A one-time reply
// that was never retained is never recreated. The exit status is 0 only when
// succeeded is true (and, for -complete, the environment is authenticated).
//
// The code goes only to the environment's actor: it is never printed or
// journaled. Until the reply is durably retained it is kept in REC/.requests,
// owner-only, for a same-byte retry, and removed once it is. No credential
// ever leaves the environment. The interactive form makes its own operation
// IDs, and never recovers an existing operation.
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

	"github.com/zeroemployeeorg/pomar/internal/calljournal"
	"github.com/zeroemployeeorg/pomar/internal/localclient"
	"github.com/zeroemployeeorg/pomar/internal/ownerclient"
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
		ID          string `json:"session_id"`
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
	startOnly := fs.Bool("start", false, "start a sign-in and print its result as JSON, with the login ID and authorization URL")
	complete := fs.String("complete", "", "complete the waiting sign-in with this login ID, reading the code from stdin on the first dispatch")
	status := fs.Bool("status", false, "print whether the environment is authenticated")
	opID := fs.String("operation-id", "", "required with -start or -complete: the request's operation ID, supplied and kept by the caller and reused on retries")
	records := fs.String("records", "", "required with -start or -complete: the caller's private (0700) operation records directory")
	reconcile := fs.Bool("reconcile", false, "with -start or -complete: inspect an uncertain original operation afresh, without repeating it")
	retry := fs.Bool("retry-known-unaccepted", false, "with -reconcile: resend the same original request only if the fresh inspection proves it was never accepted")
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
	scriptedMode := *startOnly || *complete != ""
	if scriptedMode && (*opID == "" || *records == "") {
		return errors.New("-start and -complete require -operation-id and -records: supply the operation ID and keep it before the request, so a retry can carry it")
	}
	if !scriptedMode && (*opID != "" || *records != "" || *reconcile || *retry) {
		return errors.New("-operation-id, -records, -reconcile and -retry-known-unaccepted apply only to -start or -complete")
	}
	if *retry && !*reconcile {
		return errors.New("-retry-known-unaccepted requires -reconcile")
	}
	if *opID != "" && !validOperation.MatchString(*opID) {
		return fmt.Errorf("-operation-id %q is not a valid operation ID (1-128 letters, digits, '.', '_' or '-', starting with a letter or digit)", *opID)
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
	case scriptedMode:
		return scripted(c, s, ownerclient.Config{
			Root: *root, Records: *records, Environment: *environment, OperationID: *opID,
			Session: s.Session.ID, Incarnation: inc, Reconcile: *reconcile, RetryKnownUnaccepted: *retry,
		}, *complete, stdin, stdout, stderr)
	}
	id, url, err := c.start(inc, operationID("login"))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Open this URL, sign in, and copy the code it shows:\n\n%s\n\n", url)
	code, err := readCode(stdin, stdout)
	if err != nil {
		return err
	}
	return finish(c, inc, operationID("login-complete"), id, code, stdout, stderr)
}

func finish(c *client, incarnation, op, id, code string, stdout, stderr io.Writer) error {
	fmt.Fprintf(stderr, "operation: %s\n", op)
	// A retained broker record comes back as the error: it keeps that the
	// completion was sent, not its result, and an authenticated account doesn't
	// prove it, so it is never reported as signed in.
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

// result is the scripted modes' machine-readable report: the owner client's
// separate dimensions (transport, original operation, retained response,
// current session), and what the retained response says.
type result struct {
	ownerclient.Receipt
	Succeeded bool   `json:"succeeded"`
	LoginID   string `json:"login_id,omitempty"`
	AuthURL   string `json:"auth_url,omitempty"`
}

// scripted runs -start or -complete through the owner client's operation
// journal: the request is sealed before dispatch, every dispatch, response
// and reconciliation is retained as its own event, and a one-time reply is
// read back from the journal, never recreated. Success needs that retained
// reply: an authorization URL, or a completion whose own success is true;
// the current session is reported separately and never stands in for it.
func scripted(c *client, s session, cfg ownerclient.Config, loginID string, stdin *os.File, stdout, stderr io.Writer) error {
	cfg.Action = "login"
	if loginID != "" {
		cfg.Action = "login-complete"
	}
	request, err := sealRequest(cfg, loginID, stdin, stderr)
	if err != nil {
		return err
	}
	cfg.Request = request
	cfg.ConsumeRequest = cfg.Action == "login-complete"
	r, runErr := ownerclient.Run(cfg)
	out := result{Receipt: r}
	if now, e := c.session(); e != nil {
		out.SessionState = "unavailable"
	} else if now.Authenticated {
		out.SessionState = "authenticated"
	} else {
		out.SessionState = "not-authenticated"
	}
	if (runErr == nil || errors.Is(runErr, ownerclient.ErrUncertain)) && (r.ResponseAvailable || r.RetainedResponseAvailable) {
		if code, body, e := ownerclient.RetainedResponse(cfg.Records, cfg.OperationID); e == nil && code >= 200 && code < 300 {
			var reply struct {
				Login struct {
					LoginID string `json:"loginId"`
					AuthURL string `json:"authUrl"`
					Success *bool  `json:"success"`
				} `json:"login"`
			}
			json.Unmarshal(body, &reply)
			if cfg.Action == "login" && reply.Login.LoginID != "" && strings.HasPrefix(reply.Login.AuthURL, "https://") {
				out.Succeeded, out.LoginID, out.AuthURL = true, reply.Login.LoginID, reply.Login.AuthURL
			}
			if cfg.Action == "login-complete" && reply.Login.Success != nil && *reply.Login.Success {
				out.Succeeded = true
			}
		}
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return err
	}
	switch {
	case !out.Succeeded && runErr != nil:
		return runErr
	case !out.Succeeded:
		return errors.New("no retained reply establishes this operation's result")
	case cfg.Action == "login-complete" && out.SessionState != "authenticated":
		return errors.New("the completion's retained reply succeeded, but the environment is not authenticated now")
	}
	return nil
}

// sealRequest returns the operation's private request file, writing it once:
// records/.requests/OP.json, owner-only. A completion's code is read from
// stdin only for its first dispatch, and the owner client removes the file
// once the reply is durably retained. An existing request must be for the
// same login; once it is consumed, a retry carries no request and the owner
// client answers from the sealed intent and its journal.
func sealRequest(cfg ownerclient.Config, loginID string, stdin *os.File, stderr io.Writer) (string, error) {
	if err := localclient.PrivateDir(cfg.Records); err != nil {
		return "", err
	}
	dir := filepath.Join(cfg.Records, ".requests")
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	if err := localclient.PrivateDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, cfg.OperationID+".json")
	if b, err := localclient.ReadPrivate(path, 64<<10); err == nil {
		var prior struct {
			LoginID string `json:"login_id"`
		}
		if json.Unmarshal(b, &prior) != nil || prior.LoginID != loginID {
			return "", ownerclient.ErrConflict
		}
		return path, nil
	} else if _, e := os.Lstat(path); !os.IsNotExist(e) {
		return "", errors.New("private request unreadable; preserve and inspect")
	}
	if cfg.Action == "login-complete" {
		if _, err := os.Lstat(filepath.Join(cfg.Records, cfg.OperationID, "intent.json")); err == nil {
			return "", nil
		}
	}
	req := map[string]string{"operation_id": cfg.OperationID, "expected_incarnation": cfg.Incarnation}
	if cfg.Action == "login-complete" {
		code, err := readCode(stdin, stderr)
		if err != nil {
			return "", err
		}
		req["login_id"], req["code"] = loginID, code
	}
	if err := calljournal.Save(path, req); err != nil {
		return "", err
	}
	return path, nil
}
