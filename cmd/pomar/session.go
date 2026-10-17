package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/zeroemployeeorg/pomar/internal/seatterm"
	"github.com/zeroemployeeorg/pomar/internal/sessiond"
)

// `pomar session`: named programs on terminals that outlive every client,
// served by this user's session service (docs/sessions.md). Everything but
// attach prints JSON, and every refusal has a fixed exit code, so another
// program can drive a session as well as a person can.

// Exit codes of `pomar session`, beyond 0, 1 and 2.
const (
	exitNoService = 3  // no session service runs for the root: start one with `pomar session serve`
	exitNotFound  = 4  // no session has that name
	exitConflict  = 5  // refused as it stands: the name runs, another client controls it, or the operation is still running
	exitExited    = 6  // the session's program has exited
	exitStale     = 7  // the send named a generation or grant epoch the session no longer has: read it again
	exitUnsupp    = 8  // the session can't do it as asked: a when-ready send to a session without a state channel
	exitNotReady  = 9  // a when-ready send's session didn't report itself waiting for input in time: nothing was written
	exitHeld      = 10 // the session has an operation whose outcome is unknown: pass -ack-unknown to send anyway
	exitTooLarge  = 11 // a send or paste is over its own bound: nothing was queued or written
	exitUncertain = 12 // a stop could not confirm the leader or its group gone: it may still be running
)

// maxPasteBytes bounds one paste's payload read client-side, before it's
// even sent: the server enforces its own, identical bound (sessiond's
// maxPasteBytes) regardless, since a non-CLI caller never reads this one.
const maxPasteBytes = 8 << 20

const sessionUsage = `usage:
  pomar session serve [-root DIR]   run this user's session service (in the foreground)
  pomar session new -id OP [-kind K] [-cols C -rows R] [-env K=V]... [-dir D] [-state] [-expected-sha256 HEX] [-preserves-install-path] NAME -- /ABS/PROGRAM ARGS...
                                    -state: a state channel for an adapter ($POMAR_SESSION_STATE in its environment)
                                    -expected-sha256: refuse the launch unless the program hashes to this
                                    -preserves-install-path: the program resolves resources relative to its installed
                                    location; refuses -expected-sha256 outright rather than run a relocated copy
  pomar session ls [-json]
  pomar session show NAME           the session's state, as JSON
  pomar session screen [-json] NAME the current screen, as text
  pomar session scrollback [-json] [-lines N] NAME
                                    the lines scrolled off the screen, oldest first, readable without attaching
  pomar session health NAME         a passive, heuristic read of the screen and last 200 scrollback lines:
                                    possible_panic, possible_readonly_refusal, possible_approval_prompt,
                                    last_output, pattern_version
  pomar session logs [-from N] [-limit N] [-o PATH] NAME
                                    the session's durable log, paged; written to PATH, or stdout
  pomar session send -id OP [-enter] [-take] [-session ID] [-epoch N] [-when-ready [-ready-wait D]] [-accept-wait D] [-ack-unknown] NAME [--] TEXT
                                    -session/-epoch: refused as stale if the session changed since you read it
                                    -when-ready: written once the session's adapter reports it waiting for input
                                    refused (exit 10) if an operation's outcome is unknown, unless -ack-unknown
                                    text over 64 KiB is refused outright (exit 11), never truncated; use paste for bulk content
  pomar session input NAME INPUT_ID  a send's current acceptance, read live: yes, pending, unsupported or unknown
                                    distinct from a retried send's own frozen reply; never re-sends anything
  pomar session paste -id OP [-file PATH] [-take] [-raw] [-session ID] [-epoch N] [-when-ready [-ready-wait D]] [-accept-wait D] [-ack-unknown] NAME
                                    sends a file's (or stdin's) whole content as one write, bracketed as a paste unless -raw
                                    same held-operation refusal as send
  pomar session attach [-view] [-take] [-title=false] NAME
                                    the session's terminal; Ctrl-] detaches, and the program keeps running
  pomar session stop -id OP NAME    the program's leader and process group are read back
Every command takes -root DIR; the root defaults to $POMAR_SESSION_ROOT, or ~/.local/state/pomar/sessions.
`

