// pomar-agent-owner is the bounded normal-owner environment client. Private
// response bytes never go to stdout/stderr; stable operation records survive
// lost transport and allow local response recovery without repeating effects.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/zeroemployeeorg/pomar/internal/ownerclient"
)

func main() {
	var c ownerclient.Config
	flag.StringVar(&c.Root, "root", "", "private owner data root")
	flag.StringVar(&c.Records, "records", "", "private client receipt directory")
	flag.StringVar(&c.Environment, "environment", "", "original environment")
	flag.StringVar(&c.Action, "action", "", "inspect, session, operation, agent-operation, start, stop, login, login-complete")
	flag.StringVar(&c.OperationID, "operation-id", "", "explicit original call identity; never generated")
	flag.StringVar(&c.Request, "request", "", "private 0600 request file; sensitive values never in arguments")
	flag.StringVar(&c.Session, "session", "", "original session for mutations/reconciliation")
	flag.StringVar(&c.Incarnation, "incarnation", "", "original incarnation")
	flag.BoolVar(&c.Reconcile, "reconcile", false, "inspect an uncertain original operation without repeating it")
	flag.BoolVar(&c.RetryKnownUnaccepted, "retry-known-unaccepted", false, "permit same-byte same-ID retry only after fresh durable non-acceptance")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	source := "unversioned"
	dirty := false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				source = s.Value
			}
			if s.Key == "vcs.modified" && s.Value == "true" {
				dirty = true
			}
		}
	}
	if dirty {
		source += "+dirty"
	}
	digest := "unavailable"
	if path, e := os.Executable(); e == nil {
		if f, e := os.Open(path); e == nil {
			h := sha256.New()
			if _, e = io.Copy(h, f); e == nil {
				digest = hex.EncodeToString(h.Sum(nil))
			}
			f.Close()
		}
	}
	if source == "unversioned" || dirty || digest == "unavailable" {
		fmt.Fprintln(os.Stderr, "pomar-agent-owner: clean source-stamped executable required")
		os.Exit(1)
	}
	r, err := ownerclient.Run(c)
	json.NewEncoder(os.Stdout).Encode(struct {
		ownerclient.Receipt
		Source string `json:"source"`
		Binary string `json:"binary_sha256"`
		PID    int    `json:"pid"`
		PPID   int    `json:"ppid"`
	}{r, source, digest, os.Getpid(), os.Getppid()})
	if err != nil {
		fmt.Fprintln(os.Stderr, "pomar-agent-owner:", err)
		os.Exit(1)
	}
}
