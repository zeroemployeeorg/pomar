package seatterm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Open upgrades conn to the terminal stream at target (a request path, such
// as the host's /v1/environments/ENV/agent/terminal?...). conn is already
// connected and checked; Open only speaks the upgrade.
func Open(conn net.Conn, target string) (io.ReadWriter, error) {
	if !strings.HasPrefix(target, "/") || strings.ContainsAny(target, " \r\n") {
		return nil, errors.New("invalid terminal target")
	}
	if _, err := fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: owner\r\nConnection: Upgrade\r\nUpgrade: %s\r\nContent-Length: 0\r\n\r\n", target, Protocol); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(resp.Header.Get("Upgrade"), Protocol) {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, fmt.Errorf("the terminal wasn't opened: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// Pump carries a local terminal over the stream: the window size first,
// then input as data frames and each size change as a resize, and the
// guest's screen to out, until the guest closes the stream or input ends.
func Pump(stream io.ReadWriter, in io.Reader, out io.Writer, cols, rows uint16, resizes <-chan [2]uint16) error {
	var wmu sync.Mutex
	send := func(kind byte, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return WriteFrame(stream, kind, b)
	}
	if err := send(Resize, ResizePayload(cols, rows)); err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 && send(Data, buf[:n]) != nil {
				return
			}
			if err != nil {
				send(Close, nil)
				return
			}
		}
	}()
	if resizes != nil {
		go func() {
			for size := range resizes {
				if size[0] >= 1 && size[0] <= MaxCols && size[1] >= 1 && size[1] <= MaxRows {
					if send(Resize, ResizePayload(size[0], size[1])) != nil {
						return
					}
				}
			}
		}()
	}
	for {
		kind, payload, err := ReadFrame(stream)
		if err == io.EOF || kind == Close {
			return nil
		}
		if err != nil {
			return err
		}
		if kind == Data {
			if _, err := out.Write(payload); err != nil {
				return err
			}
		}
	}
}