func sessionCmd(args []string, stdin *os.File, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, sessionUsage)
		return 2
	}
	verb, args := args[0], args[1:]
	fs := flag.NewFlagSet("session "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "", "the session service's root (default $POMAR_SESSION_ROOT, or ~/.local/state/pomar/sessions)")
	op := fs.String("id", "", "the operation's ID: a retry with the same ID returns what it did, and never acts twice")
	// Everything after the first -- is never a flag: new's program, or
	// send's text, which may begin with a dash.
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var (
		asJSON    = fs.Bool("json", false, "print JSON")
		kind      = fs.String("kind", "program", "the session's kind: a lower-case word (agent, shell, ...)")
		cols      = fs.Uint("cols", 0, "the window's columns (default: this terminal's, or 80)")
		rows      = fs.Uint("rows", 0, "the window's rows (default: this terminal's, or 24)")
		dir       = fs.String("dir", "", "the program's working directory")
		enter     = fs.Bool("enter", false, "end the text with Enter (a carriage return)")
		take      = fs.Bool("take", false, "take control from whoever holds it")
		view      = fs.Bool("view", false, "view only: nothing you type is sent")
		state     = fs.Bool("state", false, "give the session a state channel, on which an adapter reports its agent state")
		expectSHA = fs.String("expected-sha256", "", "new: refuse the launch unless the program's file hashes to this")
		preserves = fs.Bool("preserves-install-path", false, "new: the program resolves resources relative to its installed location; refuses -expected-sha256 outright rather than run a relocated copy")
		gen       = fs.String("session", "", "the session ID (generation) you last read: refused as stale if the name now has another")
		epoch     = fs.Int64("epoch", -1, "the grant epoch you last read: refused as stale if control changed since")
		ready     = fs.Bool("when-ready", false, "write the text only once the session's adapter reports it waiting for input")
		rwait     = fs.Duration("ready-wait", 0, "how long -when-ready waits (default 30s, at most 10m)")
		await     = fs.Duration("accept-wait", 0, "how long to wait, after the write, for the adapter to report the input accepted")
		ackUnk    = fs.Bool("ack-unknown", false, "send, paste: proceed despite a held (acceptance_unknown) operation; the hold itself is not cleared")
		title     = fs.Bool("title", true, "show your role and the agent state in the terminal's title while attached")
		from      = fs.Int64("from", 0, "logs: the byte offset to start from")
		limit     = fs.Int64("limit", 0, "logs: at most this many bytes (default and max 1 MiB)")
		out       = fs.String("o", "", "logs: write to this path instead of stdout")
		file      = fs.String("file", "", "paste: read the payload from this file, instead of stdin")
		raw       = fs.Bool("raw", false, "paste: send the payload as is, without bracketed-paste markers")
		lines     = fs.Int("lines", 0, "scrollback: at most this many of the most recent lines (default: all kept, up to HistoryLines)")
		envs      multiFlag
	)
	fs.Var(&envs, "env", "K=V for the program's environment (repeatable); TERM, HOME, PATH and LANG come from yours unless set")
	pos, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if verb != "new" {
		pos, tail = append(pos, tail...), nil
	}
	if *root == "" {
		if *root, err = sessiond.DefaultRoot(); err != nil {
			fmt.Fprintln(stderr, "pomar session:", err)
			return 2
		}
	}
	need := func(n int, what string) bool {
		if len(pos) != n {
			fmt.Fprintf(stderr, "pomar session %s: %s\n%s", verb, what, sessionUsage)
			return false
		}
		return true
	}
	needOp := func() bool {
		if *op == "" {
			fmt.Fprintf(stderr, "pomar session %s: -id names the operation, so a retry never acts twice\n", verb)
			return false
		}
		return true
	}
	c := sessiond.NewClient(*root)
	printJSON := func(v any) {
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Fprintln(stdout, string(b))
	}
	call := func(method, path string, body, out any) int {
		return sessionExit(verb, *op, c.Do(method, path, body, out), stderr)
	}
	// callOp is call for a send/paste/stop: out is a sessiond.OpResult, and
	// a refusal that still carries one (a same-ID retry's 409 replay of an
	// original partial or uncertain result, not a hard refusal with no
	// result at all) is printed before the exit code is returned, not
	// silently dropped -- the queued/written/epoch fields are exactly what
	// an operator needs to see on that outcome, same as a clean 200 prints
	// them.
	callOp := func(method, path string, body any, out *sessiond.OpResult) int {
		rc := sessionExit(verb, *op, c.Do(method, path, body, out), stderr)
		if rc != 0 && out.Outcome != "" {
			printJSON(*out)
		}
		return rc
	}
	switch verb {
	case "serve":
		if !need(0, "serve takes no arguments") {
			return 2
		}
		return sessionServe(*root, stdout, stderr)
	case "new":
		if !need(1, "new names its session") || !needOp() {
			return 2
		}
		if len(tail) == 0 {
			fmt.Fprintln(stderr, "pomar session new: the program follows --, by its absolute path")
			return 2
		}
		w, h := uint16(80), uint16(24)
		if onTerminal(stdin) {
			if tc, tr, err := ttySize(stdin); err == nil {
				w, h = tc, tr
			}
		}
		if *cols > 0 {
			w = uint16(min(*cols, seatterm.MaxCols))
		}
		if *rows > 0 {
			h = uint16(min(*rows, seatterm.MaxRows))
		}
		req := sessiond.CreateRequest{Operation: *op, Name: pos[0], Kind: *kind, Path: tail[0], Args: tail[1:], Env: sessionEnv(envs), Dir: *dir, Cols: w, Rows: h, StateChannel: *state, ExpectedSHA256: *expectSHA, PreservesInstallPath: *preserves}
		var out sessiond.Created
		if rc := call("POST", "/v1/sessions", req, &out); rc != 0 {
			return rc
		}
		printJSON(out)
	case "ls":
		if !need(0, "ls takes no arguments") {
			return 2
		}
		var l sessiond.Listing
		if rc := call("GET", "/v1/sessions", nil, &l); rc != 0 {
			return rc
		}
		if *asJSON {
			printJSON(l)
			return 0
		}
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tGEN\tKIND\tSTATE\tAGENT\tSIZE\tCONTROLLER\tEPOCH\tVIEWERS\tLAST OUTPUT")
		for _, s := range l.Sessions {
			state := "running"
			if !s.Running && s.ExitCode != nil {
				state = fmt.Sprintf("exited %d", *s.ExitCode)
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%dx%d\t%s\t%d\t%d\t%s\n", s.Name, s.Generation, s.Kind, state, s.Agent.State, s.Cols, s.Rows, dash(s.Controller), s.Epoch, len(s.Viewers), dash(s.LastOutput))
		}
		tw.Flush()
		for _, u := range l.Unrecovered {
			fmt.Fprintf(stdout, "%s (%s): unrecovered, held by an earlier service and out of this one's reach\n", dash(u.Name), u.ID)
		}
	case "show":
		if !need(1, "show names its session") {
			return 2
		}
		var info json.RawMessage
		if rc := call("GET", "/v1/sessions/"+url.PathEscape(pos[0]), nil, &info); rc != 0 {
			return rc
		}
		printJSON(info)
	case "screen":
		if !need(1, "screen names its session") {
			return 2
		}
		var scr seatterm.ScreenText
		if rc := call("GET", "/v1/sessions/"+url.PathEscape(pos[0])+"/screen", nil, &scr); rc != 0 {
			return rc
		}
		if *asJSON {
			printJSON(scr)
			return 0
		}
		lines := scr.Lines
		for len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1] // the blank rows below the last text
		}
		fmt.Fprintln(stdout, strings.Join(lines, "\n"))
	case "scrollback":
		if !need(1, "scrollback names its session") {
			return 2
		}
		path := "/v1/sessions/" + url.PathEscape(pos[0]) + "/scrollback"
		if *lines > 0 {
			path += "?lines=" + fmt.Sprint(*lines)
		}
		var sb seatterm.ScrollbackText
		if rc := call("GET", path, nil, &sb); rc != 0 {
			return rc
		}
		if *asJSON {
			printJSON(sb)
			return 0
		}
		fmt.Fprintln(stdout, strings.Join(sb.Lines, "\n"))
		if sb.Truncated {
			fmt.Fprintf(stderr, "pomar session scrollback: more history is kept than shown; raise -lines for it\n")
		}
	case "health":
		if !need(1, "health names its session") {
			return 2
		}
		var h seatterm.Health
		if rc := call("GET", "/v1/sessions/"+url.PathEscape(pos[0])+"/health", nil, &h); rc != 0 {
			return rc
		}
		printJSON(h)
	case "logs":
		if !need(1, "logs names its session") {
			return 2
		}
		q := url.Values{}
		if *from > 0 {
			q.Set("from", fmt.Sprintf("%d", *from))
		}
		if *limit > 0 {
			q.Set("limit", fmt.Sprintf("%d", *limit))
		}
		path := "/v1/sessions/" + url.PathEscape(pos[0]) + "/logs"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		chunk, err := c.RawGet(path)
		if rc := sessionExit(verb, "", err, stderr); rc != 0 {
			return rc
		}
		w := io.Writer(stdout)
		if *out != "" {
			f, err := os.Create(*out)
			if err != nil {
				fmt.Fprintln(stderr, "pomar session logs:", err)
				return 1
			}
			defer f.Close()
			w = f
		}
		w.Write(chunk.Data)
		if chunk.Truncated {
			fmt.Fprintf(stderr, "pomar session logs: %d of %d bytes from offset %d; more follows, -from %d for the rest\n",
				len(chunk.Data), chunk.Size, chunk.From, chunk.From+int64(len(chunk.Data)))
		}
	case "send":
		if !need(2, "send names its session and its text") || !needOp() {
			return 2
		}
		text := pos[1]
		if *enter {
			text += "\r"
		}
		req := sessiond.SendRequest{Operation: *op, Text: text, Take: *take, Session: *gen, WhenReady: *ready,
			ReadyWaitMS: int(rwait.Milliseconds()), AcceptWaitMS: int(await.Milliseconds()), AckUnknown: *ackUnk}
		if *epoch >= 0 {
			e := uint64(*epoch)
			req.Epoch = &e
		}
		var out sessiond.OpResult
		if rc := callOp("POST", "/v1/sessions/"+url.PathEscape(pos[0])+"/send", req, &out); rc != 0 {
			return rc
		}
		printJSON(out)
	case "input":
		if !need(2, "input names its session and the input ID to ask about") {
			return 2
		}
		var st sessiond.InputStatusResult
		if rc := call("GET", "/v1/sessions/"+url.PathEscape(pos[0])+"/input?id="+url.QueryEscape(pos[1]), nil, &st); rc != 0 {
			return rc
		}
		printJSON(st)
	case "paste":
		if !need(1, "paste names its session") || !needOp() {
			return 2
		}
		var src io.Reader = stdin
		if *file != "" {
			f, err := os.Open(*file)
			if err != nil {
				fmt.Fprintln(stderr, "pomar session paste:", err)
				return 1
			}
			defer f.Close()
			src = f
		}
		payload, err := io.ReadAll(io.LimitReader(src, maxPasteBytes+1))
		if err != nil {
			fmt.Fprintln(stderr, "pomar session paste:", err)
			return 1
		}
		if len(payload) > maxPasteBytes {
			fmt.Fprintf(stderr, "pomar session paste: the payload is over the %d-byte bound; nothing was sent\n", maxPasteBytes)
			return 2
		}
		// The payload goes to the server exactly as read, never wrapped or
		// scanned here: the server bounds it on its own (maxPasteBytes, not
		// an ordinary send's bound) and, unless -raw, refuses any ESC byte
		// in it before bracketing it, so a caller can't construct a request
		// that bypasses that by wrapping it first (zeocouncil's and the
		// Architect's BLOCKER on #128's stack).
		req := sessiond.SendRequest{Operation: *op, Text: string(payload), Take: *take, Session: *gen, WhenReady: *ready,
			ReadyWaitMS: int(rwait.Milliseconds()), AcceptWaitMS: int(await.Milliseconds()), Paste: true, Raw: *raw, AckUnknown: *ackUnk}
		if *epoch >= 0 {
			e := uint64(*epoch)
			req.Epoch = &e
		}
		var out sessiond.OpResult
		if rc := callOp("POST", "/v1/sessions/"+url.PathEscape(pos[0])+"/send", req, &out); rc != 0 {
			return rc
		}
		printJSON(out)
	case "stop":
		if !need(1, "stop names its session") || !needOp() {
			return 2
		}
		var out sessiond.OpResult
		if rc := callOp("POST", "/v1/sessions/"+url.PathEscape(pos[0])+"/stop", sessiond.StopRequest{Operation: *op}, &out); rc != 0 {
			return rc
		}
		printJSON(out)
	case "attach":
		if !need(1, "attach names its session") {
			return 2
		}
		return sessionAttach(c, pos[0], *view, *take, *title, stdin, stdout, stderr)
	default:
		fmt.Fprint(stderr, sessionUsage)
		return 2
	}
	return 0
}

