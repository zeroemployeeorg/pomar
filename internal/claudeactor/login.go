package claudeactor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
)

// Sign-in, split so a controller can do it programmatically, and repeat it
// whenever the credential needs renewing:
//
//  1. account/login/start runs `claude auth login --claudeai` as the coding
//     user. With no terminal, Claude Code prints an authorization URL and
//     waits for a code on stdin (qualified on 2.1.280). The URL is returned
//     to the caller only; the broker never journals a login result.
//  2. The person opens the URL, signs in, and gets a code.
//  3. account/login/complete writes that code to the waiting process, which
//     exchanges it itself and writes CLAUDE_CONFIG_DIR/.credentials.json.
//     The code is never journaled, logged or returned; this package never
//     reads the credential it produces.

// authURL matches the authorization URL Claude Code prints.
var authURL = regexp.MustCompile(`https://\S+/oauth/authorize\?\S+`)

// loginCode is what an authorization code may contain; anything else is
// refused before it reaches the process.
var loginCode = regexp.MustCompile(`^[A-Za-z0-9._~#-]{8,512}$`)

type loginRun struct {
	id   string
	proc Process
	done chan error
}

func (a *Actor) startLogin() (json.RawMessage, error) {
	if a.cfg.Login == nil {
		return nil, fmt.Errorf("%w: account/login/start (no login command configured)", ErrUnsupported)
	}
	a.mu.Lock()
	if a.login != nil {
		a.mu.Unlock()
		return nil, errors.New("claude actor: a sign-in is already waiting for its code")
	}
	a.mu.Unlock()
	proc, err := a.cfg.Login()
	if err != nil {
		return nil, err
	}
	found := make(chan string, 1)
	run := &loginRun{id: a.cfg.NewID(), proc: proc, done: make(chan error, 1)}
	go func() {
		// Read the URL, then drain the rest unseen until the process ends.
		s := bufio.NewScanner(proc.Stdout())
		s.Buffer(make([]byte, 4096), 1<<20)
		sent := false
		for s.Scan() {
			if u := authURL.FindString(s.Text()); u != "" && !sent {
				found <- u
				sent = true
			}
		}
		if !sent {
			close(found)
		}
		run.done <- proc.Wait()
	}()
	select {
	case u, ok := <-found:
		if !ok {
			return nil, errors.New("claude actor: sign-in ended without an authorization URL")
		}
		a.mu.Lock()
		a.login = run
		a.mu.Unlock()
		return json.Marshal(map[string]string{"type": "claudeAiOAuth", "loginId": run.id, "authUrl": u})
	case <-time.After(45 * time.Second):
		proc.Interrupt()
		return nil, errors.New("claude actor: no authorization URL within 45 s")
	}
}

func (a *Actor) completeLogin(params any) (json.RawMessage, error) {
	var p struct {
		LoginID string `json:"loginId"`
		Code    string `json:"code"`
	}
	if err := remarshal(params, &p); err != nil {
		return nil, err
	}
	a.mu.Lock()
	run := a.login
	if run == nil || run.id != p.LoginID {
		a.mu.Unlock()
		return nil, errors.New("claude actor: no waiting sign-in with that id")
	}
	a.login = nil
	a.mu.Unlock()
	finish := func(success bool) (json.RawMessage, error) {
		if success {
			a.signedInAgain()
		}
		raw, _ := json.Marshal(map[string]any{"success": success, "loginId": run.id})
		// Clears the broker's pending login either way, so it can be retried.
		a.observe(agentenv.Message{Method: "account/login/completed", Params: raw})
		return raw, nil
	}
	if !loginCode.MatchString(p.Code) {
		run.proc.Interrupt()
		return finish(false)
	}
	if _, err := run.proc.Stdin().Write([]byte(p.Code + "\n")); err != nil {
		run.proc.Interrupt()
		return finish(false)
	}
	run.proc.Stdin().Close()
	select {
	case err := <-run.done:
		return finish(err == nil && a.cfg.Authenticated != nil && a.cfg.Authenticated())
	case <-time.After(90 * time.Second):
		run.proc.Interrupt()
		return finish(false)
	}
}
