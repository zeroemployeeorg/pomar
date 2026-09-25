// Command pomar-shim runs inside a guest. It listens on one loopback port and
// forwards each connection, unchanged, to one Unix socket that the helper
// relays from the host over vsock. Tools in the guest reach the host's Go
// module proxy at http://127.0.0.1:<port> with no network interface.
//
// It is built static for linux/arm64 and copied into the guest like the
// source. It refuses to listen on anything but a loopback address.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7070", "loopback address to listen on")
	socket := flag.String("socket", "/run/pomar/goproxy.sock", "Unix socket to forward to")
	ready := flag.String("ready", "", "file to create once listening")
	flag.Parse()
	if err := run(*listen, *socket, *ready); err != nil {
		fmt.Fprintln(os.Stderr, "pomar-shim:", err)
		os.Exit(1)
	}
}

// checkLoopback refuses any listen address that is not a loopback IP.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address %q is not a loopback IP", addr)
	}
	return nil
}

func run(listen, socket, ready string) error {
	if err := checkLoopback(listen); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	if ready != "" {
		if err := os.WriteFile(ready, nil, 0o644); err != nil {
			return err
		}
	}
	return serve(ln, socket)
}

func serve(ln net.Listener, socket string) error {
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		go forward(c, socket)
	}
}

func forward(c net.Conn, socket string) {
	defer c.Close()
	u, err := net.Dial("unix", socket)
	if err != nil {
		return
	}
	defer u.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(u, c); closeWrite(u) }()
	go func() { defer wg.Done(); io.Copy(c, u); closeWrite(c) }()
	wg.Wait()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}