// sessionExit maps a call's error to the fixed exit codes, and says what to
// do next.
func sessionExit(verb, op string, err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "pomar session %s: %v\n", verb, err)
	var r *sessiond.Refusal
	switch {
	case errors.Is(err, sessiond.ErrNoService):
		fmt.Fprintln(stderr, "No session service answers for this root. Its owner starts one with `pomar session serve`; see docs/sessions.md.")
		return exitNoService
	case errors.Is(err, sessiond.ErrNoReply):
		if op != "" {
			fmt.Fprintf(stderr, "Ask again with the same -id %s: it returns what the operation did, and never acts twice.\n", op)
		}
		return 1
	case errors.As(err, &r):
		switch r.Kind {
		case sessiond.KindUsage:
			return 2
		case sessiond.KindNotFound:
			return exitNotFound
		case sessiond.KindConflict, sessiond.KindInProgress:
			return exitConflict
		case sessiond.KindExited:
			return exitExited
		case sessiond.KindStale:
			fmt.Fprintln(stderr, "Read the session again (`pomar session show`), then decide; nothing was written.")
			return exitStale
		case sessiond.KindUnsupported:
			return exitUnsupp
		case sessiond.KindNotReady:
			return exitNotReady
		case sessiond.KindTooLarge:
			return exitTooLarge
		case sessiond.KindUncertain:
			fmt.Fprintln(stderr, "The program's exit could not be confirmed; it may still be running. Check `pomar session show`.")
			return exitUncertain
		case sessiond.KindHeld:
			fmt.Fprintln(stderr, "Pass -ack-unknown to send anyway; the hold itself is not cleared by that.")
			return exitHeld
		}
	}
	return 1
}

