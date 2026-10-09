package seatterm

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"
)

// Route serves POST /v1/terminal in a seat's guest: an upgrade to
// Protocol, bound to the environment's exact session and incarnation
// (SOW 16 §3.2). The host's agent proxy carries it from the owner-only
// socket. The first frame must be the client's window size; then the fixed
// attach command runs on a terminal for the rest of the stream.
type Route struct {
	// Binding reports whether session and incarnation are this
	// environment's current ones; a stale or foreign binding is refused.
	Binding func(session, incarnation string) bool
	// Ensure makes the seat's tmux session exist, holding only the idle
	// supervisor; it never starts the actor (SOW 16 §3.1).
	Ensure func() error
	// Attach is the tmux client for the seat's session.
	Attach Command
	// slots bounds concurrent attaches.
	slots chan struct{}
}

// MaxAttaches bounds concurrent attaches to one seat.
const MaxAttaches = 4

func (rt *Route) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
	if r.Method != http.MethodPost {
		reply(405, "POST only")
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), Protocol) || !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		reply(400, "the terminal is an upgrade to "+Protocol)
		return
	}
	q := r.URL.Query()
	if rt.Binding == nil || !rt.Binding(q.Get("session_id"), q.Get("expected_incarnation")) {
		reply(409, "the terminal binds this environment's current session and incarnation")
		return
	}
	if rt.slots == nil {
		reply(500, "the terminal route isn't initialised")
		return
	}
	select {
	case rt.slots <- struct{}{}:
		defer func() { <-rt.slots }()
	default:
		reply(429, "too many terminals are attached to this seat")
		return
	}
	if rt.Ensure != nil {
		if err := rt.Ensure(); err != nil {
			reply(503, "the seat's session isn't available: "+err.Error())
			return
		}
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		reply(500, "the connection can't be upgraded")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + Protocol + "\r\nConnection: Upgrade\r\n\r\n")); err != nil {
		return
	}
	stream := &bufferedConn{Conn: conn, r: buf.Reader}
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	kind, payload, err := ReadFrame(stream)
	if err != nil || kind != Resize {
		WriteFrame(conn, Close, nil)
		return
	}
	conn.SetReadDeadline(time.Time{})
	cols := uint16(payload[0])<<8 | uint16(payload[1])
	rows := uint16(payload[2])<<8 | uint16(payload[3])
	Attach(stream, rt.Attach, cols, rows)
}

// NewRoute returns a route with its attach bound.
func NewRoute(binding func(session, incarnation string) bool, ensure func() error, attach Command) (*Route, error) {
	if binding == nil || attach.Path == "" {
		return nil, errors.New("a terminal route needs its binding and attach command")
	}
	return &Route{Binding: binding, Ensure: ensure, Attach: attach, slots: make(chan struct{}, MaxAttaches)}, nil
}

// bufferedConn reads through the bytes the HTTP server had already
// buffered past the request.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
