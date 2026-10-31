package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/zeroemployeeorg/pomar/internal/agentenv"
	"github.com/zeroemployeeorg/pomar/internal/localclient"
)

type actorSwitchRequest struct {
	Operation   string `json:"operation_id"`
	Session     string `json:"session_id"`
	Incarnation string `json:"expected_incarnation"`
	Profile     string `json:"target_profile"`
}

func actorSwitchCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("actor-switch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := seatHostRoot(fs)
	environment := fs.String("environment", "", "retained environment ID")
	session := fs.String("session", "", "stable seat session ID")
	incarnation := fs.String("incarnation", "", "original actor incarnation")
	operation := fs.String("id", "", "unique durable operation ID; retain it on lost replies")
	profile := fs.String("profile", "", "owner-configured target harness profile")
	status := fs.Bool("status", false, "inspect the original operation without dispatch")
	if fs.Parse(args) != nil || fs.NArg() != 0 || *root == "" || *environment == "" || *session == "" || *incarnation == "" || *operation == "" || (!*status && *profile == "") {
		return 2
	}
	c := localclient.New(filepath.Join(*root, "host.sock"), os.Geteuid())
	c.Timeout = 2 * time.Minute
	defer c.CloseIdleConnections()
	return actorSwitchCall(c, *environment, actorSwitchRequest{*operation, *session, *incarnation, *profile}, *status, stdout, stderr)
}

func actorSwitchCall(c *http.Client, environment string, body actorSwitchRequest, status bool, stdout, stderr io.Writer) int {
	method, path := "POST", "/v1/environments/"+url.PathEscape(environment)+"/replace"
	b, _ := json.Marshal(body)
	if status {
		method = "GET"
		path = "/v1/environments/" + url.PathEscape(environment) + "/operations/" + url.PathEscape(body.Operation) + "?" + url.Values{"session_id": {body.Session}, "expected_incarnation": {body.Incarnation}}.Encode()
		b = nil
	}
	req, err := http.NewRequest(method, "http://owner"+path, bytes.NewReader(b))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "switch reply unavailable; inspect the original operation with -status and the same -id, -session and -incarnation; never invent a new ID to retry")
		return 1
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 || !json.Valid(data) {
		fmt.Fprintln(stderr, "switch response incomplete; inspect the original operation")
		return 1
	}
	stdout.Write(append(data, '\n'))
	if resp.StatusCode != 200 {
		return 1
	}
	var reply struct {
		Action  agentenv.Action            `json:"action"`
		Actions map[string]agentenv.Action `json:"actions"`
	}
	if json.Unmarshal(data, &reply) != nil {
		fmt.Fprintln(stderr, "switch response has an invalid receipt shape; inspect the original operation")
		return 1
	}
	action := reply.Actions[body.Operation]
	if status {
		action = reply.Action
	}
	if action.Kind != "replace" || action.Expected != body.Incarnation || action.Switch == nil || action.Switch.SessionID != body.Session || (body.Profile != "" && action.Switch.TargetProfile != body.Profile) {
		fmt.Fprintln(stderr, "response does not bind the requested harness switch")
		return 1
	}
	if action.State != "completed" || action.Switch.Phase != "selected" {
		fmt.Fprintln(stderr, "original switch is held or incomplete; no second switch was dispatched")
		return 1
	}
	return 0
}