func sessionServe(root string, stdout, stderr io.Writer) int {
	s, err := sessiond.Open(root)
	if err != nil {
		fmt.Fprintln(stderr, "pomar session serve:", err)
		if errors.Is(err, sessiond.ErrServing) {
			return exitConflict
		}
		return 1
	}
	defer s.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	b, _ := json.Marshal(map[string]string{"socket": s.Socket(), "root": root, "state": "listening",
		"survives": "clients and dropped connections; not a restart of this service, and not a reboot"})
	ready := func() { fmt.Fprintln(stdout, string(b)) }
	if err := s.Serve(ctx, ready); err != nil {
		fmt.Fprintln(stderr, "pomar session serve:", err)
		return 1
	}
	return 0
}

func sessionAttach(c *sessiond.Client, name string, view, take, title bool, stdin *os.File, stdout, stderr io.Writer) int {
	// Held operations (an adapter's effect whose outcome was never
	// confirmed) are shown before anything is typed, so "continue" isn't
	// the reflex. A failed read here never blocks the attach itself.
	var v sessiond.SessionView
	if c.Do("GET", "/v1/sessions/"+url.PathEscape(name), nil, &v) == nil && len(v.HeldOperations) > 0 {
		oldest := v.HeldOperations[0].AgeSecs
		for _, h := range v.HeldOperations {
			if h.AgeSecs > oldest {
				oldest = h.AgeSecs
			}
		}
		fmt.Fprintf(stderr, "pomar session attach: %d operation(s) have an unknown outcome, the oldest for %ds — check before continuing (pomar session show %s)\n",
			len(v.HeldOperations), oldest, name)
	}
	st, err := c.Attach(name, view, take)
	if err != nil {
		return sessionExit("attach", "", err, stderr)
	}
	defer st.Close()
	cols, rows := uint16(80), uint16(24)
	var resizes chan [2]uint16
	tty := onTerminal(stdin)
	if tty {
		if tc, tr, err := ttySize(stdin); err == nil {
			cols, rows = tc, tr
		}
		restore, err := rawMode(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "pomar session attach:", err)
			return 1
		}
		defer restore()
		resizes = make(chan [2]uint16, 4)
		winch := make(chan os.Signal, 4)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				if tc, tr, err := ttySize(stdin); err == nil {
					resizes <- [2]uint16{tc, tr}
				}
			}
		}()
	}
	// The status, your role and the agent state, is shown live without
	// touching the program's screen: on stderr when it isn't this terminal,
	// else in the terminal's title, written only between the program's
	// sequences and characters and restored on detach. Notices go to stderr
	// live in the first case, and after detach otherwise.
	errFile, _ := stderr.(*os.File)
	shared := errFile != nil && onTerminal(errFile)
	st2 := &attachStatus{name: name, agent: "unknown"}
	var notices []string
	h := sessiond.Hooks{}
	switch {
	case !shared:
		h.Notice = func(n string) { fmt.Fprintf(stderr, "pomar session: %s\n", n) }
		st2.show = func(s string) { fmt.Fprintf(stderr, "pomar session: %s\n", s) }
	case tty && title:
		h.Notice = func(n string) { notices = append(notices, n) }
		fmt.Fprint(stdout, "\x1b[22;0t") // keep the terminal's own title, to restore
		defer fmt.Fprint(stdout, "\x1b[23;0t")
		safe := true
		h.Safe = func(ok bool) {
			if safe = ok; ok {
				st2.flush(stdout)
			}
		}
		st2.show = func(string) {
			if safe {
				st2.flush(stdout)
			}
		}
	default:
		h.Notice = func(n string) { notices = append(notices, n) }
	}
	h.Presence = st2.presence
	h.State = st2.state
	h.Control = st2.control
	var in io.Reader = stdin
	if !view {
		// An attached operator must never stay locked out by whoever took
		// control: this key asks for it back without a detach/reattach
		// round trip. A client that asked to only view never gets it.
		req := make(chan seatterm.ControlMsg, 1)
		in = &takeKeyReader{r: stdin, req: req}
		h.Requests = req
	}
	detached, err := sessiond.PumpWith(st, in, stdout, cols, rows, resizes, h)
	if tty {
		fmt.Fprint(stdout, "\x1b[0m\x1b[?25h\r\n")
	}
	for _, n := range notices {
		fmt.Fprintf(stderr, "pomar session: %s\r\n", n)
	}
	if err != nil {
		fmt.Fprintln(stderr, "pomar session attach:", err)
		return 1
	}
	if detached {
		fmt.Fprintf(stderr, "Detached from %s; it keeps running.\r\n", name)
	}
	return 0
}

