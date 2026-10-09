// Package localclient connects only to a custody-checked local Unix socket.
package localclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// PrivateDir refuses links and requires a private directory owned by this user.
func PrivateDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return errors.New("private directory unavailable")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	resolved, e := filepath.EvalSymlinks(path)
	if !ok || e != nil || resolved != filepath.Clean(path) || !fi.IsDir() || int(st.Uid) != os.Geteuid() || fi.Mode().Perm() != 0700 {
		return errors.New("directory must be the current owner's private unlinked path")
	}
	return nil
}

// ReadPrivate reads a bounded, single-link, owner-only regular file. Errors
// never include its contents, decoder output or provider values.
func ReadPrivate(path string, limit int64) ([]byte, error) {
	if err := PrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("private input unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, errors.New("private input metadata unavailable")
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() || int(st.Uid) != os.Geteuid() || st.Nlink != 1 || fi.Mode().Perm() != 0600 || fi.Size() > limit {
		return nil, errors.New("input must be a bounded single-link regular 0600 owner file")
	}
	b := make([]byte, fi.Size()+1)
	n, err := f.ReadAt(b, 0)
	if int64(n) != fi.Size() || (err != nil && err != io.EOF) {
		return nil, errors.New("private input changed while reading")
	}
	after, e := os.Lstat(path)
	openedAfter, oe := f.Stat()
	if e != nil || oe != nil || !os.SameFile(fi, after) || !os.SameFile(fi, openedAfter) || fi.Size() != openedAfter.Size() || !fi.ModTime().Equal(openedAfter.ModTime()) || after.Mode().Perm() != 0600 || openedAfter.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, errors.New("private input replaced while reading")
	}
	return b[:n], nil
}

// Dial connects to an owner-only socket with the same checks New applies:
// the endpoint's custody before connecting, then the kernel peer UID and
// the endpoint's identity before anything is sent. A raw stream, such as a
// seat's terminal, uses it directly.
func Dial(ctx context.Context, socket string, uid int) (net.Conn, error) {
	fi, err := os.Lstat(socket)
	parent, pe := os.Lstat(filepath.Dir(socket))
	resolved, re := filepath.EvalSymlinks(filepath.Dir(socket))
	if err != nil || pe != nil || re != nil || resolved != filepath.Dir(socket) {
		return nil, errors.New("socket custody unavailable")
	}
	s, ok := fi.Sys().(*syscall.Stat_t)
	ps, pok := parent.Sys().(*syscall.Stat_t)
	if !ok || !pok || fi.Mode()&os.ModeSocket == 0 || int(s.Uid) != uid || fi.Mode().Perm()&0007 != 0 || !parent.IsDir() || int(ps.Uid) != uid || parent.Mode().Perm()&0022 != 0 {
		return nil, errors.New("socket ownership or privacy refused")
	}
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, errors.New("owner socket connection failed")
	}
	actual, err := peerUID(c)
	after, ae := os.Lstat(socket)
	if err != nil || actual != uint32(uid) || ae != nil || !os.SameFile(fi, after) {
		c.Close()
		return nil, errors.New("kernel socket peer or endpoint identity refused")
	}
	return c, nil
}

// New checks the endpoint before each connection, verifies the kernel peer UID
// before sending HTTP, and refuses redirects and proxy/environment routing.
func New(socket string, uid int) *http.Client {
	transport := &http.Transport{MaxResponseHeaderBytes: 32 << 10, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return Dial(ctx, socket, uid)
	}}
	return &http.Client{Transport: transport, Timeout: 150 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
