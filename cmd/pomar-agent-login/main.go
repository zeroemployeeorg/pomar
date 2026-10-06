// Command pomar-agent-login signs a Claude Code agent environment in, or
// renews its sign-in, over the development host's owner-only socket:
//
//	pomar-agent-login -root DIR -environment ID
//	    prints the authorization URL, reads the code (not echoed), completes
//	    the sign-in and reports whether the environment is authenticated
//	pomar-agent-login -root DIR -environment ID -start
//	    prints {"login_id","auth_url"} as JSON and leaves the sign-in waiting
//	pomar-agent-login -root DIR -environment ID -complete LOGIN_ID
//	    reads the code from stdin and completes that waiting sign-in
//	pomar-agent-login -root DIR -environment ID -status
//	    prints whether the environment is authenticated
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

func (c *client) start(incarnation string) (id, url string, err error) {
	var out struct {
		Login struct {
			LoginID string `json:"loginId"`
			AuthURL string `json:"authUrl"`
		} `json:"login"`
	}
	if err = c.do("POST", "login", map[string]string{"expected_incarnation": incarnation, "operation_id": operationID("login")}, &out); err != nil {
		return "", "", err
	}
	if out.Login.LoginID == "" || !strings.HasPrefix(out.Login.AuthURL, "https://") {
		return "", "", errors.New("the environment returned no authorization URL (is it a Claude Code environment?)")
	}
	return out.Login.LoginID, out.Login.AuthURL, nil
}

func (c *client) complete(incarnation, id, code string) (bool, error) {
	var out struct {
		Login struct {
			Success bool `json:"success"`
		} `json:"login"`
	}
	err := c.do("POST", "login/complete", map[string]string{"expected_incarnation": incarnation, "operation_id": operationID("login-complete"), "login_id": id, "code": code}, &out)
	return out.Login.Success, err
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
	startOnly := fs.Bool("start", false, "print the login ID and authorization URL as JSON, and leave the sign-in waiting")
	complete := fs.String("complete", "", "complete the waiting sign-in with this login ID, reading the code from stdin")
	status := fs.Bool("status", false, "print whether the environment is authenticated")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *root == "" || *environment == "" {
		return errors.New("-root and -environment are required")
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
		id, url, err := c.start(inc)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(map[string]string{"login_id": id, "auth_url": url})
	case *complete != "":
		code, err := readCode(stdin, stderr)
		if err != nil {
			return err
		}
		return finish(c, inc, *complete, code, stdout)
	}
	id, url, err := c.start(inc)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Open this URL, sign in, and copy the code it shows:\n\n%s\n\n", url)
	code, err := readCode(stdin, stdout)
	if err != nil {
		return err
	}
	return finish(c, inc, id, code, stdout)
}

func finish(c *client, incarnation, id, code string, stdout io.Writer) error {
	ok, err := c.complete(incarnation, id, code)
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