// takeControlKey asks for control, from whoever holds it, without
// detaching: Ctrl-^, never sent to the program.
const takeControlKey = 0x1e

// takeKeyReader strips takeControlKey out of an attached operator's input
// and turns each one into a control request on req, so taking control back
// never needs a detach and a reattach with -take. Everything else passes
// through unchanged. req is sent to non-blocking: a key held or repeated
// while a request is already queued asks for nothing more.
type takeKeyReader struct {
	r   io.Reader
	req chan<- seatterm.ControlMsg
}

func (t *takeKeyReader) Read(p []byte) (int, error) {
	for {
		n, err := t.r.Read(p)
		out := 0
		for i := 0; i < n; i++ {
			if p[i] == takeControlKey {
				select {
				case t.req <- seatterm.ControlMsg{Op: "take"}:
				default:
				}
				continue
			}
			p[out] = p[i]
			out++
		}
		if out > 0 || err != nil {
			return out, err
		}
		// The whole read was take-key presses: read again rather than
		// report a zero-byte, nil-error read.
	}
}

// attachStatus is an attached client's live status line. Its hooks run on
// the pump's one goroutine, so it needs no lock.
type attachStatus struct {
	name, role, agent string
	clients           int
	epoch             uint64
	pending           string // a title not yet written
	show              func(string)
}

