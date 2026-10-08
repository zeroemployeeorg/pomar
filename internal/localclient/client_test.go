//go:build darwin

package localclient

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestNativePeerAndRedirectRefusal(t *testing.T) {
	root, err := os.MkdirTemp(os.TempDir(), "lc-")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	defer os.RemoveAll(root)
	socket := filepath.Join(root, "socket")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(socket, 0600)
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://must-not-follow.invalid/private", 302)
	})}
	go s.Serve(ln)
	defer s.Close()
	c := New(socket, os.Geteuid())
	defer c.CloseIdleConnections()
	r, err := c.Get("http://owner/")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 302 {
		t.Fatal("redirect followed")
	}
	wrong := New(socket, os.Geteuid()+1)
	defer wrong.CloseIdleConnections()
	if _, err := wrong.Get("http://owner/"); err == nil {
		t.Fatal("unexpected socket owner accepted")
	}
}

func TestPrivateFileRefusesHardLinksAndOversize(t *testing.T) {
	root, err := os.MkdirTemp(os.TempDir(), "lf-")
	if err != nil {
		t.Fatal(err)
	}
	root, _ = filepath.EvalSymlinks(root)
	defer os.RemoveAll(root)
	p := filepath.Join(root, "input")
	os.WriteFile(p, []byte("fixture"), 0600)
	if _, err := ReadPrivate(p, 3); err == nil {
		t.Fatal("oversized input accepted")
	}
	if _, err := ReadPrivate(p, 64); err != nil {
		t.Fatal(err)
	}
	os.Link(p, p+"-alias")
	if _, err := ReadPrivate(p, 64); err == nil {
		t.Fatal("hard-linked input accepted")
	}
}