func (a *attachStatus) presence(p seatterm.PresenceMsg) {
	a.role, a.clients, a.epoch = p.You, len(p.Viewers), p.Epoch
	a.changed()
}

func (a *attachStatus) state(s seatterm.AgentState) {
	a.agent = s.State
	a.changed()
}

func (a *attachStatus) control(m seatterm.ControlMsg) {
	if m.Event == "taken" || m.Event == "refused" {
		a.role = "viewer"
	}
	a.changed()
}

func (a *attachStatus) line() string {
	return fmt.Sprintf("%s: %s (epoch %d), %d attached, agent %s", a.name, a.role, a.epoch, a.clients, a.agent)
}

func (a *attachStatus) changed() {
	if a.role == "" {
		return // presence comes first: nothing to say before it
	}
	a.pending = a.line()
	if a.show != nil {
		a.show(a.pending)
	}
}

// flush writes a pending title. A title is printable text only, so nothing
// of it can end its own sequence early.
func (a *attachStatus) flush(out io.Writer) {
	if a.pending == "" {
		return
	}
	t := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, "pomar "+a.pending)
	fmt.Fprintf(out, "\x1b]2;%s\x07", t)
	a.pending = ""
}

// sessionEnv is a new session's whole environment: TERM, HOME, PATH and
// LANG from the caller's, then each -env, which wins.
func sessionEnv(set []string) []string {
	env := map[string]string{"TERM": "xterm-256color"}
	order := []string{"TERM"}
	for _, k := range []string{"TERM", "HOME", "PATH", "LANG"} {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			if _, seen := env[k]; !seen {
				order = append(order, k)
			}
			env[k] = v
		}
	}
	for _, kv := range set {
		k, v, _ := strings.Cut(kv, "=")
		if _, seen := env[k]; !seen {
			order = append(order, k)
		}
		env[k] = v
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+env[k])
	}
	return out
}

// parseMixed parses flags wherever they come among the positional
// arguments.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	if !strings.Contains(v, "=") || strings.HasPrefix(v, "=") {
		return errors.New("want K=V")
	}
	*m = append(*m, v)
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
